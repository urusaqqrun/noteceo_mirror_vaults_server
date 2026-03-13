package database

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/urusaqqrun/vault-mirror-service/model"
)

// upsertDoc 通用 upsert 邏輯：從 doc 取出 _id 作為 filter，$set 時移除 _id 避免 MongoDB 錯誤
func (m *MongoReader) upsertDoc(ctx context.Context, col *mongo.Collection, userID string, doc bson.M) error {
	id, ok := doc["_id"].(string)
	if !ok || id == "" {
		return fmt.Errorf("missing _id in doc")
	}
	// $set 不可包含 _id，否則 MongoDB 會報錯或靜默忽略
	setDoc := make(bson.M, len(doc))
	for k, v := range doc {
		if k == "_id" {
			continue
		}
		setDoc[k] = v
	}
	setDoc["memberID"] = userID
	_, err := col.UpdateOne(ctx,
		bson.M{"_id": id, "memberID": userID},
		bson.M{"$set": setDoc},
		options.Update().SetUpsert(true),
	)
	return err
}

// UpsertFolder 以 _id 為 key 做 upsert
func (m *MongoReader) UpsertFolder(ctx context.Context, userID string, doc bson.M) error {
	return m.upsertDoc(ctx, m.foldersCol(), userID, doc)
}

// UpsertNote 以 _id 為 key 做 upsert
func (m *MongoReader) UpsertNote(ctx context.Context, userID string, doc bson.M) error {
	return m.upsertDoc(ctx, m.notesCol(), userID, doc)
}

// UpsertCard 以 _id 為 key 做 upsert
func (m *MongoReader) UpsertCard(ctx context.Context, userID string, doc bson.M) error {
	return m.upsertDoc(ctx, m.cardsCol(), userID, doc)
}

// UpsertChart 以 _id 為 key 做 upsert
func (m *MongoReader) UpsertChart(ctx context.Context, userID string, doc bson.M) error {
	return m.upsertDoc(ctx, m.chartsCol(), userID, doc)
}

// UpsertItem 以 _id 為 key 對 Item collection 做 upsert（fields.memberID 作為 filter）。
// 使用 dot notation 展開 fields 子文件，確保僅更新傳入的欄位，不覆蓋 DB 中既有但未傳入的欄位。
func (m *MongoReader) UpsertItem(ctx context.Context, userID string, doc bson.M) error {
	id, ok := doc["_id"].(string)
	if !ok || id == "" {
		return fmt.Errorf("missing _id in item doc")
	}

	setDoc := make(bson.M)

	// 頂層欄位（name, itemType 等）直接 $set
	for k, v := range doc {
		if k == "_id" || k == "fields" {
			continue
		}
		setDoc[k] = v
	}

	// fields 子文件展開為 dot notation，避免整個替換
	var fields bson.M
	switch typed := doc["fields"].(type) {
	case bson.M:
		fields = typed
	case map[string]interface{}:
		fields = bson.M(typed)
	default:
		fields = bson.M{}
	}
	fields["memberID"] = userID
	for k, v := range fields {
		setDoc["fields."+k] = v
	}

	_, err := m.itemsCol().UpdateOne(ctx, bson.M{"_id": id, "fields.memberID": userID},
		bson.M{"$set": setDoc},
		options.Update().SetUpsert(true),
	)
	return err
}

// DeleteItemDoc 從 Item collection 刪除文件並寫入 ItemDeletionLog
func (m *MongoReader) DeleteItemDoc(ctx context.Context, userID, docID string, usn int) error {
	res, err := m.itemsCol().DeleteOne(ctx, bson.M{"_id": docID, "fields.memberID": userID})
	if err != nil {
		return err
	}
	if res.DeletedCount > 0 {
		if logErr := m.writeDeletionLog(ctx, userID, "item", docID, usn); logErr != nil {
			log.Printf("[DeleteItemDoc] deletion log write error (item/%s): %v", docID, logErr)
		}
	}
	return nil
}

