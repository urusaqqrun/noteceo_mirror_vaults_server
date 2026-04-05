package database

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	vaultsync "github.com/urusaqqrun/vault-mirror-service/sync"
)

func (s *PgStore) ensureMirrorSchema(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS mirror_sync_cursor (
			owner_user_id TEXT PRIMARY KEY,
			last_seq BIGINT NOT NULL DEFAULT 0,
			lease_owner TEXT NOT NULL DEFAULT '',
			lease_until BIGINT NOT NULL DEFAULT 0,
			last_error TEXT NOT NULL DEFAULT '',
			initialized_at BIGINT,
			updated_at BIGINT NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_mirror_sync_cursor_lease_until ON mirror_sync_cursor(lease_until)`,
		`CREATE INDEX IF NOT EXISTS idx_mirror_sync_cursor_updated_at ON mirror_sync_cursor(updated_at)`,
	}

	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("ensure mirror schema: %w", err)
		}
	}
	return nil
}

func (s *PgStore) ListOwnersForSync(ctx context.Context, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 128
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT ip.user_id
		 FROM item_permissions ip
		 LEFT JOIN mirror_sync_cursor c ON c.owner_user_id = ip.user_id
		 LEFT JOIN sync_changes sc ON sc.item_id = ip.item_id
		 WHERE ip.permission = 'owner'
		 GROUP BY ip.user_id, c.last_seq, c.initialized_at, c.updated_at
		 HAVING c.initialized_at IS NULL OR COALESCE(MAX(sc.seq), 0) > COALESCE(c.last_seq, 0)
		 ORDER BY COALESCE(c.updated_at, 0) ASC, ip.user_id ASC
		 LIMIT $1`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list owners for sync: %w", err)
	}
	defer rows.Close()

	var owners []string
	for rows.Next() {
		var ownerUserID string
		if err := rows.Scan(&ownerUserID); err != nil {
			return nil, fmt.Errorf("scan owner for sync: %w", err)
		}
		if ownerUserID != "" {
			owners = append(owners, ownerUserID)
		}
	}
	return owners, rows.Err()
}

func (s *PgStore) AcquireCursorLease(ctx context.Context, ownerUserID, leaseOwner string, leaseTTL time.Duration) (*vaultsync.CursorState, bool, error) {
	nowMs := time.Now().UnixMilli()
	leaseUntilMs := nowMs + leaseTTL.Milliseconds()

	row := s.db.QueryRowContext(ctx,
		`INSERT INTO mirror_sync_cursor (
			owner_user_id, last_seq, lease_owner, lease_until, last_error, initialized_at, updated_at
		)
		VALUES ($1, 0, $2, $3, '', NULL, $4)
		ON CONFLICT (owner_user_id) DO UPDATE
		SET lease_owner = EXCLUDED.lease_owner,
		    lease_until = EXCLUDED.lease_until,
		    updated_at = EXCLUDED.updated_at
		WHERE mirror_sync_cursor.lease_until < $4
		   OR mirror_sync_cursor.lease_owner = EXCLUDED.lease_owner
		RETURNING owner_user_id, last_seq, lease_owner, lease_until, last_error, initialized_at, updated_at`,
		ownerUserID, leaseOwner, leaseUntilMs, nowMs,
	)

	var cursor vaultsync.CursorState
	var initializedAt sql.NullInt64
	if err := row.Scan(
		&cursor.OwnerUserID,
		&cursor.LastSeq,
		&cursor.LeaseOwner,
		&cursor.LeaseUntilMs,
		&cursor.LastError,
		&initializedAt,
		&cursor.UpdatedAtMs,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("acquire cursor lease: %w", err)
	}
	cursor.Initialized = initializedAt.Valid
	return &cursor, true, nil
}

