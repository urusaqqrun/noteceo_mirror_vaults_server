package executor

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"golang.org/x/sync/errgroup"

	"github.com/urusaqqrun/vault-mirror-service/mirror"
)

// DataWriter MongoDB 寫入能力（由 database.MongoReader 實作）
type DataWriter interface {
	UpsertFolder(ctx context.Context, userID string, doc bson.M) error
	UpsertNote(ctx context.Context, userID string, doc bson.M) error
	UpsertCard(ctx context.Context, userID string, doc bson.M) error
	UpsertChart(ctx context.Context, userID string, doc bson.M) error
	UpsertItem(ctx context.Context, userID string, doc bson.M) error
	DeleteItemDoc(ctx context.Context, userID, docID string, usn int) error
	DeleteDocument(ctx context.Context, userID, collection, docID string, usn int) error
}

// USNReader 查詢文件當前 USN（衝突判定用）
type USNReader interface {
	GetDocUSN(ctx context.Context, userID, collection, docID string) (int, error)
}

// USNIncrementer 遞增用戶 USN（回寫時為每個文件取得新 USN）
type USNIncrementer interface {
	IncrementUSN(ctx context.Context, userID string) (int, error)
}

// WriteBackResult 回寫統計
type WriteBackResult struct {
	Created int
	Updated int
	Moved   int
	Deleted int
	Skipped int
	Errors  int
}

