package database

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"time"
)

// ChatMessage represents a single chat message stored in the chat_messages table.
type ChatMessage struct {
	ID            string          `json:"id"`
	ThreadID      string          `json:"thread_id"`
	Mode          string          `json:"mode"`
	Role          string          `json:"role"`
	Content       string          `json:"content"`
	Thinking      string          `json:"thinking,omitempty"`
	ToolCalls     json.RawMessage `json:"tool_calls,omitempty"`
	ToolCallID    string          `json:"tool_call_id,omitempty"`
	AttachedItems json.RawMessage `json:"attached_items,omitempty"`
	TokenUsage    json.RawMessage `json:"token_usage,omitempty"`
	Seq           int64           `json:"seq"`
	CreatedAt     time.Time       `json:"created_at"`
}

// EnsureChatMessagesTable creates the chat_messages table and indexes if they
// do not already exist. Call this at startup after PgStore creation.
func (s *PgStore) EnsureChatMessagesTable(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS chat_messages (
			id             TEXT PRIMARY KEY,
			thread_id      TEXT NOT NULL,
			mode           TEXT NOT NULL DEFAULT 'chat',
			role           TEXT NOT NULL,
			content        TEXT,
			thinking       TEXT,
			tool_calls     JSONB,
			tool_call_id   TEXT,
			attached_items JSONB,
			token_usage    JSONB,
			seq            BIGSERIAL,
			created_at     TIMESTAMPTZ DEFAULT NOW()
		)`)
	if err != nil {
		return fmt.Errorf("create chat_messages table: %w", err)
	}

	_, err = s.db.ExecContext(ctx, `
		CREATE INDEX IF NOT EXISTS idx_chat_messages_thread_seq
		ON chat_messages (thread_id, mode, seq)`)
	if err != nil {
		return fmt.Errorf("create idx_chat_messages_thread_seq: %w", err)
	}

	_, err = s.db.ExecContext(ctx, `
		CREATE INDEX IF NOT EXISTS idx_chat_messages_id
		ON chat_messages (id)`)
	if err != nil {
		return fmt.Errorf("create idx_chat_messages_id: %w", err)
	}

	log.Println("vault-mirror-service: chat_messages table ensured")
	return nil
}

// InsertChatMessage inserts a new chat message. If msg.ID is empty a random
// ID of the form "msg_<hex16>" is generated.
func (s *PgStore) InsertChatMessage(ctx context.Context, msg *ChatMessage) error {
	if msg.ID == "" {
		msg.ID = fmt.Sprintf("msg_%s", randomHex(8))
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO chat_messages (id, thread_id, mode, role, content, thinking,
			tool_calls, tool_call_id, attached_items, token_usage)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		msg.ID, msg.ThreadID, msg.Mode, msg.Role, msg.Content, msg.Thinking,
		nullableJSON(msg.ToolCalls), msg.ToolCallID,
		nullableJSON(msg.AttachedItems), nullableJSON(msg.TokenUsage),
	)
	return err
}

// GetMessagesAfter returns messages for a thread/mode after a cursor.
// When cursorID is empty the newest `limit` messages are returned (in ascending order).
// hasPrevious indicates whether older messages exist before the returned set.
func (s *PgStore) GetMessagesAfter(ctx context.Context, threadID, mode, cursorID string, limit int) ([]ChatMessage, bool, error) {
	if limit <= 0 {
		limit = 50
	}

	var rows []ChatMessage
	var hasPrevious bool

	if cursorID != "" {
		var cursorSeq int64
		err := s.db.QueryRowContext(ctx,
			"SELECT seq FROM chat_messages WHERE id=$1", cursorID).Scan(&cursorSeq)
		if err != nil {
			// cursor not found – return empty
			return nil, false, nil
		}
		pgRows, err := s.db.QueryContext(ctx, `
			SELECT id,thread_id,mode,role,content,thinking,tool_calls,tool_call_id,attached_items,token_usage,seq,created_at
			FROM chat_messages WHERE thread_id=$1 AND mode=$2 AND seq > $3 ORDER BY seq ASC`,
			threadID, mode, cursorSeq)
		if err != nil {
			return nil, false, err
		}
		defer pgRows.Close()
		rows, err = scanMessages(pgRows)
		if err != nil {
			return nil, false, err
		}
	} else {
		pgRows, err := s.db.QueryContext(ctx, `
			SELECT id,thread_id,mode,role,content,thinking,tool_calls,tool_call_id,attached_items,token_usage,seq,created_at
			FROM chat_messages WHERE thread_id=$1 AND mode=$2 ORDER BY seq DESC LIMIT $3`,
			threadID, mode, limit)
		if err != nil {
			return nil, false, err
		}
		defer pgRows.Close()
		rows, err = scanMessages(pgRows)
		if err != nil {
			return nil, false, err
		}
		reverseMessages(rows)
	}

	if len(rows) > 0 {
		var exists bool
		_ = s.db.QueryRowContext(ctx, `
			SELECT EXISTS(SELECT 1 FROM chat_messages WHERE thread_id=$1 AND mode=$2 AND seq < $3)`,
			threadID, mode, rows[0].Seq).Scan(&exists)
		hasPrevious = exists
	}

	return rows, hasPrevious, nil
}

// GetMessagesBefore returns up to `limit` messages before the given cursor,
// ordered ascending by seq. hasPrevious indicates whether even older messages exist.
func (s *PgStore) GetMessagesBefore(ctx context.Context, threadID, mode, cursorID string, limit int) ([]ChatMessage, bool, error) {
	if limit <= 0 {
		limit = 50
	}

	var cursorSeq int64
	err := s.db.QueryRowContext(ctx,
		"SELECT seq FROM chat_messages WHERE id=$1", cursorID).Scan(&cursorSeq)
	if err != nil {
		return nil, false, nil
	}

	pgRows, err := s.db.QueryContext(ctx, `
		SELECT id,thread_id,mode,role,content,thinking,tool_calls,tool_call_id,attached_items,token_usage,seq,created_at
		FROM chat_messages WHERE thread_id=$1 AND mode=$2 AND seq < $3 ORDER BY seq DESC LIMIT $4`,
		threadID, mode, cursorSeq, limit)
	if err != nil {
		return nil, false, err
	}
	defer pgRows.Close()
	rows, err := scanMessages(pgRows)
	if err != nil {
		return nil, false, err
	}
	reverseMessages(rows)

	var hasPrevious bool
	if len(rows) > 0 {
		_ = s.db.QueryRowContext(ctx, `
			SELECT EXISTS(SELECT 1 FROM chat_messages WHERE thread_id=$1 AND mode=$2 AND seq < $3)`,
			threadID, mode, rows[0].Seq).Scan(&hasPrevious)
	}

	return rows, hasPrevious, nil
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// scanMessages reads all rows from a *sql.Rows into a []ChatMessage slice.
func scanMessages(rows *sql.Rows) ([]ChatMessage, error) {
	var out []ChatMessage
	for rows.Next() {
		var m ChatMessage
		var content, thinking, toolCallID sql.NullString
		var toolCalls, attachedItems, tokenUsage []byte

		if err := rows.Scan(
			&m.ID, &m.ThreadID, &m.Mode, &m.Role,
			&content, &thinking, &toolCalls, &toolCallID,
			&attachedItems, &tokenUsage,
			&m.Seq, &m.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan chat_message: %w", err)
		}

		m.Content = content.String
		m.Thinking = thinking.String
		m.ToolCallID = toolCallID.String
		if len(toolCalls) > 0 {
			m.ToolCalls = toolCalls
		}
		if len(attachedItems) > 0 {
			m.AttachedItems = attachedItems
		}
		if len(tokenUsage) > 0 {
			m.TokenUsage = tokenUsage
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// reverseMessages reverses a slice of ChatMessage in place.
func reverseMessages(msgs []ChatMessage) {
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
}

// randomHex returns n random bytes encoded as a hex string (2*n chars).
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// nullableJSON returns nil (SQL NULL) when the json.RawMessage is empty or
// represents JSON null, so that JSONB columns store NULL instead of an empty value.
func nullableJSON(data json.RawMessage) interface{} {
	if len(data) == 0 || string(data) == "null" {
		return nil
	}
	return []byte(data)
}
