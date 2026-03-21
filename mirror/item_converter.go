package mirror

import (
	"encoding/json"
	"log"
	"strings"

	"github.com/urusaqqrun/vault-mirror-service/model"
)

// ItemToNoteMeta 將 Item（NOTE/TODO）轉換為 NoteMeta + HTML content
func ItemToNoteMeta(item *model.Item) (NoteMeta, string) {
	f := item.Fields
	parentID := model.StrPtrDeref(model.StrPtrField(f, "parentID"))
	folderID := strFieldDefault(f, "folderID", "")
	// parentID 為空時退回 folderID（相容舊資料）
	if parentID == "" {
		parentID = folderID
	}
	meta := NoteMeta{
		ID:        item.ID,
		ParentID:  parentID,
		FolderID:  folderID,
		Title:     item.GetTitle(),
		Type:      item.Type,
		USN:       item.GetUSN(),
		Tags:      model.StringSliceField(f, "tags"),
		IsNew:     model.BoolField(f, "isNew"),
		CreatedAt: model.Int64StrField(f, "createdAt"),
		UpdatedAt: model.Int64StrField(f, "updatedAt"),
	}
	if meta.UpdatedAt == "" {
		meta.UpdatedAt = model.Int64StrField(f, "updateAt")
	}
	if v := model.StrPtrField(f, "orderAt"); v != nil {
		meta.OrderAt = *v
	}
	if v := model.StrPtrField(f, "status"); v != nil {
		meta.Status = *v
	}
	if v := model.StrPtrField(f, "aiTitle"); v != nil {
		meta.AiTitle = *v
	}
	meta.AiTags = model.StringSliceField(f, "aiTags")
	meta.ImgURLs = model.StringSliceField(f, "imgURLs")

	content := strFieldDefault(f, "content", "")
	return meta, content
}

// ItemToFolderMeta 將 Item（FOLDER / NOTE_FOLDER / …）轉換為 FolderMeta
func ItemToFolderMeta(item *model.Item) FolderMeta {
	f := item.Fields
	folderType := model.StrPtrField(f, "folderType")
	// 若 fields 沒有 folderType，從 itemType 推斷（NOTE_FOLDER→"NOTE"）
	if folderType == nil {
		if sub := model.FolderSubType(item.Type); sub != "" {
			folderType = &sub
		}
	}
	meta := FolderMeta{
		ID:         item.ID,
		FolderName: item.GetName(),
		Type:       folderType,
		ParentID:   model.StrPtrField(f, "parentID"),
		OrderAt:    model.StrPtrField(f, "orderAt"),
		Icon:       model.StrPtrField(f, "icon"),
		CreatedAt:  model.Int64StrField(f, "createdAt"),
		UpdatedAt:  model.Int64StrField(f, "updatedAt"),
		USN:        item.GetUSN(),
		NoteNum:    model.Int64Field(f, "noteNum"),
		IsTemp:     model.BoolField(f, "isTemp"),

		FolderSummary:     model.StrPtrField(f, "folderSummary"),
		AiFolderName:      model.StrPtrField(f, "aiFolderName"),
		AiFolderSummary:   model.StrPtrField(f, "aiFolderSummary"),
		AiInstruction:     model.StrPtrField(f, "aiInstruction"),
		AutoUpdateSummary: model.BoolField(f, "autoUpdateSummary"),

		TemplateHTML:    model.StrPtrField(f, "templateHtml"),
		TemplateCSS:     model.StrPtrField(f, "templateCss"),
		UIPrompt:        model.StrPtrField(f, "uiPrompt"),
		IsShared:        model.BoolField(f, "isShared"),
		Searchable:      model.BoolField(f, "searchable"),
		AllowContribute: model.BoolField(f, "allowContribute"),
		ChartKind:       model.StrPtrField(f, "chartKind"),
	}
	decodeField(f, "indexes", &meta.Indexes)
	meta.IsSummarizedNoteIds = decodeStringPtrSliceField(f, "isSummarizedNoteIds")
	decodeField(f, "fields", &meta.Fields)
	decodeField(f, "templateHistory", &meta.TemplateHistory)
	decodeField(f, "sharers", &meta.Sharers)
	return meta
}

