package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	_ "github.com/lib/pq"
	"github.com/redis/go-redis/v9"

	"github.com/urusaqqrun/vault-mirror-service/model"
)

// PgStore PostgreSQL 資料存取層
type PgStore struct {
	db  *sql.DB
	rdb *redis.Client
}

func NewPgStore(ctx context.Context, pgURI string) (*PgStore, error) {
	db, err := sql.Open("postgres", pgURI)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	db.SetMaxOpenConns(15)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	log.Println("vault-mirror-service: PostgreSQL connected")
	store := &PgStore{db: db}
	if err := store.ensureMirrorSchema(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *PgStore) SetRedis(rdb *redis.Client) { s.rdb = rdb }

func (s *PgStore) Close() error {
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

// ---------------------------------------------------------------------------
// DataReader 介面（sync/event_handler.go 使用）
// ---------------------------------------------------------------------------

func (s *PgStore) GetItem(ctx context.Context, userID, itemID string) (*model.Item, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT bi.id, bi.item_type, bi.name, bi.fields, bi.version, bi.created_at, bi.updated_at, bi.backlinks
		 FROM base_items bi
		 JOIN item_permissions ip ON ip.item_id = bi.id AND ip.permission = 'owner'
		 WHERE bi.id = $1 AND ip.user_id = $2`,
		itemID, userID,
	)
	return s.scanItem(row)
}

func (s *PgStore) ListAllItems(ctx context.Context, userID string) ([]*model.Item, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT bi.id, bi.item_type, bi.name, bi.fields, bi.version, bi.created_at, bi.updated_at, bi.backlinks
		 FROM base_items bi
		 JOIN item_permissions ip ON ip.item_id = bi.id AND ip.permission = 'owner'
		 WHERE ip.user_id = $1`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.scanItems(rows)
}

// ---------------------------------------------------------------------------
// DataWriter 介面（executor/writeback.go 使用）
// 所有 Upsert 統一寫入 base_items + sync_changes
// ---------------------------------------------------------------------------

func (s *PgStore) UpsertItem(ctx context.Context, userID string, doc map[string]interface{}) error {
	id, _ := doc["_id"].(string)
	if id == "" {
		return fmt.Errorf("missing _id in doc")
	}
	itemType, _ := doc["itemType"].(string)
	if itemType == "" {
		return fmt.Errorf("missing itemType in doc (id=%s)", id)
	}

	fieldsRaw, _ := doc["fields"]
	fields := toMap(fieldsRaw)
	// 從 fields 取得 name（優先 doc 頂層 name）
	name, _ := doc["name"].(string)
	if name == "" {
		if n, ok := fields["name"].(string); ok {
			name = n
		}
		if name == "" {
			if t, ok := fields["title"].(string); ok {
				name = t
			}
		}
		if name == "" {
			if fn, ok := fields["folderName"].(string); ok {
				name = fn
			}
		}
	}

	version := 1
	if u, ok := fields["usn"]; ok {
		version = toInt(u)
	}
	delete(fields, "usn")
	delete(fields, "ownerID")

	now := time.Now().UnixMilli()
	createdAt := now
	if c, ok := fields["createdAt"]; ok {
		if v := toInt64(c); v > 0 {
			createdAt = v
		}
	}
	updatedAt := now
	if u, ok := fields["updatedAt"]; ok {
		if v := toInt64(u); v > 0 {
			updatedAt = v
		}
	}

	delete(fields, "backlinks")

	fieldsJSON, err := json.Marshal(fields)
	if err != nil {
		return fmt.Errorf("marshal fields: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	var oldContent string
	var oldSubjectID string
	var oldFieldsJSON string
	err = tx.QueryRowContext(ctx,
		`SELECT COALESCE(fields, '{}') FROM base_items WHERE id = $1`, id,
	).Scan(&oldFieldsJSON)
	if err == nil {
		var oldFields map[string]interface{}
		if json.Unmarshal([]byte(oldFieldsJSON), &oldFields) == nil {
			oldContent, _ = oldFields["content"].(string)
			oldSubjectID, _ = oldFields["subjectID"].(string)
		}
	}

	_, err = tx.ExecContext(ctx,
		`INSERT INTO base_items (id, item_type, name, fields, version, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (id) DO UPDATE SET
		   item_type = EXCLUDED.item_type,
		   name = EXCLUDED.name,
		   fields = EXCLUDED.fields,
		   version = EXCLUDED.version,
		   updated_at = EXCLUDED.updated_at`,
		id, itemType, name, string(fieldsJSON), version, createdAt, updatedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert base_items: %w", err)
	}

	_, err = tx.ExecContext(ctx,
		`INSERT INTO item_permissions (item_id, user_id, permission, created_at, updated_at)
		 VALUES ($1, $2, 'owner', $3, $4)
		 ON CONFLICT (item_id, user_id) DO NOTHING`,
		id, userID, createdAt, updatedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert item_permissions: %w", err)
	}

	_, err = tx.ExecContext(ctx,
		`INSERT INTO sync_changes (item_id, item_version, change_type, actor_id, details, created_at)
		 VALUES ($1, $2, 'updated', $3, 'mirror-service writeback', $4)`,
		id, version, userID, now,
	)
	if err != nil {
		return fmt.Errorf("insert sync_changes: %w", err)
	}

	newContent, _ := fields["content"].(string)
	if oldContent != newContent {
		s.ProcessBacklinksOnContentChange(ctx, tx, id, name, itemType, oldContent, newContent, userID)
	}

	newSubjectID, _ := fields["subjectID"].(string)
	s.ProcessBacklinksOnSubjectChange(ctx, tx, id, oldSubjectID, newSubjectID, userID)

	return tx.Commit()
}

func (s *PgStore) DeleteItemDoc(ctx context.Context, userID, docID string, version int) error {
	now := time.Now().UnixMilli()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	var exists bool
	err = tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM base_items WHERE id = $1
		 AND EXISTS (SELECT 1 FROM item_permissions WHERE item_id = $1 AND user_id = $2 AND permission = 'owner'))`,
		docID, userID,
	).Scan(&exists)
	if err != nil {
		return fmt.Errorf("check existence: %w", err)
	}
	if !exists {
		return nil
	}

	// 刪除前清除 backlinks：subjectID → child backlink、content → mention backlinks
	var delFieldsJSON string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(fields, '{}') FROM base_items WHERE id = $1`, docID).Scan(&delFieldsJSON); err == nil {
		var delFields map[string]interface{}
		if json.Unmarshal([]byte(delFieldsJSON), &delFields) == nil {
			if subjectID, ok := delFields["subjectID"].(string); ok && subjectID != "" {
				s.removeBacklink(ctx, tx, subjectID, docID, "child", userID)
			}
			if content, ok := delFields["content"].(string); ok && content != "" {
				s.ProcessBacklinksOnContentChange(ctx, tx, docID, "", "", content, "", userID)
			}
		}
	}

	_, err = tx.ExecContext(ctx,
		`INSERT INTO sync_changes (item_id, item_version, change_type, actor_id, details, created_at)
		 VALUES ($1, $2, 'deleted', $3, 'mirror-service writeback', $4)`,
		docID, version, userID, now,
	)
	if err != nil {
		return fmt.Errorf("insert sync_changes for delete: %w", err)
	}

	_, err = tx.ExecContext(ctx,
		`DELETE FROM base_items WHERE id = $1`,
		docID,
	)
	if err != nil {
		return fmt.Errorf("hard delete: %w", err)
	}

	return tx.Commit()
}

func (s *PgStore) DeleteDocument(ctx context.Context, userID, collection, docID string, usn int) error {
	return s.DeleteItemDoc(ctx, userID, docID, usn)
}

// ---------------------------------------------------------------------------
// ---------------------------------------------------------------------------
// IncrementUSN（版本號遞增）
// 使用 Redis INCR 維持與 golang_service 相容的 usn:{ownerID} key
// ---------------------------------------------------------------------------

var incrIfExists = redis.NewScript(`
	if redis.call("EXISTS", KEYS[1]) == 1 then
		return redis.call("INCR", KEYS[1])
	end
	return -1
`)

var initAndIncrUSN = redis.NewScript(`
	if redis.call("EXISTS", KEYS[1]) == 0 then
		redis.call("SET", KEYS[1], ARGV[1])
	end
	return redis.call("INCR", KEYS[1])
`)

func (s *PgStore) IncrementVersion(ctx context.Context, userID string) (int, error) {
	if s.rdb == nil {
		return s.incrementVersionViaPG(ctx, userID)
	}

	key := "usn:" + userID

	result, err := incrIfExists.Run(ctx, s.rdb, []string{key}).Int64()
	if err == nil && result > 0 {
		if sErr := s.rdb.SAdd(ctx, "usn:dirty", userID).Err(); sErr != nil {
			log.Printf("[PgStore] SAdd usn:dirty 失敗: %v", sErr)
		}
		return int(result), nil
	}

	// 慢路徑：key 不存在，從 PG base_items 取基準 version
	baseVersion := s.getMaxVersionFromPG(ctx, userID)

	newVersion, err := initAndIncrUSN.Run(ctx, s.rdb, []string{key}, baseVersion).Int64()
	if err != nil {
		log.Printf("[PgStore] Redis Lua INCR 失敗，fallback PG: %v", err)
		return s.incrementVersionViaPG(ctx, userID)
	}

	if sErr := s.rdb.SAdd(ctx, "usn:dirty", userID).Err(); sErr != nil {
		log.Printf("[PgStore] SAdd usn:dirty 失敗: %v", sErr)
	}
	return int(newVersion), nil
}

func (s *PgStore) getMaxVersionFromPG(ctx context.Context, userID string) int {
	var maxVer sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(bi.version), 0) FROM base_items bi
		 JOIN item_permissions ip ON ip.item_id = bi.id AND ip.permission = 'owner'
		 WHERE ip.user_id = $1`,
		userID,
	).Scan(&maxVer)
	if err != nil || !maxVer.Valid {
		return 0
	}
	return int(maxVer.Int64)
}

func (s *PgStore) incrementVersionViaPG(ctx context.Context, userID string) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin tx for version incr: %w", err)
	}
	defer tx.Rollback()

	// 以 userID 的 hash 作為 advisory lock key，確保同一用戶串行化
	_, err = tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtext($1))`, userID)
	if err != nil {
		return 0, fmt.Errorf("advisory lock: %w", err)
	}

	var maxVer sql.NullInt64
	err = tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(bi.version), 0) FROM base_items bi
		 JOIN item_permissions ip ON ip.item_id = bi.id AND ip.permission = 'owner'
		 WHERE ip.user_id = $1`,
		userID,
	).Scan(&maxVer)
	if err != nil {
		return 0, fmt.Errorf("select max version: %w", err)
	}

	newVer := 1
	if maxVer.Valid {
		newVer = int(maxVer.Int64) + 1
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit version incr tx: %w", err)
	}
	return newVer, nil
}

func (s *PgStore) SyncUserVersion(ctx context.Context, userID string) error {
	return nil
}

// ---------------------------------------------------------------------------
// latestUSNReader 介面（AI 任務開始前的基準版本號）
// ---------------------------------------------------------------------------

func (s *PgStore) GetLatestVersion(ctx context.Context, userID string) (int, error) {
	var maxVer sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(bi.version), 0) FROM base_items bi
		 JOIN item_permissions ip ON ip.item_id = bi.id AND ip.permission = 'owner'
		 WHERE ip.user_id = $1`,
		userID,
	).Scan(&maxVer)
	if err != nil {
		return 0, fmt.Errorf("get latest version: %w", err)
	}
	if !maxVer.Valid {
		return 0, nil
	}
	return int(maxVer.Int64), nil
}

