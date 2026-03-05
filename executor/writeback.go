package executor

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"

	"github.com/urusaqqrun/vault-mirror-service/mirror"
)

// DataWriter MongoDB 寫入能力（由 database.MongoReader 實作）
type DataWriter interface {
	UpsertFolder(ctx context.Context, userID string, doc bson.M) error
	UpsertNote(ctx context.Context, userID string, doc bson.M) error
	UpsertCard(ctx context.Context, userID string, doc bson.M) error
	UpsertChart(ctx context.Context, userID string, doc bson.M) error
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
	Created  int
	Updated  int
	Moved    int
	Deleted  int
	Skipped  int
	Errors   int
}

// WriteBack 將 ImportEntry 清單寫回 MongoDB，並透過 USN 衝突判定決定是否套用。
// usnReader 為 nil 時跳過衝突判定（全部套用）。
// usnInc 為 nil 時不遞增 USN（保留原始值）。
func WriteBack(ctx context.Context, writer DataWriter, usnReader USNReader, usnInc USNIncrementer, userID string, entries []mirror.ImportEntry, aiStartUSN int) WriteBackResult {
	var result WriteBackResult
	for _, e := range entries {
		if ctx.Err() != nil {
			break
		}

		// 衝突判定：查詢 DB 當前 USN，若用戶在 AI 執行期間修改了該文件則跳過回寫。
		// 判定規則：dbUSN > aiStartUSN 表示用戶在 AI 執行期間更新了此文件，跳過以保留用戶修改。
		// Create 永遠套用（新檔案不存在衝突）。
		if usnReader != nil && e.Action != mirror.ImportActionCreate {
			docID := resolveDocID(e)
			if docID != "" {
				dbUSN, usnErr := usnReader.GetDocUSN(ctx, userID, e.Collection, docID)
				if usnErr != nil {
					log.Printf("[WriteBack] GetDocUSN error (%s/%s): %v", e.Collection, docID, usnErr)
				} else if dbUSN > aiStartUSN {
					log.Printf("[WriteBack] conflict skip %s %s: user modified during AI task (dbUSN=%d > aiStartUSN=%d)",
						e.Action, e.Path, dbUSN, aiStartUSN)
					result.Skipped++
					continue
				}
			}
		}

		// 遞增 USN：每個寫入操作取得新 USN，確保客戶端能透過 USN polling 偵測到 AI 修改
		newUSN := 0
		if usnInc != nil {
			usn, usnErr := usnInc.IncrementUSN(ctx, userID)
			if usnErr != nil {
				log.Printf("[WriteBack] IncrementUSN error: %v", usnErr)
				result.Errors++
				continue
			}
			newUSN = usn
		}

		var err error
		switch e.Action {
		case mirror.ImportActionCreate:
			err = upsertEntry(ctx, writer, userID, e, newUSN)
			if err == nil {
				result.Created++
			}
		case mirror.ImportActionUpdate:
			err = upsertEntry(ctx, writer, userID, e, newUSN)
			if err == nil {
				result.Updated++
			}
		case mirror.ImportActionMove:
			err = upsertEntry(ctx, writer, userID, e, newUSN)
			if err == nil {
				result.Moved++
			}
		case mirror.ImportActionDelete:
			err = deleteEntry(ctx, writer, userID, e, newUSN)
			if err == nil {
				result.Deleted++
			}
		}
		if err != nil {
			log.Printf("[WriteBack] %s %s error: %v", e.Action, e.Path, err)
			result.Errors++
		}
	}
	return result
}

// resolveDocID 從 ImportEntry 取得文件 ID（用於衝突判定查詢）
func resolveDocID(e mirror.ImportEntry) string {
	if e.DocID != "" {
		return e.DocID
	}
	switch e.Collection {
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
	case "folder":
		if e.FolderMeta == nil {
			return fmt.Errorf("folder meta is nil")
		}
		doc := folderMetaToBson(e.FolderMeta)
		if newUSN > 0 {
			doc["usn"] = newUSN
		}
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
		return w.UpsertNote(ctx, userID, doc)
	case "card":
		if e.CardMeta == nil {
			return fmt.Errorf("card meta is nil")
		}
		doc := cardMetaToBson(e.CardMeta)
		if newUSN > 0 {
			doc["usn"] = newUSN
		}
		return w.UpsertCard(ctx, userID, doc)
	case "chart":
		if e.CardMeta == nil {
			return fmt.Errorf("chart meta is nil")
		}
		doc := chartMetaToBson(e.CardMeta)
		if newUSN > 0 {
			doc["usn"] = newUSN
		}
		return w.UpsertChart(ctx, userID, doc)
	default:
		return fmt.Errorf("unknown collection: %s", e.Collection)
	}
}

func deleteEntry(ctx context.Context, w DataWriter, userID string, e mirror.ImportEntry, usn int) error {
	docID := e.DocID
	if docID == "" {
		switch e.Collection {
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
		"_id":      m.ID,
		"parentID": m.ParentID,
		"name":     m.Name,
		"usn":      m.USN,
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
		"_id":      m.ID,
		"parentID": m.ParentID,
		"name":     m.Name,
		"usn":      m.USN,
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