// ItemToCardMeta 將 Item（CARD）轉換為 CardMeta
func ItemToCardMeta(item *model.Item) CardMeta {
	f := item.Fields
	return CardMeta{
		ID:            item.ID,
		ContributorID: model.StrPtrField(f, "contributorId"),
		ParentID:      item.GetParentID(),
		Name:          item.GetName(),
		Fields:        model.StrPtrField(f, "fields"),
		Reviews:       model.StrPtrField(f, "reviews"),
		Coordinates:   model.StrPtrField(f, "coordinates"),
		OrderAt:       model.StrPtrField(f, "orderAt"),
		IsDeleted:     model.BoolField(f, "isDeleted"),
		CreatedAt:     model.Int64StrField(f, "createdAt"),
		UpdatedAt:     model.Int64StrField(f, "updatedAt"),
		USN:           item.GetUSN(),
	}
}

// ItemToChartMeta 將 Item（CHART）轉換為 CardMeta（Chart 共用 CardMeta 結構）
func ItemToChartMeta(item *model.Item) CardMeta {
	f := item.Fields
	return CardMeta{
		ID:       item.ID,
		ParentID: item.GetParentID(),
		Name:      item.GetName(),
		Fields:    model.StrPtrField(f, "data"),
		OrderAt:   model.StrPtrField(f, "orderAt"),
		IsDeleted: model.BoolField(f, "isDeleted"),
		CreatedAt: model.Int64StrField(f, "createdAt"),
		UpdatedAt: model.Int64StrField(f, "updatedAt"),
		USN:       item.GetUSN(),
	}
}

// ItemToMirrorData 將 model.Item 轉換為 ItemMirrorData（新 JSON 鏡像格式）
func ItemToMirrorData(item *model.Item) ItemMirrorData {
	name := item.GetName()
	if name == "" {
		name = VaultFallbackName(item.ID)
	}
	// 深拷貝 Fields 避免共用 map reference 導致原始 Item 被異動
	fields := make(map[string]interface{}, len(item.Fields))
	for k, v := range item.Fields {
		fields[k] = v
	}
	return ItemMirrorData{
		ID:       item.ID,
		Name:     name,
		ItemType: item.Type,
		Fields:   fields,
	}
}

// ItemFolderType 回傳 FOLDER item 的子類型（NOTE/CARD/CHART），用於 PathResolver。
// 優先從 itemType（NOTE_FOLDER→NOTE）推斷，再看 fields.folderType，預設 NOTE。
func ItemFolderType(item *model.Item) string {
	if sub := model.FolderSubType(item.Type); sub != "" {
		return sub
	}
	if v := model.StrPtrField(item.Fields, "folderType"); v != nil {
		return *v
	}
	return "NOTE"
}

func strFieldDefault(fields map[string]interface{}, key, def string) string {
	if v, ok := fields[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return def
}

// decodeField 將 map 的動態值解碼為指定型別（缺值/型別不符時保持原值）
func decodeField(fields map[string]interface{}, key string, out interface{}) {
	v, ok := fields[key]
	if !ok || v == nil {
		return
	}
	raw, err := json.Marshal(v)
	if err != nil {
		log.Printf("[decodeField] marshal %s error: %v", key, err)
		return
	}
	if err := json.Unmarshal(raw, out); err != nil {
		log.Printf("[decodeField] unmarshal %s error: %v", key, err)
	}
}

// decodeStringPtrSliceField 相容舊資料：支援 []string / []interface{} / string
func decodeStringPtrSliceField(fields map[string]interface{}, key string) []*string {
	v, ok := fields[key]
	if !ok || v == nil {
		return nil
	}

	toPtrSlice := func(items []string) []*string {
		out := make([]*string, 0, len(items))
		for _, s := range items {
			if s == "" {
				continue
			}
			value := s
			out = append(out, &value)
		}
		return out
	}

	switch val := v.(type) {
	case []string:
		return toPtrSlice(val)
	case []interface{}:
		items := make([]string, 0, len(val))
		for _, it := range val {
			if s, ok := it.(string); ok {
				items = append(items, s)
			}
		}
		return toPtrSlice(items)
	case string:
		trimmed := strings.TrimSpace(val)
		if trimmed == "" {
			return nil
		}
		// 部分舊資料可能將陣列序列化為字串儲存
		if strings.HasPrefix(trimmed, "[") {
			var arr []string
			if err := json.Unmarshal([]byte(trimmed), &arr); err == nil {
				return toPtrSlice(arr)
			}
		}
		return toPtrSlice([]string{trimmed})
	default:
		var arr []string
		raw, err := json.Marshal(v)
		if err != nil {
			log.Printf("[decodeField] marshal %s error: %v", key, err)
			return nil
		}
		if err := json.Unmarshal(raw, &arr); err != nil {
			log.Printf("[decodeField] unmarshal %s error: %v", key, err)
			return nil
		}
		return toPtrSlice(arr)
	}
}
