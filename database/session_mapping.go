package database

import (
	"context"
	"fmt"
	"log"
)

// EnsureSessionMappingConstraint adds the unique constraint on (member_id, session_id)
// if it doesn't already exist. Required for ON CONFLICT upsert.
func (s *PgStore) EnsureSessionMappingConstraint(ctx context.Context) error {
	// First remove duplicates (keep the newest row per member_id+session_id)
	_, _ = s.db.ExecContext(ctx, `
		DELETE FROM session_mapping a USING session_mapping b
		WHERE a.id < b.id AND a.member_id = b.member_id AND a.session_id = b.session_id`)

	_, err := s.db.ExecContext(ctx, `
		DO $$ BEGIN
			IF NOT EXISTS (
				SELECT 1 FROM pg_constraint
				WHERE conname = 'uq_session_mapping_member_session'
			) THEN
				ALTER TABLE session_mapping
				ADD CONSTRAINT uq_session_mapping_member_session
				UNIQUE (member_id, session_id);
			END IF;
		END $$`)
	if err != nil {
		return fmt.Errorf("ensure session_mapping constraint: %w", err)
	}
	log.Println("vault-mirror-service: session_mapping constraint ensured")
	return nil
}

// SessionInfo represents a session in the session_mapping table.
// JSON tags use snake_case to match the frontend (chatStore.ts syncSessions).
type SessionInfo struct {
	SessionID string `json:"session_id"`
	Title     string `json:"session_title"`
	Mode      string `json:"mode"`
	UpdatedAt string `json:"updated_at"`
}

// GetSessionsByMemberID returns all sessions for a member filtered by mode,
// ordered by creation time descending (newest first).
func (s *PgStore) GetSessionsByMemberID(ctx context.Context, memberID, mode string) ([]SessionInfo, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT session_id, COALESCE(session_title, ''), mode,
		       COALESCE(updated_at::text, '0')
		FROM session_mapping WHERE member_id=$1 AND mode=$2
		ORDER BY updated_at DESC`, memberID, mode)
	if err != nil {
		return nil, fmt.Errorf("get sessions by member: %w", err)
	}
	defer rows.Close()

	var sessions []SessionInfo
	for rows.Next() {
		var t SessionInfo
		if err := rows.Scan(&t.SessionID, &t.Title, &t.Mode, &t.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		sessions = append(sessions, t)
	}
	return sessions, rows.Err()
}

// AddSessionMapping inserts or updates a session mapping entry. It also enforces
// a per-member limit (50 for chat, 200 for task) by deleting the oldest sessions
// that exceed the cap.
func (s *PgStore) AddSessionMapping(ctx context.Context, memberID, sessionID, title, mode string) error {
	limit := 50
	if mode == "task" {
		limit = 200
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO session_mapping (member_id, session_id, session_title, mode)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (member_id, session_id) DO UPDATE SET session_title = $3`,
		memberID, sessionID, title, mode)
	if err != nil {
		return fmt.Errorf("upsert session mapping: %w", err)
	}

	// Trim sessions that exceed the per-member limit.
	_, err = s.db.ExecContext(ctx, `
		DELETE FROM session_mapping WHERE member_id=$1 AND mode=$2
		AND session_id NOT IN (
			SELECT session_id FROM session_mapping
			WHERE member_id=$1 AND mode=$2
			ORDER BY updated_at DESC LIMIT $3
		)`, memberID, mode, limit)
	if err != nil {
		return fmt.Errorf("trim session mappings: %w", err)
	}
	return nil
}

// DeleteUserSessions deletes all session mappings and associated chat messages
// for a given member. Returns (sessionsDeleted, messagesDeleted, error).
func (s *PgStore) DeleteUserSessions(ctx context.Context, memberID string) (int, int, error) {
	// Collect session IDs belonging to this member.
	rows, err := s.db.QueryContext(ctx,
		"SELECT session_id FROM session_mapping WHERE member_id=$1", memberID)
	if err != nil {
		return 0, 0, fmt.Errorf("select session ids: %w", err)
	}
	defer rows.Close()

	var sessionIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return 0, 0, fmt.Errorf("scan session id: %w", err)
		}
		sessionIDs = append(sessionIDs, id)
	}
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}

	// Delete chat messages for each session.
	msgDeleted := 0
	for _, sid := range sessionIDs {
		res, err := s.db.ExecContext(ctx,
			"DELETE FROM chat_messages WHERE session_id=$1", sid)
		if err != nil {
			continue
		}
		n, _ := res.RowsAffected()
		msgDeleted += int(n)
	}

	// Delete session mappings.
	res, err := s.db.ExecContext(ctx,
		"DELETE FROM session_mapping WHERE member_id=$1", memberID)
	if err != nil {
		return 0, 0, fmt.Errorf("delete session mappings: %w", err)
	}
	sessionsDeleted, _ := res.RowsAffected()

	return int(sessionsDeleted), msgDeleted, nil
}
