package database

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/redis/go-redis/v9"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/urusaqqrun/vault-mirror-service/model"
	vaultsync "github.com/urusaqqrun/vault-mirror-service/sync"
)

// MongoReader 提供同步所需的 MongoDB 讀寫能力。
type MongoReader struct {
	client *mongo.Client
	db     *mongo.Database
	rdb    *redis.Client
}

// SetRedis 注入 Redis 用戶端（USN 遞增用）
func (m *MongoReader) SetRedis(rdb *redis.Client) {
	m.rdb = rdb
}

func NewMongoReader(ctx context.Context, mongoURI, dbName string) (*MongoReader, error) {
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(mongoURI))
	if err != nil {
		return nil, fmt.Errorf("connect mongo: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx, nil); err != nil {
		return nil, fmt.Errorf("ping mongo: %w", err)
	}
	return &MongoReader{client: client, db: client.Database(dbName)}, nil
}

func (m *MongoReader) Close(ctx context.Context) error {
	if m.client == nil {
		return nil
	}
	return m.client.Disconnect(ctx)
}

func (m *MongoReader) foldersCol() *mongo.Collection { return m.db.Collection("Folder") }
func (m *MongoReader) notesCol() *mongo.Collection   { return m.db.Collection("Note") }
func (m *MongoReader) cardsCol() *mongo.Collection   { return m.db.Collection("Card") }
func (m *MongoReader) chartsCol() *mongo.Collection  { return m.db.Collection("Chart") }
func (m *MongoReader) itemsCol() *mongo.Collection   { return m.db.Collection("Item") }

func (m *MongoReader) ListFolders(ctx context.Context, userID string) ([]*model.Folder, error) {
	cur, err := m.foldersCol().Find(ctx, bson.M{"memberID": userID})
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	var out []*model.Folder
	for cur.Next(ctx) {
		var f model.Folder
		if err := cur.Decode(&f); err != nil {
			continue
		}
		out = append(out, &f)
	}
	return out, cur.Err()
}

