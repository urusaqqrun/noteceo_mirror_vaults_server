package executor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/urusaqqrun/vault-mirror-service/mirror"
)

// Doc 通用文件表示
type Doc = map[string]interface{}

// DataWriter 資料庫寫入能力（由 database 層實作）
type DataWriter interface {
	UpsertItem(ctx context.Context, userID string, doc Doc) error
	DeleteItemDoc(ctx context.Context, userID, docID string, version int) error
	IncrementUSN(ctx context.Context, userID string) (int, error)
}

// WriteBackResult 回寫統計
type WriteBackResult struct {
	Created int
	Updated int
	Moved   int
	Deleted int
	Errors  int
}

// WriteBack 將 ImportEntry 清單寫回資料庫，使用 errgroup 並行處理（上限 8）。
func WriteBack(ctx context.Context, writer DataWriter, userID string, entries []mirror.ImportEntry) WriteBackResult {
	var created, updated, moved, deleted, errors int64

	g, gCtx := errgroup.WithContext(ctx)
	g.SetLimit(8)

	for _, e := range entries {
		e := e
		g.Go(func() error {
			if gCtx.Err() != nil {
				return nil
			}

			version, vErr := writer.IncrementUSN(gCtx, userID)
			if vErr != nil {
				log.Printf("[WriteBack] IncrementUSN error: %v", vErr)
				atomic.AddInt64(&errors, 1)
				return nil
			}

			var err error
			if e.Action == mirror.ImportActionDelete {
				err = deleteEntry(gCtx, writer, userID, e, version)
				if err == nil {
					atomic.AddInt64(&deleted, 1)
				}
			} else {
				err = upsertEntry(gCtx, writer, userID, e, version)
				if err == nil {
					switch e.Action {
					case mirror.ImportActionCreate:
						atomic.AddInt64(&created, 1)
					case mirror.ImportActionUpdate:
						atomic.AddInt64(&updated, 1)
					case mirror.ImportActionMove:
						atomic.AddInt64(&moved, 1)
					}
				}
			}

			if err != nil {
				log.Printf("[WriteBack] %s %s error: %v", e.Action, e.Path, err)
				atomic.AddInt64(&errors, 1)
			}
			return nil
		})
	}

	g.Wait()

	return WriteBackResult{
		Created: int(created),
		Updated: int(updated),
		Moved:   int(moved),
		Deleted: int(deleted),
		Errors:  int(errors),
	}
}

func resolveDocID(e mirror.ImportEntry) string {
	if e.DocID != "" {
		return e.DocID
	}
	if e.ItemData != nil {
		return e.ItemData.ID
	}
	return ""
}

// upsertEntry 統一走 upsertItemEntry，不分 collection
func upsertEntry(ctx context.Context, w DataWriter, userID string, e mirror.ImportEntry, newUSN int) error {
	return upsertItemEntry(ctx, w, userID, e, newUSN)
}

func upsertItemEntry(ctx context.Context, w DataWriter, userID string, e mirror.ImportEntry, newUSN int) error {
	if e.ItemData == nil {
		return fmt.Errorf("item entry has no ItemData")
	}
	doc := itemDataToItemDoc(e.ItemData, newUSN)
	ensureDocID(doc, e.Action)
	return w.UpsertItem(ctx, userID, doc)
}

func itemDataToItemDoc(d *mirror.ItemMirrorData, version int) Doc {
	fields := Doc{}
	for k, v := range d.Fields {
		fields[k] = v
	}
	fields["updatedAt"] = time.Now().UnixMilli()
	return Doc{
		"_id":      d.ID,
		"name":     d.Name,
		"itemType": d.ItemType,
		"fields":   fields,
	}
}

// ensureDocID 確保 doc 有 _id；AI 新建的文件可能沒有 ID，自動生成
func ensureDocID(doc Doc, action mirror.ImportAction) {
	if action != mirror.ImportActionCreate {
		return
	}
	id, ok := doc["_id"].(string)
	if !ok || id == "" {
		newID := generateID()
		doc["_id"] = newID
		log.Printf("[WriteBack] auto-generated _id=%s for new document", newID)
	}
}

// generateID 產生 24 字元 hex ID
func generateID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func deleteEntry(ctx context.Context, w DataWriter, userID string, e mirror.ImportEntry, usn int) error {
	docID := e.DocID
	if docID == "" && e.ItemData != nil {
		docID = e.ItemData.ID
	}
	if docID == "" {
		log.Printf("[WriteBack] skip delete %s: no docID available", e.Path)
		return nil
	}
	return w.DeleteItemDoc(ctx, userID, docID, usn)
}