// ---------------------------------------------------------------------------
// SeqReader 介面（同步 worker 初始化 cursor 用）
// ---------------------------------------------------------------------------

// GetLatestSeq 取得該用戶在 sync_changes 中的最大 seq。
func (s *PgStore) GetLatestSeq(ctx context.Context, userID string) (int, error) {
	var maxSeq sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT MAX(sc.seq) FROM sync_changes sc
		 JOIN base_items bi ON bi.id = sc.item_id
		 JOIN item_permissions ip ON ip.item_id = bi.id AND ip.permission = 'owner'
		 WHERE ip.user_id = $1`,
		userID,
	).Scan(&maxSeq)
	if err != nil {
		return 0, fmt.Errorf("get latest seq: %w", err)
	}
	if !maxSeq.Valid {
		return 0, nil
	}
	return int(maxSeq.Int64), nil
}

// ---------------------------------------------------------------------------
// 內部工具函式
// ---------------------------------------------------------------------------

func (s *PgStore) scanItem(row *sql.Row) (*model.Item, error) {
	var id, itemType, name, fieldsJSON, backlinksJSON string
	var version int
	var createdAt, updatedAt int64
	if err := row.Scan(&id, &itemType, &name, &fieldsJSON, &version, &createdAt, &updatedAt, &backlinksJSON); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}

	var fields map[string]interface{}
	if err := json.Unmarshal([]byte(fieldsJSON), &fields); err != nil {
		fields = make(map[string]interface{})
	}
	fields["usn"] = version
	fields["createdAt"] = fmt.Sprintf("%d", createdAt)
	fields["updatedAt"] = fmt.Sprintf("%d", updatedAt)

	var backlinks []interface{}
	if err := json.Unmarshal([]byte(backlinksJSON), &backlinks); err == nil && len(backlinks) > 0 {
		fields["backlinks"] = backlinks
	}

	return &model.Item{
		ID:     id,
		Name:   name,
		Type:   itemType,
		Fields: fields,
	}, nil
}

func (s *PgStore) scanItems(rows *sql.Rows) ([]*model.Item, error) {
	var out []*model.Item
	for rows.Next() {
		var id, itemType, name, fieldsJSON, backlinksJSON string
		var version int
		var createdAt, updatedAt int64
		if err := rows.Scan(&id, &itemType, &name, &fieldsJSON, &version, &createdAt, &updatedAt, &backlinksJSON); err != nil {
			log.Printf("[scanItems] scan error: %v", err)
			continue
		}
		var fields map[string]interface{}
		if err := json.Unmarshal([]byte(fieldsJSON), &fields); err != nil {
			fields = make(map[string]interface{})
		}
		fields["usn"] = version
		fields["createdAt"] = fmt.Sprintf("%d", createdAt)
		fields["updatedAt"] = fmt.Sprintf("%d", updatedAt)

		var backlinks []interface{}
		if err := json.Unmarshal([]byte(backlinksJSON), &backlinks); err == nil && len(backlinks) > 0 {
			fields["backlinks"] = backlinks
		}

		out = append(out, &model.Item{
			ID:     id,
			Name:   name,
			Type:   itemType,
			Fields: fields,
		})
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// 型別轉換工具
// ---------------------------------------------------------------------------

func toMap(v interface{}) map[string]interface{} {
	if m, ok := v.(map[string]interface{}); ok {
		return m
	}
	return make(map[string]interface{})
}

func toInt(v interface{}) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case int32:
		return int(n)
	case float64:
		return int(n)
	case string:
		var parsed int
		if _, err := fmt.Sscanf(n, "%d", &parsed); err == nil {
			return parsed
		}
		return 0
	default:
		return 0
	}
}

func toInt64(v interface{}) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case int32:
		return int64(n)
	case float64:
		return int64(n)
	case string:
		if strings.TrimSpace(n) == "" {
			return 0
		}
		var i int64
		if _, err := fmt.Sscanf(n, "%d", &i); err == nil {
			return i
		}
		return 0
	default:
		return 0
	}
}
