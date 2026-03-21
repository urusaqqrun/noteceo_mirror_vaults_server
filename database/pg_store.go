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

	"github.com/urusaqqrun/vault-mirror-service/executor"
	"github.com/urusaqqrun/vault-mirror-service/model"
	vaultsync "github.com/urusaqqrun/vault-mirror-service/sync"
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
	return &PgStore{db: db}, nil
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
		`SELECT bi.id, bi.item_type, bi.name, bi.fields, bi.version, bi.created_at, bi.updated_at
		 FROM base_items bi
		 JOIN item_permissions ip ON ip.item_id = bi.id AND ip.permission = 'owner'
		 WHERE bi.id = $1 AND ip.user_id = $2 AND bi.deleted_at IS NULL`,
		itemID, userID,
	)
	return s.scanItem(row)
}

func (s *PgStore) ListItemFolders(ctx context.Context, userID string) ([]*model.Item, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT bi.id, bi.item_type, bi.name, bi.fields, bi.version, bi.created_at, bi.updated_at
		 FROM base_items bi
		 JOIN item_permissions ip ON ip.item_id = bi.id AND ip.permission = 'owner'
		 WHERE ip.user_id = $1 AND bi.deleted_at IS NULL
		 AND (bi.item_type = 'FOLDER' OR bi.item_type LIKE '%\_FOLDER' ESCAPE '\')`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.scanItems(rows)
}

func (s *PgStore) ListAllItems(ctx context.Context, userID string) ([]*model.Item, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT bi.id, bi.item_type, bi.name, bi.fields, bi.version, bi.created_at, bi.updated_at
		 FROM base_items bi
		 JOIN item_permissions ip ON ip.item_id = bi.id AND ip.permission = 'owner'
		 WHERE ip.user_id = $1 AND bi.deleted_at IS NULL`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.scanItems(rows)
}

// 以下為 DataReader 舊介面的相容實作，golang_service 只發 collection=item 事件，
// 這些方法是 fallback，直接從 base_items 轉換

func (s *PgStore) ListFolders(ctx context.Context, userID string) ([]*model.Folder, error) {
	items, err := s.ListItemFolders(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]*model.Folder, 0, len(items))
	for _, item := range items {
		out = append(out, itemToFolder(item))
	}
	return out, nil
}

func (s *PgStore) GetFolder(ctx context.Context, userID, folderID string) (*model.Folder, error) {
	item, err := s.GetItem(ctx, userID, folderID)
	if err != nil || item == nil {
		return nil, err
	}
	if !model.IsFolder(item.Type) {
		return nil, nil
	}
	return itemToFolder(item), nil
}

func (s *PgStore) GetNote(ctx context.Context, userID, noteID string) (*model.Note, error) {
	item, err := s.GetItem(ctx, userID, noteID)
	if err != nil || item == nil {
		return nil, err
	}
	return itemToNote(item), nil
}

func (s *PgStore) GetCard(ctx context.Context, userID, cardID string) (*model.Card, error) {
	item, err := s.GetItem(ctx, userID, cardID)
	if err != nil || item == nil {
		return nil, err
	}
	return itemToCard(item), nil
}

func (s *PgStore) GetChart(ctx context.Context, userID, chartID string) (*model.Chart, error) {
	item, err := s.GetItem(ctx, userID, chartID)
	if err != nil || item == nil {
		return nil, err
	}
	return itemToChart(item), nil
}

// ---------------------------------------------------------------------------
// DataWriter 介面（executor/writeback.go 使用）
// 所有 Upsert 統一寫入 base_items + sync_changes
// ---------------------------------------------------------------------------

func (s *PgStore) UpsertItem(ctx context.Context, userID string, doc executor.Doc) error {
	id, _ := doc["_id"].(string)
	if id == "" {
		return fmt.Errorf("missing _id in doc")
	}
	itemType, _ := doc["itemType"].(string)
	if itemType == "" {
		itemType = "NOTE"
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

	fieldsJSON, err := json.Marshal(fields)
	if err != nil {
		return fmt.Errorf("marshal fields: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

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
		`INSERT INTO sync_changes (item_id, item_version, change_type, actor_id, actor_name, created_at)
		 VALUES ($1, $2, 'updated', $3, 'mirror-service', $4)`,
		id, version, userID, now,
	)
	if err != nil {
		return fmt.Errorf("insert sync_changes: %w", err)
	}

	return tx.Commit()
}

// UpsertFolder/Note/Card/Chart 統一轉換為 Item 格式後呼叫 UpsertItem
func (s *PgStore) UpsertFolder(ctx context.Context, userID string, doc executor.Doc) error {
	return s.upsertLegacyAsItem(ctx, userID, doc, "FOLDER")
}

func (s *PgStore) UpsertNote(ctx context.Context, userID string, doc executor.Doc) error {
	return s.upsertLegacyAsItem(ctx, userID, doc, "NOTE")
}

func (s *PgStore) UpsertCard(ctx context.Context, userID string, doc executor.Doc) error {
	return s.upsertLegacyAsItem(ctx, userID, doc, "CARD")
}

func (s *PgStore) UpsertChart(ctx context.Context, userID string, doc executor.Doc) error {
	return s.upsertLegacyAsItem(ctx, userID, doc, "CHART")
}

// upsertLegacyAsItem 將舊格式 doc 轉為 Item 格式
func (s *PgStore) upsertLegacyAsItem(ctx context.Context, userID string, doc executor.Doc, defaultType string) error {
	id, _ := doc["_id"].(string)
	if id == "" {
		return fmt.Errorf("missing _id in legacy doc")
	}

	fields := make(map[string]interface{})
	for k, v := range doc {
		if k == "_id" {
			continue
		}
		fields[k] = v
	}

	itemDoc := executor.Doc{
		"_id":      id,
		"itemType": defaultType,
		"fields":   fields,
	}
	return s.UpsertItem(ctx, userID, itemDoc)
}

func (s *PgStore) DeleteItemDoc(ctx context.Context, userID, docID string, version int) error {
	now := time.Now().UnixMilli()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx,
		`UPDATE base_items SET deleted_at = $1, version = $2, updated_at = $1
		 WHERE id = $3 AND deleted_at IS NULL
		 AND EXISTS (SELECT 1 FROM item_permissions WHERE item_id = $3 AND user_id = $4 AND permission = 'owner')`,
		now, version, docID, userID,
	)
	if err != nil {
		return fmt.Errorf("soft delete: %w", err)
	}

	affected, _ := result.RowsAffected()
	if affected > 0 {
		_, err = tx.ExecContext(ctx,
			`INSERT INTO sync_changes (item_id, item_version, change_type, actor_id, actor_name, created_at)
			 VALUES ($1, $2, 'deleted', $3, 'mirror-service', $4)`,
			docID, version, userID, now,
		)
		if err != nil {
			return fmt.Errorf("insert sync_changes for delete: %w", err)
		}
	}

	return tx.Commit()
}

func (s *PgStore) DeleteDocument(ctx context.Context, userID, collection, docID string, usn int) error {
	return s.DeleteItemDoc(ctx, userID, docID, usn)
}

// ---------------------------------------------------------------------------
// USNReader 介面（衝突判定用）
// ---------------------------------------------------------------------------

func (s *PgStore) GetDocUSN(ctx context.Context, userID, collection, docID string) (int, error) {
	var version int
	err := s.db.QueryRowContext(ctx,
		`SELECT bi.version FROM base_items bi
		 JOIN item_permissions ip ON ip.item_id = bi.id AND ip.permission = 'owner'
		 WHERE bi.id = $1 AND ip.user_id = $2 AND bi.deleted_at IS NULL`,
		docID, userID,
	).Scan(&version)
	if err == sql.ErrNoRows {
		return -1, nil
	}
	if err != nil {
		return 0, err
	}
	return version, nil
}

// ---------------------------------------------------------------------------
// USNIncrementer 介面（版本號遞增）
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

func (s *PgStore) IncrementUSN(ctx context.Context, userID string) (int, error) {
	if s.rdb == nil {
		return s.incrementUSNViaPG(ctx, userID)
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
	baseUSN := s.getMaxVersionFromPG(ctx, userID)

	newUSN, err := initAndIncrUSN.Run(ctx, s.rdb, []string{key}, baseUSN).Int64()
	if err != nil {
		log.Printf("[PgStore] Redis Lua INCR 失敗，fallback PG: %v", err)
		return s.incrementUSNViaPG(ctx, userID)
	}

	if sErr := s.rdb.SAdd(ctx, "usn:dirty", userID).Err(); sErr != nil {
		log.Printf("[PgStore] SAdd usn:dirty 失敗: %v", sErr)
	}
	return int(newUSN), nil
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

func (s *PgStore) incrementUSNViaPG(ctx context.Context, userID string) (int, error) {
	// 使用 pg_advisory_xact_lock 確保同一用戶的 USN 遞增是原子的
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin tx for USN incr: %w", err)
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
		return 0, fmt.Errorf("commit USN incr tx: %w", err)
	}
	return newVer, nil
}

// ---------------------------------------------------------------------------
// USNSyncer 介面（PostgreSQL 模式不需要額外同步，no-op）
// ---------------------------------------------------------------------------

func (s *PgStore) SyncUserUSN(ctx context.Context, userID string) error {
	return nil
}

// ---------------------------------------------------------------------------
// latestUSNReader 介面（AI 任務開始前的基準版本號）
// ---------------------------------------------------------------------------

func (s *PgStore) GetLatestUSN(ctx context.Context, userID string) (int, error) {
	var maxVer sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(bi.version), 0) FROM base_items bi
		 JOIN item_permissions ip ON ip.item_id = bi.id AND ip.permission = 'owner'
		 WHERE ip.user_id = $1`,
		userID,
	).Scan(&maxVer)
	if err != nil {
		return 0, fmt.Errorf("get latest USN: %w", err)
	}
	if !maxVer.Valid {
		return 0, nil
	}
	return int(maxVer.Int64), nil
}