// WriteBack 將 ImportEntry 清單寫回 MongoDB，使用 errgroup 並行處理（上限 8）。
// usnReader 為 nil 時跳過衝突判定（全部套用）。
// usnInc 為 nil 時不遞增 USN（保留原始值）。
func WriteBack(ctx context.Context, writer DataWriter, usnReader USNReader, usnInc USNIncrementer, userID string, entries []mirror.ImportEntry, aiStartUSN int) WriteBackResult {
	var created, updated, moved, deleted, skipped, errors int64

	g, gCtx := errgroup.WithContext(ctx)
	g.SetLimit(8)

	for _, e := range entries {
		e := e
		g.Go(func() error {
			if gCtx.Err() != nil {
				return nil
			}

			// 衝突判定
			if usnReader != nil && e.Action != mirror.ImportActionCreate {
				docID := resolveDocID(e)
				if docID != "" {
					dbUSN, usnErr := usnReader.GetDocUSN(gCtx, userID, e.Collection, docID)
					if usnErr != nil {
						log.Printf("[WriteBack] GetDocUSN error (%s/%s): %v", e.Collection, docID, usnErr)
					} else if dbUSN > aiStartUSN {
						log.Printf("[WriteBack] conflict skip %s %s: user modified during AI task (dbUSN=%d > aiStartUSN=%d)",
							e.Action, e.Path, dbUSN, aiStartUSN)
						atomic.AddInt64(&skipped, 1)
						return nil
					}
				}
			}

			// USN 遞增（Redis INCR 本身是原子操作，可安全並行呼叫）
			newUSN := 0
			if usnInc != nil {
				usn, usnErr := usnInc.IncrementUSN(gCtx, userID)
				if usnErr != nil {
					log.Printf("[WriteBack] IncrementUSN error: %v", usnErr)
					atomic.AddInt64(&errors, 1)
					return nil
				}
				newUSN = usn
			}

			var err error
			switch e.Action {
			case mirror.ImportActionCreate:
				err = upsertEntry(gCtx, writer, userID, e, newUSN)
				if err == nil {
					atomic.AddInt64(&created, 1)
				}
			case mirror.ImportActionUpdate:
				err = upsertEntry(gCtx, writer, userID, e, newUSN)
				if err == nil {
					atomic.AddInt64(&updated, 1)
				}
			case mirror.ImportActionMove:
				err = upsertEntry(gCtx, writer, userID, e, newUSN)
				if err == nil {
					atomic.AddInt64(&moved, 1)
				}
			case mirror.ImportActionDelete:
				err = deleteEntry(gCtx, writer, userID, e, newUSN)
				if err == nil {
					atomic.AddInt64(&deleted, 1)
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
		Skipped: int(skipped),
		Errors:  int(errors),
	}
}

// resolveDocID 從 ImportEntry 取得文件 ID（用於衝突判定查詢）
func resolveDocID(e mirror.ImportEntry) string {
	if e.DocID != "" {
		return e.DocID
	}
	switch e.Collection {
	case "item":
		if e.FolderMeta != nil {
			return e.FolderMeta.ID
		}
		if e.NoteMeta != nil {
			return e.NoteMeta.ID
		}
		if e.CardMeta != nil {
			return e.CardMeta.ID
		}
	case "folder":
		if e.FolderMeta != nil {
			return e.FolderMeta.ID
		}
	case "note":
		if e.NoteMeta != nil {
			return e.NoteMeta.ID
		}
	case "card", "chart":
		if e.CardMeta != nil {
			return e.CardMeta.ID
		}
	}
	return ""
}

func upsertEntry(ctx context.Context, w DataWriter, userID string, e mirror.ImportEntry, newUSN int) error {
	switch e.Collection {
	case "item":
		return upsertItemEntry(ctx, w, userID, e, newUSN)
	case "folder":
		if e.FolderMeta == nil {
			return fmt.Errorf("folder meta is nil")
		}
		doc := folderMetaToBson(e.FolderMeta)
		if newUSN > 0 {
			doc["usn"] = newUSN
		}
		ensureDocID(doc, e.Action)
		return w.UpsertFolder(ctx, userID, doc)
	case "note":
		if e.NoteMeta == nil {
			return fmt.Errorf("note meta is nil")
		}
		doc := noteMetaToBson(e.NoteMeta, e.NoteBody)
		if newUSN > 0 {
			doc["usn"] = newUSN
		}
		if e.Action == mirror.ImportActionMove {
			newParent := parentFolderFromPath(e.Path)
			if newParent != "" {
				doc["parentID"] = newParent
			}
		}
		ensureDocID(doc, e.Action)
		return w.UpsertNote(ctx, userID, doc)
	case "card":
		if e.CardMeta == nil {
			return fmt.Errorf("card meta is nil")
		}
		doc := cardMetaToBson(e.CardMeta)
		if newUSN > 0 {
			doc["usn"] = newUSN
		}
		ensureDocID(doc, e.Action)
		return w.UpsertCard(ctx, userID, doc)
	case "chart":
		if e.CardMeta == nil {
			return fmt.Errorf("chart meta is nil")
		}
		doc := chartMetaToBson(e.CardMeta)
		if newUSN > 0 {
			doc["usn"] = newUSN
		}
		ensureDocID(doc, e.Action)
		return w.UpsertChart(ctx, userID, doc)
	default:
		return fmt.Errorf("unknown collection: %s", e.Collection)
	}
}

// upsertItemEntry 將 meta 轉換為 Item collection 格式後 upsert
func upsertItemEntry(ctx context.Context, w DataWriter, userID string, e mirror.ImportEntry, newUSN int) error {
	var doc bson.M
	switch {
	case e.FolderMeta != nil:
		doc = folderMetaToItemBson(e.FolderMeta, e.ItemType, newUSN)
	case e.NoteMeta != nil:
		doc = noteMetaToItemBson(e.NoteMeta, e.NoteBody, newUSN)
	case e.CardMeta != nil:
		doc = cardMetaToItemBson(e.CardMeta, e.ItemType, newUSN)
	default:
		return fmt.Errorf("item entry has no meta data")
	}
	ensureDocID(doc, e.Action)
	return w.UpsertItem(ctx, userID, doc)
}

// folderMetaToItemBson 將 FolderMeta 轉為 Item collection 的 bson.M
func folderMetaToItemBson(m *mirror.FolderMeta, itemType string, usn int) bson.M {
	fields := bson.M{
		"memberID": m.MemberID,
		"name":     m.FolderName,
		"noteNum":  m.NoteNum,
		"isTemp":   m.IsTemp,
	}
	if usn > 0 {
		fields["usn"] = usn
	} else {
		fields["usn"] = m.USN
	}
	if m.Type != nil {
		fields["folderType"] = *m.Type
	}
	if m.ParentID != nil {
		fields["parentID"] = *m.ParentID
	}
	if m.OrderAt != nil {
		fields["orderAt"] = *m.OrderAt
	}
	if m.Icon != nil {
		fields["icon"] = *m.Icon
	}
	if m.CreatedAt != "" {
		fields["createdAt"] = parseTimestamp(m.CreatedAt)
	}
	if m.UpdatedAt != "" {
		fields["updatedAt"] = parseTimestamp(m.UpdatedAt)
	}
	if m.FolderSummary != nil {
		fields["folderSummary"] = *m.FolderSummary
	}
	if m.AiFolderName != nil {
		fields["aiFolderName"] = *m.AiFolderName
	}
	if m.AiFolderSummary != nil {
		fields["aiFolderSummary"] = *m.AiFolderSummary
	}
	if m.AiInstruction != nil {
		fields["aiInstruction"] = *m.AiInstruction
	}
	fields["autoUpdateSummary"] = m.AutoUpdateSummary
	if len(m.Indexes) > 0 {
		fields["indexes"] = m.Indexes
	}
	if len(m.IsSummarizedNoteIds) > 0 {
		fields["isSummarizedNoteIds"] = m.IsSummarizedNoteIds
	}
	if m.TemplateHTML != nil {
		fields["templateHtml"] = *m.TemplateHTML
	}
	if m.TemplateCSS != nil {
		fields["templateCss"] = *m.TemplateCSS
	}
	if m.UIPrompt != nil {
		fields["uiPrompt"] = *m.UIPrompt
	}
	fields["isShared"] = m.IsShared
	fields["searchable"] = m.Searchable
	fields["allowContribute"] = m.AllowContribute
	if len(m.Fields) > 0 {
		fields["fields"] = m.Fields
	}
	if len(m.TemplateHistory) > 0 {
		fields["templateHistory"] = m.TemplateHistory
	}
	if len(m.Sharers) > 0 {
		fields["sharers"] = m.Sharers
	}
	if m.ChartKind != nil {
		fields["chartKind"] = *m.ChartKind
	}
	return bson.M{
		"_id":      m.ID,
		"itemType": normalizeFolderItemType(itemType),
		"fields":   fields,
	}
}

func normalizeFolderItemType(itemType string) string {
	switch itemType {
	case "FOLDER", "NOTE_FOLDER", "TODO_FOLDER", "CARD_FOLDER", "CHART_FOLDER":
		return itemType
	default:
		return "FOLDER"
	}
}

// noteMetaToItemBson 將 NoteMeta + body 轉為 Item collection 的 bson.M
func noteMetaToItemBson(m *mirror.NoteMeta, body string, usn int) bson.M {
	itemType := "NOTE"
	if m.Type == "TODO" {
		itemType = "TODO"
	}
	fields := bson.M{
		"title":     m.Title,
		"name":      m.Title,
		"parentID":  m.ParentID,
		"updatedAt": time.Now().UnixMilli(),
		"isNew":     m.IsNew,
	}
	if usn > 0 {
		fields["usn"] = usn
	} else {
		fields["usn"] = m.USN
	}
	tags := m.Tags
	if tags == nil {
		tags = []string{}
	}
	fields["tags"] = tags
	if m.CreatedAt != "" {
		fields["createdAt"] = parseTimestamp(m.CreatedAt)
	}
	if m.FolderID != "" {
		fields["folderID"] = m.FolderID
	}
	if m.OrderAt != "" {
		fields["orderAt"] = m.OrderAt
	}
	if m.Status != "" {
		fields["status"] = m.Status
	}
	if m.AiTitle != "" {
		fields["aiTitle"] = m.AiTitle
	}
	if m.AiTags != nil {
		fields["aiTags"] = m.AiTags
	}
	if m.ImgURLs != nil {
		fields["imgURLs"] = m.ImgURLs
	}
	if body != "" {
		html, err := mirror.MarkdownToHTML(body)
		if err != nil {
			log.Printf("[noteMetaToItemBson] MD→HTML conversion error: %v, using raw body", err)
			fields["content"] = body
		} else {
			fields["content"] = html
		}
	}
	return bson.M{
		"_id":      m.ID,
		"itemType": itemType,
		"fields":   fields,
	}
}

// cardMetaToItemBson 將 CardMeta 轉為 Item collection 的 bson.M
func cardMetaToItemBson(m *mirror.CardMeta, itemType string, usn int) bson.M {
	if itemType == "" {
		itemType = "CARD"
	}
	fields := bson.M{
		"parentID": m.ParentID,
		"name":     m.Name,
	}
	if usn > 0 {
		fields["usn"] = usn
	} else {
		fields["usn"] = m.USN
	}
	if itemType == "CHART" {
		if m.Fields != nil {
			fields["data"] = *m.Fields
		}
	} else {
		if m.Fields != nil {
			fields["fields"] = *m.Fields
		}
		if m.Reviews != nil {
			fields["reviews"] = *m.Reviews
		}
		if m.ContributorID != nil {
			fields["contributorId"] = *m.ContributorID
		}
		if m.Coordinates != nil {
			fields["coordinates"] = *m.Coordinates
		}
	}
	if m.OrderAt != nil {
		fields["orderAt"] = *m.OrderAt
	}
	if m.CreatedAt != "" {
		fields["createdAt"] = parseTimestamp(m.CreatedAt)
	}
	if m.UpdatedAt != "" {
		fields["updatedAt"] = parseTimestamp(m.UpdatedAt)
	}
	fields["isDeleted"] = m.IsDeleted
	return bson.M{
		"_id":      m.ID,
		"itemType": itemType,
		"fields":   fields,
	}
}

// ensureDocID 確保 BSON doc 有 _id；AI 新建的文件可能沒有 ID，自動生成 ObjectID
func ensureDocID(doc bson.M, action mirror.ImportAction) {
	if action != mirror.ImportActionCreate {
		return
	}
	id, ok := doc["_id"].(string)
	if !ok || id == "" {
		newID := primitive.NewObjectID().Hex()
		doc["_id"] = newID
		log.Printf("[WriteBack] auto-generated _id=%s for new document", newID)
	}
}

// parseTimestamp 嘗試將字串解析為 int64 毫秒時間戳，失敗時回傳原字串
func parseTimestamp(s string) interface{} {
	if v, err := strconv.ParseInt(s, 10, 64); err == nil {
		return v
	}
	return s
}

func deleteEntry(ctx context.Context, w DataWriter, userID string, e mirror.ImportEntry, usn int) error {
	docID := e.DocID
	if docID == "" {
		switch e.Collection {
		case "item":
			if e.FolderMeta != nil {
				docID = e.FolderMeta.ID
			} else if e.NoteMeta != nil {
				docID = e.NoteMeta.ID
			} else if e.CardMeta != nil {
				docID = e.CardMeta.ID
			}
		case "folder":
			if e.FolderMeta != nil {
				docID = e.FolderMeta.ID
			}
		case "note":
			if e.NoteMeta != nil {
				docID = e.NoteMeta.ID
			}
		case "card", "chart":
			if e.CardMeta != nil {
				docID = e.CardMeta.ID
			}
		}
	}
	if docID == "" {
		log.Printf("[WriteBack] skip delete %s: no docID available", e.Path)
		return nil
	}
	if e.Collection == "item" {
		return w.DeleteItemDoc(ctx, userID, docID, usn)
	}
	return w.DeleteDocument(ctx, userID, e.Collection, docID, usn)
}

func folderMetaToBson(m *mirror.FolderMeta) bson.M {
	doc := bson.M{
		"_id":        m.ID,
		"folderName": m.FolderName,
		"usn":        m.USN,
		"noteNum":    m.NoteNum,
		"isTemp":     m.IsTemp,
	}
	if m.Type != nil {
		doc["type"] = *m.Type
	}
	if m.ParentID != nil {
		doc["parentID"] = *m.ParentID
	}
	if m.OrderAt != nil {
		doc["orderAt"] = *m.OrderAt
	}
	if m.Icon != nil {
		doc["icon"] = *m.Icon
	}
	if m.CreatedAt != "" {
		doc["createdAt"] = m.CreatedAt
	}
	if m.UpdatedAt != "" {
		doc["updatedAt"] = m.UpdatedAt
	}
	if m.FolderSummary != nil {
		doc["folderSummary"] = *m.FolderSummary
	}
	if m.AiFolderName != nil {
		doc["aiFolderName"] = *m.AiFolderName
	}
	if m.AiFolderSummary != nil {
		doc["aiFolderSummary"] = *m.AiFolderSummary
	}
	if m.AiInstruction != nil {
		doc["aiInstruction"] = *m.AiInstruction
	}
	doc["autoUpdateSummary"] = m.AutoUpdateSummary
	if m.IsSummarizedNoteIds != nil {
		doc["isSummarizedNoteIds"] = m.IsSummarizedNoteIds
	}
	if len(m.Indexes) > 0 {
		doc["indexes"] = m.Indexes
	}
	if len(m.Fields) > 0 {
		doc["fields"] = m.Fields
	}
	if m.TemplateHTML != nil {
		doc["templateHtml"] = *m.TemplateHTML
	}
	if m.TemplateCSS != nil {
		doc["templateCss"] = *m.TemplateCSS
	}
	if m.UIPrompt != nil {
		doc["uiPrompt"] = *m.UIPrompt
	}
	if len(m.TemplateHistory) > 0 {
		doc["templateHistory"] = m.TemplateHistory
	}
	doc["isShared"] = m.IsShared
	doc["searchable"] = m.Searchable
	doc["allowContribute"] = m.AllowContribute
	if len(m.Sharers) > 0 {
		doc["sharers"] = m.Sharers
	}
	if m.ChartKind != nil {
		doc["chartKind"] = *m.ChartKind
	}
	return doc
}

func noteMetaToBson(m *mirror.NoteMeta, body string) bson.M {
	tags := m.Tags
	if tags == nil {
		tags = []string{}
	}
	doc := bson.M{
		"_id":      m.ID,
		"title":    m.Title,
		"parentID": m.ParentID,
		"usn":      m.USN,
		"tags":     tags,
		"updateAt": time.Now().UnixMilli(),
	}
	if m.CreatedAt != "" {
		if v, err := strconv.ParseInt(m.CreatedAt, 10, 64); err == nil {
			doc["createAt"] = v
		}
	}
	if m.FolderID != "" {
		doc["folderID"] = m.FolderID
	}
	if m.Type != "" {
		doc["_type"] = m.Type
	}
	if m.OrderAt != "" {
		doc["orderAt"] = m.OrderAt
	}
	if m.Status != "" {
		doc["status"] = m.Status
	}
	if m.AiTitle != "" {
		doc["aiTitle"] = m.AiTitle
	}
	if m.AiTags != nil {
		doc["aiTags"] = m.AiTags
	}
	if m.ImgURLs != nil {
		doc["imgURLs"] = m.ImgURLs
	}
	doc["isNew"] = m.IsNew
	if body != "" {
		html, err := mirror.MarkdownToHTML(body)
		if err != nil {
			log.Printf("[noteMetaToBson] MD→HTML conversion error: %v, using raw body", err)
			doc["content"] = body
		} else {
			doc["content"] = html
		}
	}
	return doc
}

func cardMetaToBson(m *mirror.CardMeta) bson.M {
	doc := bson.M{
		"_id":       m.ID,
		"parentID":  m.ParentID,
		"name":      m.Name,
		"usn":       m.USN,
		"isDeleted": m.IsDeleted,
	}
	if m.Fields != nil {
		doc["fields"] = *m.Fields
	}
	if m.Reviews != nil {
		doc["reviews"] = *m.Reviews
	}
	if m.OrderAt != nil {
		doc["orderAt"] = *m.OrderAt
	}
	if m.ContributorID != nil {
		doc["contributorId"] = *m.ContributorID
	}
	if m.Coordinates != nil {
		doc["coordinates"] = *m.Coordinates
	}
	if m.CreatedAt != "" {
		doc["createdAt"] = m.CreatedAt
	}
	if m.UpdatedAt != "" {
		doc["updatedAt"] = m.UpdatedAt
	}
	return doc
}

// chartMetaToBson Chart 專用：CardMeta.Fields 映射到 MongoDB 的 data 欄位（而非 fields）
func chartMetaToBson(m *mirror.CardMeta) bson.M {
	doc := bson.M{
		"_id":       m.ID,
		"parentID":  m.ParentID,
		"name":      m.Name,
		"usn":       m.USN,
		"isDeleted": m.IsDeleted,
	}
	if m.Fields != nil {
		doc["data"] = *m.Fields
	}
	if m.OrderAt != nil {
		doc["orderAt"] = *m.OrderAt
	}
	if m.CreatedAt != "" {
		doc["createdAt"] = m.CreatedAt
	}
	if m.UpdatedAt != "" {
		doc["updatedAt"] = m.UpdatedAt
	}
	return doc
}

// parentFolderFromPath 從 Vault 相對路徑推導父目錄 ID（需要事先建立路徑→ID 映射）
// 目前回傳空字串，由 NoteMeta.ParentID 欄位提供
func parentFolderFromPath(path string) string {
	_ = strings.Split(path, "/")
	return ""
}