// DeleteDocument 刪除指定 collection 的文件，並寫入對應的 Deletion Log。
func (m *MongoReader) DeleteDocument(ctx context.Context, userID, collection, docID string, usn int) error {
	if collection == "item" {
		return m.DeleteItemDoc(ctx, userID, docID, usn)
	}
	var col *mongo.Collection
	switch collection {
	case "note":
		col = m.notesCol()
	case "card":
		col = m.cardsCol()
	case "chart":
		col = m.chartsCol()
	default:
		col = m.foldersCol()
	}
	res, err := col.DeleteOne(ctx, bson.M{"_id": docID, "memberID": userID})
	if err != nil {
		return err
	}
	if res.DeletedCount > 0 {
		if logErr := m.writeDeletionLog(ctx, userID, collection, docID, usn); logErr != nil {
			log.Printf("[DeleteDocument] deletion log write error (%s/%s): %v", collection, docID, logErr)
		}
	}
	return nil
}

// writeDeletionLog 寫入 deletion log，欄位格式與 noteceo_golang_service 一致
func (m *MongoReader) writeDeletionLog(ctx context.Context, userID, collection, docID string, usn int) error {
	now := time.Now().UnixMilli()
	switch collection {
	case "note":
		_, err := m.db.Collection("NoteDeletionLog").InsertOne(ctx, bson.M{
			"noteID":   docID,
			"memberID": userID,
			"deleteAt": now,
			"usn":      usn,
		})
		return err
	case "folder":
		_, err := m.db.Collection("FolderDeletionLog").InsertOne(ctx, bson.M{
			"folderID": docID,
			"memberID": userID,
			"deleteAt": now,
			"usn":      usn,
		})
		return err
	case "card":
		nowStr := strconv.FormatInt(now, 10)
		_, err := m.db.Collection("DeletedCard").InsertOne(ctx, bson.M{
			"deletedID": docID,
			"memberID":  userID,
			"deletedAt": nowStr,
			"usn":       usn,
		})
		return err
	case "chart":
		nowStr := strconv.FormatInt(now, 10)
		_, err := m.db.Collection("DeletedChart").InsertOne(ctx, bson.M{
			"deletedID": docID,
			"memberID":  userID,
			"deletedAt": nowStr,
			"usn":       usn,
		})
		return err
	case "item":
		_, err := m.db.Collection("ItemDeletionLog").InsertOne(ctx, bson.M{
			"itemID":   docID,
			"memberID": userID,
			"deleteAt": now,
			"usn":      usn,
		})
		return err
	default:
		return fmt.Errorf("unknown collection for deletion log: %s", collection)
	}
}

// incrIfExists 原子性快速路徑：key 存在才 INCR，不存在回傳 -1（避免意外建立 key）
var incrIfExists = redis.NewScript(`
	if redis.call("EXISTS", KEYS[1]) == 1 then
		return redis.call("INCR", KEYS[1])
	end
	return -1
`)

// initAndIncrUSN 原子化初始化 + INCR：若 key 不存在則先以 baseUSN 初始化再 INCR
var initAndIncrUSN = redis.NewScript(`
	if redis.call("EXISTS", KEYS[1]) == 0 then
		redis.call("SET", KEYS[1], ARGV[1])
	end
	return redis.call("INCR", KEYS[1])
`)

// IncrementUSN 遞增用戶 USN，使用 Redis INCR 搭配 MongoDB fallback。
// Redis key 格式 usn:{memberID} 與 noteceo_golang_service 共用。
func (m *MongoReader) IncrementUSN(ctx context.Context, userID string) (int, error) {
	if m.rdb != nil {
		return m.incrementUSNViaRedis(ctx, userID)
	}
	return m.incrementUSNViaMongo(ctx, userID)
}

func (m *MongoReader) incrementUSNViaRedis(ctx context.Context, userID string) (int, error) {
	key := "usn:" + userID

	// 快速路徑：原子性檢查 key 存在才 INCR，不存在回傳 -1
	result, err := incrIfExists.Run(ctx, m.rdb, []string{key}).Int64()
	if err == nil && result > 0 {
		m.rdb.SAdd(ctx, "usn:dirty", userID)
		return int(result), nil
	}

	// 慢路徑：key 不存在，查 MongoDB 基準值，再用 Lua 原子初始化+INCR
	baseUSN, _ := m.getUserUSNFromMongo(ctx, userID)

	newUSN, err := initAndIncrUSN.Run(ctx, m.rdb, []string{key}, baseUSN).Int64()
	if err != nil {
		log.Printf("Redis Lua INCR 失敗，fallback MongoDB: %v", err)
		return m.incrementUSNViaMongo(ctx, userID)
	}

	m.rdb.SAdd(ctx, "usn:dirty", userID)
	return int(newUSN), nil
}

