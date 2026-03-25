package database

import (
	"context"
	"fmt"
)

// ThreadInfo represents a thread in the thread_mapping table.
type ThreadInfo struct {
	ThreadID string `json:"threadID"`
	Title    string `json:"title"`
	Mode     string `json:"mode"`
}

// GetThreadsByMemberID returns all threads for a member filtered by mode,
// ordered by creation time descending (newest first).
func (s *PgStore) GetThreadsByMemberID(ctx context.Context, memberID, mode string) ([]ThreadInfo, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT thread_id, COALESCE(thread_title, ''), mode
		FROM thread_mapping WHERE member_id=$1 AND mode=$2
		ORDER BY updated_at DESC`, memberID, mode)
	if err != nil {
		return nil, fmt.Errorf("get threads by member: %w", err)
	}
	defer rows.Close()

	var threads []ThreadInfo
	for rows.Next() {
		var t ThreadInfo
		if err := rows.Scan(&t.ThreadID, &t.Title, &t.Mode); err != nil {
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