func (s *PgStore) ReleaseCursorLease(ctx context.Context, ownerUserID, leaseOwner string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE mirror_sync_cursor
		 SET lease_owner = '', lease_until = 0, updated_at = $3
		 WHERE owner_user_id = $1 AND lease_owner = $2`,
		ownerUserID, leaseOwner, time.Now().UnixMilli(),
	)
	if err != nil {
		return fmt.Errorf("release cursor lease: %w", err)
	}
	return nil
}

func (s *PgStore) GetChangesAfterSeq(ctx context.Context, userID string, afterSeq, limit int) ([]vaultsync.ChangeRecord, error) {
	if limit <= 0 {
		limit = 100
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT sc.seq, sc.item_id, sc.change_type, sc.created_at
		 FROM sync_changes sc
		 JOIN item_permissions ip ON ip.item_id = sc.item_id AND ip.permission = 'owner'
		 WHERE ip.user_id = $1 AND sc.seq > $2
		 ORDER BY sc.seq ASC
		 LIMIT $3`,
		userID, afterSeq, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("get changes after seq: %w", err)
	}
	defer rows.Close()

	changes := make([]vaultsync.ChangeRecord, 0, limit)
	for rows.Next() {
		var change vaultsync.ChangeRecord
		change.UserID = userID
		if err := rows.Scan(&change.Seq, &change.ItemID, &change.ChangeType, &change.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan change after seq: %w", err)
		}
		changes = append(changes, change)
	}
	return changes, rows.Err()
}

func (s *PgStore) MarkCursorInitialized(ctx context.Context, ownerUserID, leaseOwner string, seq int) error {
	nowMs := time.Now().UnixMilli()
	_, err := s.db.ExecContext(ctx,
		`UPDATE mirror_sync_cursor
		 SET last_seq = GREATEST(last_seq, $3),
		     initialized_at = COALESCE(initialized_at, $4),
		     last_error = '',
		     updated_at = $4
		 WHERE owner_user_id = $1 AND lease_owner = $2`,
		ownerUserID, leaseOwner, seq, nowMs,
	)
	if err != nil {
		return fmt.Errorf("mark cursor initialized: %w", err)
	}
	return nil
}

func (s *PgStore) AdvanceCursor(ctx context.Context, ownerUserID, leaseOwner string, seq int) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE mirror_sync_cursor
		 SET last_seq = GREATEST(last_seq, $3),
		     last_error = '',
		     updated_at = $4
		 WHERE owner_user_id = $1 AND lease_owner = $2`,
		ownerUserID, leaseOwner, seq, time.Now().UnixMilli(),
	)
	if err != nil {
		return fmt.Errorf("advance cursor: %w", err)
	}
	return nil
}

func (s *PgStore) RecordCursorError(ctx context.Context, ownerUserID, leaseOwner, message string) error {
	if len(message) > 1000 {
		message = message[:1000]
	}
	message = strings.TrimSpace(message)
	_, err := s.db.ExecContext(ctx,
		`UPDATE mirror_sync_cursor
		 SET last_error = $3, updated_at = $4
		 WHERE owner_user_id = $1 AND lease_owner = $2`,
		ownerUserID, leaseOwner, message, time.Now().UnixMilli(),
	)
	if err != nil {
		return fmt.Errorf("record cursor error: %w", err)
	}
	return nil
}

func (s *PgStore) GetBacklogStats(ctx context.Context) (vaultsync.BacklogStats, error) {
	row := s.db.QueryRowContext(ctx,
		`WITH owner_progress AS (
			SELECT ip.user_id,
			       COALESCE(c.last_seq, 0) AS last_seq,
			       c.initialized_at
			  FROM item_permissions ip
			  LEFT JOIN mirror_sync_cursor c ON c.owner_user_id = ip.user_id
			 WHERE ip.permission = 'owner'
			 GROUP BY ip.user_id, c.last_seq, c.initialized_at
		),
		pending AS (
			SELECT op.user_id,
			       MIN(sc.created_at) AS oldest_created_at
			  FROM owner_progress op
			  LEFT JOIN item_permissions ip ON ip.user_id = op.user_id AND ip.permission = 'owner'
			  LEFT JOIN sync_changes sc ON sc.item_id = ip.item_id AND sc.seq > op.last_seq
			 GROUP BY op.user_id, op.initialized_at
			HAVING op.initialized_at IS NULL OR MIN(sc.created_at) IS NOT NULL
		)
		SELECT
			COALESCE((SELECT COUNT(*) FROM pending), 0),
			COALESCE((SELECT MIN(oldest_created_at) FROM pending), 0),
			COALESCE((SELECT COUNT(*) FROM mirror_sync_cursor WHERE last_error != ''), 0)`,
	)

	var stats vaultsync.BacklogStats
	if err := row.Scan(&stats.OwnersWithBacklog, &stats.OldestPendingAtMs, &stats.StuckOwners); err != nil {
		return vaultsync.BacklogStats{}, fmt.Errorf("get backlog stats: %w", err)
	}
	return stats, nil
}