// SyncUserUSN 將 Redis 中的最新 USN 立即同步到 MongoDB User 文件，
// 不依賴 golang_service 的 SyncWorker。回寫完成後呼叫。
func (m *MongoReader) SyncUserUSN(ctx context.Context, userID string) error {
	if m.rdb == nil {
		return nil
	}
	key := "usn:" + userID
	val, err := m.rdb.Get(ctx, key).Result()
	if err != nil {
		return fmt.Errorf("redis GET %s: %w", key, err)
	}
	usn, err := strconv.Atoi(val)
	if err != nil {
		return fmt.Errorf("invalid USN value %q: %w", val, err)
	}

	_, err = m.db.Collection("User").UpdateOne(ctx,
		buildUserFilter(userID),
		bson.M{"$max": bson.M{"usn": usn}},
	)
	if err != nil {
		return fmt.Errorf("update User.usn: %w", err)
	}

	m.rdb.SRem(ctx, "usn:dirty", userID)
	return nil
}

func (m *MongoReader) getUserUSNFromMongo(ctx context.Context, userID string) (int, error) {
	coll := m.db.Collection("User")
	var row struct {
		Usn int `bson:"usn"`
	}
	err := coll.FindOne(ctx, buildUserFilter(userID),
		options.FindOne().SetProjection(bson.M{"usn": 1}),
	).Decode(&row)
	if err != nil {
		return 0, err
	}
	return row.Usn, nil
}

func (m *MongoReader) incrementUSNViaMongo(ctx context.Context, userID string) (int, error) {
	coll := m.db.Collection("User")
	after := options.After
	res := coll.FindOneAndUpdate(ctx,
		buildUserFilter(userID),
		bson.M{"$inc": bson.M{"usn": 1}},
		&options.FindOneAndUpdateOptions{ReturnDocument: &after},
	)
	var row struct {
		Usn int `bson:"usn"`
	}
	if err := res.Decode(&row); err != nil {
		return 0, err
	}
	return row.Usn, nil
}

func buildUserFilter(userID string) bson.M {
	orFilters := bson.A{
		bson.M{"_id": userID},
		bson.M{"memberID": userID},
	}
	if oid, err := primitive.ObjectIDFromHex(userID); err == nil {
		orFilters = append(orFilters, bson.M{"_id": oid})
	}
	return bson.M{"$or": orFilters}
}

// ListAllNotes 回傳用戶所有 Note（全量匯出用）
func (m *MongoReader) ListAllNotes(ctx context.Context, userID string) ([]*model.Note, error) {
	cur, err := m.notesCol().Find(ctx, bson.M{"memberID": userID})
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []*model.Note
	for cur.Next(ctx) {
		var n model.Note
		if err := cur.Decode(&n); err != nil {
			log.Printf("[ListAllNotes] decode error: %v", err)
			continue
		}
		out = append(out, &n)
	}
	return out, cur.Err()
}

// ListAllCards 回傳用戶所有 Card（全量匯出用）
func (m *MongoReader) ListAllCards(ctx context.Context, userID string) ([]*model.Card, error) {
	cur, err := m.cardsCol().Find(ctx, bson.M{"memberID": userID})
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []*model.Card
	for cur.Next(ctx) {
		var c model.Card
		if err := cur.Decode(&c); err != nil {
			log.Printf("[ListAllCards] decode error: %v", err)
			continue
		}
		out = append(out, &c)
	}
	return out, cur.Err()
}

// ListAllCharts 回傳用戶所有 Chart（全量匯出用）
func (m *MongoReader) ListAllCharts(ctx context.Context, userID string) ([]*model.Chart, error) {
	cur, err := m.chartsCol().Find(ctx, bson.M{"memberID": userID})
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []*model.Chart
	for cur.Next(ctx) {
		var c model.Chart
		if err := cur.Decode(&c); err != nil {
			log.Printf("[ListAllCharts] decode error: %v", err)
			continue
		}
		out = append(out, &c)
	}
	return out, cur.Err()
}
