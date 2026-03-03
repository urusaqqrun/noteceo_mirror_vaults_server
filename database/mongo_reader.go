package database

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/urusaqqrun/vault-mirror-service/model"
	vaultsync "github.com/urusaqqrun/vault-mirror-service/sync"
)

// MongoReader 提供同步所需的 MongoDB 讀取能力。
type MongoReader struct {
	client *mongo.Client
	db     *mongo.Database
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

// GetLatestUSN 回傳用戶四個集合中的最大 USN（並行查詢）。
func (m *MongoReader) GetLatestUSN(ctx context.Context, userID string) (int, error) {
	cols := []*mongo.Collection{m.foldersCol(), m.notesCol(), m.cardsCol(), m.chartsCol()}
	results := make([]int, len(cols))
	var wg sync.WaitGroup
	wg.Add(len(cols))
	for i, col := range cols {
		go func(idx int, c *mongo.Collection) {
			defer wg.Done()
			opts := options.FindOne().SetSort(bson.D{{Key: "usn", Value: -1}})
			var row struct {
				Usn int `bson:"usn"`
			}
			if err := c.FindOne(ctx, bson.M{"memberID": userID}, opts).Decode(&row); err == nil {
				results[idx] = row.Usn
			}
		}(i, col)
	}
	wg.Wait()

	maxUSN := 0
	for _, v := range results {
		if v > maxUSN {
			maxUSN = v
		}
	}
	return maxUSN, nil
}

// GetChangesAfterUSN 回傳大於指定 USN 的變更（兜底用途，action 統一為 update）。
// 四個集合並行查詢以減少延遲。
func (m *MongoReader) GetChangesAfterUSN(ctx context.Context, userID string, afterUSN int) ([]vaultsync.SyncEvent, error) {
	type colTask struct {
		col  *mongo.Collection
		name string
	}
	tasks := []colTask{
		{m.foldersCol(), "folder"},
		{m.notesCol(), "note"},
		{m.cardsCol(), "card"},
		{m.chartsCol(), "chart"},
	}

	type result struct {
		events []vaultsync.SyncEvent
		err    error
	}
	results := make([]result, len(tasks))
	var wg sync.WaitGroup
	wg.Add(len(tasks))

	ts := time.Now().UnixMilli()
	for i, t := range tasks {
		go func(idx int, col *mongo.Collection, collection string) {
			defer wg.Done()
			cur, err := col.Find(ctx, bson.M{"memberID": userID, "usn": bson.M{"$gt": afterUSN}})
			if err != nil {
				results[idx] = result{err: err}
				return
			}
			defer cur.Close(ctx)
			var evts []vaultsync.SyncEvent
			for cur.Next(ctx) {
				var row struct {
					ID  string `bson:"_id"`
					Usn int    `bson:"usn"`
				}
				if err := cur.Decode(&row); err != nil || row.ID == "" {
					continue
				}
				evts = append(evts, vaultsync.SyncEvent{
					Collection: collection,
					UserID:     userID,
					DocID:      row.ID,
					Action:     "update",
					Timestamp:  ts,
				})
			}
			results[idx] = result{events: evts, err: cur.Err()}
		}(i, t.col, t.name)
	}
	wg.Wait()

	var allEvents []vaultsync.SyncEvent
	for _, r := range results {
		if r.err != nil {
			return nil, r.err
		}
		allEvents = append(allEvents, r.events...)
	}
	return allEvents, nil
}

func (m *MongoReader) ListActiveUsers(ctx context.Context) ([]string, error) {
	res, err := m.foldersCol().Distinct(ctx, "memberID", bson.M{})
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
