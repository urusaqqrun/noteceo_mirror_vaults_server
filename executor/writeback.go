package executor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"strconv"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/urusaqqrun/vault-mirror-service/mirror"
)

// Doc 通用文件表示
type Doc = map[string]interface{}

// DataWriter 資料庫寫入能力（由 database 層實作）
type DataWriter interface {
	UpsertFolder(ctx context.Context, userID string, doc Doc) error
	UpsertNote(ctx context.Context, userID string, doc Doc) error
	UpsertCard(ctx context.Context, userID string, doc Doc) error
	UpsertChart(ctx context.Context, userID string, doc Doc) error
	UpsertItem(ctx context.Context, userID string, doc Doc) error
	DeleteItemDoc(ctx context.Context, userID, docID string, usn int) error
	DeleteDocument(ctx context.Context, userID, collection, docID string, usn int) error
}

// USNReader 查詢文件當前版本號（衝突判定用）
type USNReader interface {
	GetDocUSN(ctx context.Context, userID, collection, docID string) (int, error)
}

// USNIncrementer 遞增用戶版本號（回寫時為每個文件取得新版本）
type USNIncrementer interface {
	IncrementUSN(ctx context.Context, userID string) (int, error)
}

// USNSyncer 回寫完成後的清理動作（PostgreSQL 模式下為 no-op）
type USNSyncer interface {
	SyncUserUSN(ctx context.Context, userID string) error
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

// WriteBack 將 ImportEntry 清單寫回資料庫，使用 errgroup 並行處理（上限 8）。
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

			var err error

			if e.Action == mirror.ImportActionDelete {
				delUSN := 0
				if usnInc != nil {
					usn, usnErr := usnInc.IncrementUSN(gCtx, userID)
					if usnErr != nil {
						log.Printf("[WriteBack] IncrementUSN error: %v", usnErr)
						atomic.AddInt64(&errors, 1)
						return nil
					}
					delUSN = usn
				}
				err = deleteEntry(gCtx, writer, userID, e, delUSN)
				if err == nil {
					atomic.AddInt64(&deleted, 1)
				}
			} else {
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
				err = upsertEntry(gCtx, writer, userID, e, newUSN)
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
		Skipped: int(skipped),
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
		doc := folderMetaToDoc(e.FolderMeta)
		if newUSN > 0 {
			doc["usn"] = newUSN
		}
		ensureDocID(doc, e.Action)
		return w.UpsertFolder(ctx, userID, doc)
	case "note":
		if e.NoteMeta == nil {
			return fmt.Errorf("note meta is nil")
		}
		doc := noteMetaToDoc(e.NoteMeta, e.NoteBody)
		if newUSN > 0 {
			doc["usn"] = newUSN
		}
		ensureDocID(doc, e.Action)
		return w.UpsertNote(ctx, userID, doc)
	case "card":
		if e.CardMeta == nil {
			return fmt.Errorf("card meta is nil")
		}
		doc := cardMetaToDoc(e.CardMeta)
		if newUSN > 0 {
			doc["usn"] = newUSN
		}
		ensureDocID(doc, e.Action)
		return w.UpsertCard(ctx, userID, doc)
	case "chart":
		if e.CardMeta == nil {
			return fmt.Errorf("chart meta is nil")
		}
		doc := chartMetaToDoc(e.CardMeta)
		if newUSN > 0 {
			doc["usn"] = newUSN
		}
		ensureDocID(doc, e.Action)
		return w.UpsertChart(ctx, userID, doc)
	default:
		return fmt.Errorf("unknown collection: %s", e.Collection)
	}
}

func upsertItemEntry(ctx context.Context, w DataWriter, userID string, e mirror.ImportEntry, newUSN int) error {
	var doc Doc
	switch {
	case e.ItemData != nil:
		doc = itemDataToItemDoc(e.ItemData, newUSN)
	case e.FolderMeta != nil:
		doc = folderMetaToItemDoc(e.FolderMeta, e.ItemType, newUSN)
	case e.NoteMeta != nil:
		doc = noteMetaToItemDoc(e.NoteMeta, e.NoteBody, newUSN)
	case e.CardMeta != nil:
		doc = cardMetaToItemDoc(e.CardMeta, e.ItemType, newUSN)
	default:
		return fmt.Errorf("item entry has no meta data")
	}
	ensureDocID(doc, e.Action)
	return w.UpsertItem(ctx, userID, doc)
}

func folderMetaToItemDoc(m *mirror.FolderMeta, itemType string, usn int) Doc {
	fields := Doc{
		"name":    m.FolderName,
		"noteNum": m.NoteNum,
		"isTemp":  m.IsTemp,
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
	return Doc{
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

func noteMetaToItemDoc(m *mirror.NoteMeta, body string, usn int) Doc {
	itemType := "NOTE"
	if m.Type == "TODO" {
		itemType = "TODO"
	}
	fields := Doc{
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
			log.Printf("[noteMetaToItemDoc] MD→HTML conversion error: %v, using raw body", err)
			fields["content"] = body
		} else {
			fields["content"] = html
		}
	}
	return Doc{
		"_id":      m.ID,
		"itemType": itemType,
		"fields":   fields,
	}
}

func cardMetaToItemDoc(m *mirror.CardMeta, itemType string, usn int) Doc {
	if itemType == "" {
		itemType = "CARD"
	}
	fields := Doc{
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
	return Doc{
		"_id":      m.ID,
		"itemType": itemType,
		"fields":   fields,
	}
}

func itemDataToItemDoc(d *mirror.ItemMirrorData, usn int) Doc {
	fields := Doc{}
	for k, v := range d.Fields {
		fields[k] = v
	}
	if usn > 0 {
		fields["usn"] = usn
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

func parseTimestamp(s string) interface{} {
	if v, err := strconv.ParseInt(s, 10, 64); err == nil {
		return v
	}
	return s
}

func deleteEntry(ctx context.Context, w DataWriter, userID string, e mirror.ImportEntry, usn int) error {
	docID := e.DocID
	if docID == "" {
		if e.ItemData != nil {
			docID = e.ItemData.ID
		} else {
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

func folderMetaToDoc(m *mirror.FolderMeta) Doc {
	doc := Doc{
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

func noteMetaToDoc(m *mirror.NoteMeta, body string) Doc {
	tags := m.Tags
	if tags == nil {
		tags = []string{}
	}
	doc := Doc{
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
			log.Printf("[noteMetaToDoc] MD→HTML conversion error: %v, using raw body", err)
			doc["content"] = body
		} else {
			doc["content"] = html
		}
	}
	return doc
}

func cardMetaToDoc(m *mirror.CardMeta) Doc {
	doc := Doc{
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

func chartMetaToDoc(m *mirror.CardMeta) Doc {
	doc := Doc{
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