// ---------------------------------------------------------------------------
// SeqReader 介面（任務回寫後推進 Poller 游標用）
// ---------------------------------------------------------------------------

// GetLatestSeq 取得該用戶在 sync_changes 中的最大 seq（Poller 追蹤的游標）
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
// USNQuerier 介面（usn_poller 使用，改為從 sync_changes 查詢）
// ---------------------------------------------------------------------------

func (s *PgStore) GetChangesAfterUSN(ctx context.Context, userID string, afterUSN int) ([]vaultsync.SyncEvent, error) {
	// afterUSN 在 PG 模式下對應 seq
	rows, err := s.db.QueryContext(ctx,
		`SELECT sc.seq, sc.item_id, sc.change_type
		 FROM sync_changes sc
		 JOIN base_items bi ON bi.id = sc.item_id
		 JOIN item_permissions ip ON ip.item_id = bi.id AND ip.permission = 'owner'
		 WHERE sc.seq > $1 AND ip.user_id = $2
		 ORDER BY sc.seq ASC
		 LIMIT 100`,
		afterUSN, userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ts := time.Now().UnixMilli()
	var events []vaultsync.SyncEvent
	for rows.Next() {
		var seq int64
		var itemID, changeType string
		if err := rows.Scan(&seq, &itemID, &changeType); err != nil {
			log.Printf("[GetChangesAfterUSN] scan error: %v", err)
			continue
		}
		action := "update"
		if changeType == "deleted" {
			action = "delete"
		}
		events = append(events, vaultsync.SyncEvent{
			Collection: "item",
			UserID:     userID,
			DocID:      itemID,
			Action:     action,
			Timestamp:  ts,
			USN:        int(seq),
		})
	}
	return events, rows.Err()
}

// ListActiveUsers 從 base_items 查詢最近 7 天有更新的用戶
func (s *PgStore) ListActiveUsers(ctx context.Context) ([]string, error) {
	sevenDaysAgoMs := time.Now().AddDate(0, 0, -7).UnixMilli()
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT ip.user_id FROM base_items bi
		 JOIN item_permissions ip ON ip.item_id = bi.id AND ip.permission = 'owner'
		 WHERE bi.updated_at > $1`,
		sevenDaysAgoMs,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []string
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err == nil && uid != "" {
			users = append(users, uid)
		}
	}
	return users, rows.Err()
}

// ---------------------------------------------------------------------------
// 內部工具函式
// ---------------------------------------------------------------------------

func (s *PgStore) scanItem(row *sql.Row) (*model.Item, error) {
	var id, itemType, name, fieldsJSON string
	var version int
	var createdAt, updatedAt int64
	if err := row.Scan(&id, &itemType, &name, &fieldsJSON, &version, &createdAt, &updatedAt); err != nil {
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
		var id, itemType, name, fieldsJSON string
		var version int
		var createdAt, updatedAt int64
		if err := rows.Scan(&id, &itemType, &name, &fieldsJSON, &version, &createdAt, &updatedAt); err != nil {
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
// Item → 舊 model 轉換（相容 DataReader 舊介面）
// ---------------------------------------------------------------------------

func itemToFolder(item *model.Item) *model.Folder {
	f := &model.Folder{
		ID:  item.ID,
		Usn: item.GetUSN(),
	}

	if n := item.GetName(); n != "" {
		f.FolderName = n
	}
	if v := model.StrPtrField(item.Fields, "folderType"); v != nil {
		f.Type = v
	} else {
		sub := model.FolderSubType(item.Type)
		if sub != "" {
			f.Type = &sub
		}
	}
	f.ParentID = model.StrPtrField(item.Fields, "parentID")
	f.OrderAt = model.StrPtrField(item.Fields, "orderAt")
	f.Icon = model.StrPtrField(item.Fields, "icon")
	f.CreatedAt = model.Int64StrField(item.Fields, "createdAt")
	f.UpdatedAt = model.Int64StrField(item.Fields, "updatedAt")
	f.FolderSummary = model.StrPtrField(item.Fields, "folderSummary")
	f.AiFolderName = model.StrPtrField(item.Fields, "aiFolderName")
	f.AiFolderSummary = model.StrPtrField(item.Fields, "aiFolderSummary")
	f.AiInstruction = model.StrPtrField(item.Fields, "aiInstruction")
	f.AutoUpdateSummary = model.BoolField(item.Fields, "autoUpdateSummary")
	f.IsTemp = model.BoolField(item.Fields, "isTemp")
	f.NoteNum = model.Int64Field(item.Fields, "noteNum")
	f.TemplateHTML = model.StrPtrField(item.Fields, "templateHtml")
	f.TemplateCSS = model.StrPtrField(item.Fields, "templateCss")
	f.UIPrompt = model.StrPtrField(item.Fields, "uiPrompt")
	f.IsShared = model.BoolField(item.Fields, "isShared")
	f.Searchable = model.BoolField(item.Fields, "searchable")
	f.AllowContribute = model.BoolField(item.Fields, "allowContribute")
	f.ChartKind = model.StrPtrField(item.Fields, "chartKind")
	return f
}

func itemToNote(item *model.Item) *model.Note {
	n := &model.Note{
		ID:    item.ID,
		Title: model.StrPtrField(item.Fields, "title"),
		Content:  model.StrPtrField(item.Fields, "content"),
		Tags:     model.StringSliceField(item.Fields, "tags"),
		FolderID: func() string {
			if v := model.StrPtrField(item.Fields, "folderID"); v != nil {
				return *v
			}
			return ""
		}(),
		ParentID:  model.StrPtrField(item.Fields, "parentID"),
		Usn:       item.GetUSN(),
		CreateAt:  model.Int64Field(item.Fields, "createdAt"),
		UpdateAt:  model.Int64Field(item.Fields, "updatedAt"),
		OrderAt:   model.StrPtrField(item.Fields, "orderAt"),
		Status:    model.StrPtrField(item.Fields, "status"),
		AiTitle:   model.StrPtrField(item.Fields, "aiTitle"),
		AiTags:    model.StringSliceField(item.Fields, "aiTags"),
		ImgURLs:   model.StringSliceField(item.Fields, "imgURLs"),
		IsNew:     model.BoolField(item.Fields, "isNew"),
	}
	if t := model.StrPtrField(item.Fields, "_type"); t != nil {
		n.Type = *t
	} else if item.Type == "TODO" {
		n.Type = "TODO"
	}
	return n
}

func itemToCard(item *model.Item) *model.Card {
	return &model.Card{
		ID:       item.ID,
		ParentID: func() string { v := model.StrPtrField(item.Fields, "parentID"); if v != nil { return *v }; return "" }(),
		Name:      item.GetName(),
		Fields:    model.StrPtrField(item.Fields, "fields"),
		Reviews:   model.StrPtrField(item.Fields, "reviews"),
		OrderAt:   model.StrPtrField(item.Fields, "orderAt"),
		IsDeleted: model.BoolField(item.Fields, "isDeleted"),
		CreatedAt: model.Int64StrField(item.Fields, "createdAt"),
		UpdatedAt: model.Int64StrField(item.Fields, "updatedAt"),
		Usn:       item.GetUSN(),
	}
}

func itemToChart(item *model.Item) *model.Chart {
	return &model.Chart{
		ID:       item.ID,
		ParentID: func() string { v := model.StrPtrField(item.Fields, "parentID"); if v != nil { return *v }; return "" }(),
		Name:      item.GetName(),
		Data:      model.StrPtrField(item.Fields, "data"),
		IsDeleted: model.BoolField(item.Fields, "isDeleted"),
		CreatedAt: model.Int64StrField(item.Fields, "createdAt"),
		UpdatedAt: model.Int64StrField(item.Fields, "updatedAt"),
		Usn:       item.GetUSN(),
	}
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
