package database

import (
	"context"
	"fmt"
	"log"
)

// EnsureThreadMappingConstraint adds the unique constraint on (member_id, thread_id)
// if it doesn't already exist. Required for ON CONFLICT upsert.
func (s *PgStore) EnsureThreadMappingConstraint(ctx context.Context) error {
	// First remove duplicates (keep the newest row per member_id+thread_id)
	_, _ = s.db.ExecContext(ctx, `
		DELETE FROM thread_mapping a USING thread_mapping b
		WHERE a.id < b.id AND a.member_id = b.member_id AND a.thread_id = b.thread_id`)

	_, err := s.db.ExecContext(ctx, `
		DO $$ BEGIN
			IF NOT EXISTS (
				SELECT 1 FROM pg_constraint
				WHERE conname = 'uq_thread_mapping_member_thread'
			) THEN
				ALTER TABLE thread_mapping
				ADD CONSTRAINT uq_thread_mapping_member_thread
				UNIQUE (member_id, thread_id);
			END IF;
		END $$`)
	if err != nil {
		return fmt.Errorf("ensure thread_mapping constraint: %w", err)
	}
	log.Println("vault-mirror-service: thread_mapping constraint ensured")
	return nil
}

// ThreadInfo represents a thread in the thread_mapping table.
// JSON tags use snake_case to match the frontend (chatStore.ts syncThreads).
type ThreadInfo struct {
	ThreadID  string `json:"thread_id"`
	Title     string `json:"thread_title"`
	Mode      string `json:"mode"`
	UpdatedAt string `json:"updated_at"`
}

// GetThreadsByMemberID returns all threads for a member filtered by mode,
// ordered by creation time descending (newest first).
func (s *PgStore) GetThreadsByMemberID(ctx context.Context, memberID, mode string) ([]ThreadInfo, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT thread_id, COALESCE(thread_title, ''), mode,
		       COALESCE(updated_at::text, '0')
		FROM thread_mapping WHERE member_id=$1 AND mode=$2
		ORDER BY updated_at DESC`, memberID, mode)
	if err != nil {
		return nil, fmt.Errorf("get threads by member: %w", err)
	}
	defer rows.Close()

	var threads []ThreadInfo
	for rows.Next() {
		var t ThreadInfo
		if err := rows.Scan(&t.ThreadID, &t.Title, &t.Mode, &t.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan thread: %w", err)
		}
		threads = append(threads, t)
	}
	return threads, rows.Err()
}

// AddThreadMapping inserts or updates a thread mapping entry. It also enforces
// a per-member limit (50 for chat, 200 for task) by deleting the oldest threads
// that exceed the cap.
func (s *PgStore) AddThreadMapping(ctx context.Context, memberID, threadID, title, mode string) error {
	limit := 50
	if mode == "task" {
		limit = 200
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO thread_mapping (member_id, thread_id, thread_title, mode)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (member_id, thread_id) DO UPDATE SET thread_title = $3`,
		memberID, threadID, title, mode)
	if err != nil {
		return fmt.Errorf("upsert thread mapping: %w", err)
	}

	// Trim threads that exceed the per-member limit.
	_, err = s.db.ExecContext(ctx, `
		DELETE FROM thread_mapping WHERE member_id=$1 AND mode=$2
		AND thread_id NOT IN (
			SELECT thread_id FROM thread_mapping
			WHERE member_id=$1 AND mode=$2
			ORDER BY updated_at DESC LIMIT $3
		)`, memberID, mode, limit)
	if err != nil {
		return fmt.Errorf("trim thread mappings: %w", err)
	}
	return nil
}

// DeleteUserThreads deletes all thread mappings and associated chat messages
// for a given member. Returns (threadsDeleted, messagesDeleted, error).
func (s *PgStore) DeleteUserThreads(ctx context.Context, memberID string) (int, int, error) {
	// Collect thread IDs belonging to this member.
	rows, err := s.db.QueryContext(ctx,
		"SELECT thread_id FROM thread_mapping WHERE member_id=$1", memberID)
	if err != nil {
		return 0, 0, fmt.Errorf("select thread ids: %w", err)
	}
	defer rows.Close()

	var threadIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return 0, 0, fmt.Errorf("scan thread id: %w", err)
		}
		threadIDs = append(threadIDs, id)
	}
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}

	// Delete chat messages for each thread.
	msgDeleted := 0
	for _, tid := range threadIDs {
		res, err := s.db.ExecContext(ctx,
			"DELETE FROM chat_messages WHERE thread_id=$1", tid)
		if err != nil {
			continue
		}
		n, _ := res.RowsAffected()
		msgDeleted += int(n)
	}

	// Delete thread mappings.
	res, err := s.db.ExecContext(ctx,
		"DELETE FROM thread_mapping WHERE member_id=$1", memberID)
	if err != nil {
		return 0, 0, fmt.Errorf("delete thread mappings: %w", err)
	}
	threadsDeleted, _ := res.RowsAffected()

	return int(threadsDeleted), msgDeleted, nil
}
