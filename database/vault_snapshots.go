package database

import (
	"context"
	"time"
)

// SnapshotRow 對應 vault_snapshots 表的一筆資料
type SnapshotRow struct {
	FilePath string
	Hash     string
	Mtime    int64
	DocID    string
}

func (s *PgStore) EnsureVaultSnapshotsTable(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS vault_snapshots (
			member_id  TEXT   NOT NULL,
			file_path  TEXT   NOT NULL,
			hash       TEXT   NOT NULL,
			mtime      BIGINT NOT NULL,
			doc_id     TEXT   DEFAULT '',
			updated_at BIGINT NOT NULL,
			PRIMARY KEY (member_id, file_path)
		)`)
	if err != nil {
		return err
	}
	_, _ = s.db.ExecContext(ctx,
		`CREATE INDEX IF NOT EXISTS idx_vault_snapshots_member ON vault_snapshots (member_id)`)
	return nil
}

func (s *PgStore) GetSnapshot(ctx context.Context, memberID string) ([]SnapshotRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT file_path, hash, mtime, COALESCE(doc_id, '') FROM vault_snapshots WHERE member_id = $1`,
		memberID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SnapshotRow
	for rows.Next() {
		var r SnapshotRow
		if err := rows.Scan(&r.FilePath, &r.Hash, &r.Mtime, &r.DocID); err != nil {
			continue
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *PgStore) UpsertSnapshotFiles(ctx context.Context, memberID string, files []SnapshotRow) error {
	if len(files) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now().UnixMilli()
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO vault_snapshots (member_id, file_path, hash, mtime, doc_id, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (member_id, file_path) DO UPDATE SET
			hash = EXCLUDED.hash,
			mtime = EXCLUDED.mtime,
			doc_id = EXCLUDED.doc_id,
			updated_at = EXCLUDED.updated_at`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, f := range files {
		if _, err := stmt.ExecContext(ctx, memberID, f.FilePath, f.Hash, f.Mtime, f.DocID, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *PgStore) DeleteSnapshotFiles(ctx context.Context, memberID string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	for _, p := range paths {
		if _, err := s.db.ExecContext(ctx,
			`DELETE FROM vault_snapshots WHERE member_id = $1 AND file_path = $2`,
			memberID, p); err != nil {
			return err
		}
	}
	return nil
}

func (s *PgStore) ReplaceSnapshot(ctx context.Context, memberID string, files []SnapshotRow) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM vault_snapshots WHERE member_id = $1`, memberID); err != nil {
		return err
	}

	now := time.Now().UnixMilli()
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO vault_snapshots (member_id, file_path, hash, mtime, doc_id, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, f := range files {
		if _, err := stmt.ExecContext(ctx, memberID, f.FilePath, f.Hash, f.Mtime, f.DocID, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *PgStore) SnapshotExists(ctx context.Context, memberID string) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM vault_snapshots WHERE member_id = $1 LIMIT 1`,
		memberID).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// --- Single-file methods (for SnapshotAwareVaultFS wrapper) ---

func (s *PgStore) UpsertSnapshotFile(ctx context.Context, memberID, filePath, hash, docID string, mtime int64) error {
	now := time.Now().UnixMilli()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO vault_snapshots (member_id, file_path, hash, mtime, doc_id, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (member_id, file_path) DO UPDATE SET
			hash = EXCLUDED.hash, mtime = EXCLUDED.mtime,
			doc_id = EXCLUDED.doc_id, updated_at = EXCLUDED.updated_at`,
		memberID, filePath, hash, mtime, docID, now)
	return err
}

func (s *PgStore) DeleteSnapshotFile(ctx context.Context, memberID, filePath string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM vault_snapshots WHERE member_id = $1 AND file_path = $2`,
		memberID, filePath)
	return err
}

func (s *PgStore) DeleteSnapshotByPrefix(ctx context.Context, memberID, prefix string) error {
	if prefix == "" {
		_, err := s.db.ExecContext(ctx,
			`DELETE FROM vault_snapshots WHERE member_id = $1`, memberID)
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM vault_snapshots WHERE member_id = $1 AND (file_path = $2 OR file_path LIKE $3)`,
		memberID, prefix, prefix+"/%")
	return err
}

func (s *PgStore) RenameSnapshotPrefix(ctx context.Context, memberID, oldPrefix, newPrefix string) error {
	now := time.Now().UnixMilli()
	_, err := s.db.ExecContext(ctx, `
		UPDATE vault_snapshots
		SET file_path = $3 || substr(file_path, length($2) + 1),
		    updated_at = $4
		WHERE member_id = $1
		  AND (file_path = $2 OR file_path LIKE $2 || '/%')`,
		memberID, oldPrefix, newPrefix, now)
	return err
}