func (m *MongoReader) GetFolder(ctx context.Context, userID, folderID string) (*model.Folder, error) {
	var f model.Folder
	err := m.foldersCol().FindOne(ctx, bson.M{"_id": folderID, "memberID": userID}).Decode(&f)
	if err == mongo.ErrNoDocuments {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &f, nil
}

func (m *MongoReader) GetNote(ctx context.Context, userID, noteID string) (*model.Note, error) {
	var n model.Note
	err := m.notesCol().FindOne(ctx, bson.M{"_id": noteID, "memberID": userID}).Decode(&n)
	if err == mongo.ErrNoDocuments {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &n, nil
}

func (m *MongoReader) GetCard(ctx context.Context, userID, cardID string) (*model.Card, error) {
	var c model.Card
	err := m.cardsCol().FindOne(ctx, bson.M{"_id": cardID, "memberID": userID}).Decode(&c)
	if err == mongo.ErrNoDocuments {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (m *MongoReader) GetChart(ctx context.Context, userID, chartID string) (*model.Chart, error) {
	var c model.Chart
	err := m.chartsCol().FindOne(ctx, bson.M{"_id": chartID, "memberID": userID}).Decode(&c)
	if err == mongo.ErrNoDocuments {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// GetItem 從 Item collection 讀取單一 item
func (m *MongoReader) GetItem(ctx context.Context, userID, itemID string) (*model.Item, error) {
	var item model.Item
	err := m.itemsCol().FindOne(ctx, bson.M{"_id": itemID, "fields.memberID": userID}).Decode(&item)
	if err == mongo.ErrNoDocuments {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &item, nil
}

// ListItemFolders 從 Item collection 查詢所有資料夾類型 items，用於建構 PathResolver
func (m *MongoReader) ListItemFolders(ctx context.Context, userID string) ([]*model.Item, error) {
	cur, err := m.itemsCol().Find(ctx, bson.M{
		"itemType": bson.M{"$in": []string{
			model.ItemTypeFolder, model.ItemTypeNoteFolder,
			model.ItemTypeCardFolder, model.ItemTypeChartFolder, model.ItemTypeTodoFolder,
		}},
		"fields.memberID": userID,
	})
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []*model.Item
	for cur.Next(ctx) {
		var item model.Item
		if err := cur.Decode(&item); err != nil {
			continue
		}
		out = append(out, &item)
	}
	return out, cur.Err()
}

// GetLatestUSN 從 Item collection 取得用戶最大 USN
func (m *MongoReader) GetLatestUSN(ctx context.Context, userID string) (int, error) {
	opts := options.FindOne().
		SetSort(bson.D{{Key: "fields.usn", Value: -1}}).
		SetProjection(bson.M{"fields.usn": 1})
	var row struct {
		Fields struct {
			Usn int `bson:"usn"`
		} `bson:"fields"`
	}
	latestItemUSN := 0
	err := m.itemsCol().FindOne(ctx, bson.M{"fields.memberID": userID}, opts).Decode(&row)
	if err != nil && err != mongo.ErrNoDocuments {
		return 0, err
	}
	if err == nil {
		latestItemUSN = row.Fields.Usn
	}

	var delRow struct {
		USN int `bson:"usn"`
	}
	delErr := m.db.Collection("ItemDeletionLog").FindOne(ctx,
		bson.M{"memberID": userID},
		options.FindOne().SetSort(bson.D{{Key: "usn", Value: -1}}).SetProjection(bson.M{"usn": 1}),
	).Decode(&delRow)
	if delErr != nil && delErr != mongo.ErrNoDocuments {
		return 0, delErr
	}
	if delRow.USN > latestItemUSN {
		return delRow.USN, nil
	}
	return latestItemUSN, nil
}

// GetChangesAfterUSN 從 Item collection 回傳大於指定 USN 的變更（兜底用途）。
func (m *MongoReader) GetChangesAfterUSN(ctx context.Context, userID string, afterUSN int) ([]vaultsync.SyncEvent, error) {
	cur, err := m.itemsCol().Find(ctx, bson.M{
		"fields.memberID": userID,
		"fields.usn":      bson.M{"$gt": afterUSN},
	})
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	type usnEvent struct {
		usn   int
		event vaultsync.SyncEvent
	}
	ts := time.Now().UnixMilli()
	var events []usnEvent
	for cur.Next(ctx) {
		var row struct {
			ID     string `bson:"_id"`
			Fields struct {
				Usn int `bson:"usn"`
			} `bson:"fields"`
		}
		if err := cur.Decode(&row); err != nil || row.ID == "" {
			continue
		}
		events = append(events, usnEvent{
			usn: row.Fields.Usn,
			event: vaultsync.SyncEvent{
				Collection: "item",
				UserID:     userID,
				DocID:      row.ID,
				Action:     "update",
				Timestamp:  ts,
			},
		})
	}
	if err := cur.Err(); err != nil {
		return nil, err
	}

	delCur, err := m.db.Collection("ItemDeletionLog").Find(ctx, bson.M{
		"memberID": userID,
		"usn":      bson.M{"$gt": afterUSN},
	})
	if err != nil {
		return nil, err
	}
	defer delCur.Close(ctx)
	for delCur.Next(ctx) {
		var row struct {
			ItemID string `bson:"itemID"`
			USN    int    `bson:"usn"`
		}
		if err := delCur.Decode(&row); err != nil || row.ItemID == "" {
			continue
		}
		events = append(events, usnEvent{
			usn: row.USN,
			event: vaultsync.SyncEvent{
				Collection: "item",
				UserID:     userID,
				DocID:      row.ItemID,
				Action:     "delete",
				Timestamp:  ts,
			},
		})
	}
	if err := delCur.Err(); err != nil {
		return nil, err
	}

	sort.Slice(events, func(i, j int) bool {
		if events[i].usn == events[j].usn {
			return events[i].event.DocID < events[j].event.DocID
		}
		return events[i].usn < events[j].usn
	})
	out := make([]vaultsync.SyncEvent, 0, len(events))
	for _, e := range events {
		out = append(out, e.event)
	}
	return out, nil
}

// GetDocUSN 從 Item collection 查詢文件當前 USN（衝突判定用）。
// 文件不存在時回傳 -1。
func (m *MongoReader) GetDocUSN(ctx context.Context, userID, collection, docID string) (int, error) {
	var row struct {
		Fields struct {
			Usn int `bson:"usn"`
		} `bson:"fields"`
	}
	err := m.itemsCol().FindOne(ctx, bson.M{"_id": docID, "fields.memberID": userID},
		options.FindOne().SetProjection(bson.M{"fields.usn": 1}),
	).Decode(&row)
	if err == mongo.ErrNoDocuments {
		return -1, nil
	}
	if err != nil {
		return 0, err
	}
	return row.Fields.Usn, nil
}

// ListAllItems 從 Item collection 取得用戶所有 items（全量匯出用）
func (m *MongoReader) ListAllItems(ctx context.Context, userID string) ([]*model.Item, error) {
	cur, err := m.itemsCol().Find(ctx, bson.M{"fields.memberID": userID})
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []*model.Item
	for cur.Next(ctx) {
		var item model.Item
		if err := cur.Decode(&item); err != nil {
			continue
		}
		out = append(out, &item)
	}
	return out, cur.Err()
}

func (m *MongoReader) ListActiveUsers(ctx context.Context) ([]string, error) {
	res, err := m.itemsCol().Distinct(ctx, "fields.memberID", bson.M{})
	if err != nil {
		return nil, err
	}
	users := make([]string, 0, len(res))
	for _, v := range res {
		if s, ok := v.(string); ok && s != "" {
			users = append(users, s)
		}
	}
	return users, nil
}
