package model

import (
	"fmt"
	"strings"
)

// Item 統一資料類型（對應 go-service 的 Item collection）
type Item struct {
	ID     string                 `json:"id" bson:"_id,omitempty"`
	Name   string                 `json:"name" bson:"name"`
	Type   string                 `json:"itemType" bson:"itemType"`
	Fields map[string]interface{} `json:"fields" bson:"fields"`
}

// Item type 常數
const (
	ItemTypeFolder     = "FOLDER"
	ItemTypeNoteFolder = "NOTE_FOLDER"
	ItemTypeCardFolder = "CARD_FOLDER"
	ItemTypeChartFolder = "CHART_FOLDER"
	ItemTypeTodoFolder = "TODO_FOLDER"
	ItemTypeNote       = "NOTE"
	ItemTypeTodo       = "TODO"
	ItemTypeCard       = "CARD"
	ItemTypeChart      = "CHART"
)

// IsFolder 判斷 itemType 是否為資料夾類型（相容舊的 "FOLDER" + 通用 _FOLDER 後綴）
func IsFolder(itemType string) bool {
	return itemType == "FOLDER" || strings.HasSuffix(itemType, "_FOLDER")
}

// FolderSubType 從 itemType 取得資料夾子類型（NOTE_FOLDER→"NOTE"，KANBAN_FOLDER→"KANBAN"）
// 非 _FOLDER 結尾回傳空字串
func FolderSubType(itemType string) string {
	if !strings.HasSuffix(itemType, "_FOLDER") {
		return ""
	}
	return strings.TrimSuffix(itemType, "_FOLDER")
}

func (i *Item) GetUSN() int {
	return intField(i.Fields, "usn")
}

func (i *Item) GetTitle() string {
	if i.Name != "" {
		return i.Name
	}
	t := strField(i.Fields, "title")
	if t == "" {
		t = strField(i.Fields, "name")
	}
	if t == "" {
		return "Untitled"
	}
	return t
}

func (i *Item) GetContent() string {
	return strField(i.Fields, "content")
}

func (i *Item) GetParentID() string {
	return strField(i.Fields, "parentID")
}

func (i *Item) GetName() string {
	if i.Name != "" {
		return i.Name
	}
	return strField(i.Fields, "name")
}

// GetFolderID 取得非資料夾 item 所屬的資料夾 ID（優先 folderID，退回 parentID 向後相容）
func (i *Item) GetFolderID() string {
	if v := strField(i.Fields, "folderID"); v != "" {
		return v
	}
	return strField(i.Fields, "parentID")
}

// strField 從 fields map 取出字串值
func strField(fields map[string]interface{}, key string) string {
	if fields == nil {
		return ""
	}
	if v, ok := fields[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// intField 從 fields map 取出整數值
func intField(fields map[string]interface{}, key string) int {
	if v, ok := fields[key]; ok {
		switch n := v.(type) {
		case int:
			return n
		case int32:
			return int(n)
		case int64:
			return int(n)
		case float64:
			return int(n)
		}
	}
	return 0
}

// int64Field 從 fields map 取出 int64 值
func Int64Field(fields map[string]interface{}, key string) int64 {
	if v, ok := fields[key]; ok {
		switch n := v.(type) {
		case int64:
			return n
		case int32:
			return int64(n)
		case int:
			return int64(n)
		case float64:
			return int64(n)
		}
	}
	return 0
}

// BoolField 從 fields map 取出布林值
func BoolField(fields map[string]interface{}, key string) bool {
	if v, ok := fields[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return false
}

// StringSliceField 從 fields map 取出字串陣列
func StringSliceField(fields map[string]interface{}, key string) []string {
	v, ok := fields[key]
	if !ok || v == nil {
		return nil
	}
	switch s := v.(type) {
	case []string:
		return s
	case []interface{}:
		out := make([]string, 0, len(s))
		for _, item := range s {
			if str, ok := item.(string); ok {
				out = append(out, str)
			}
		}
		return out
	}
	return nil
}

// StrPtrField 從 fields map 取出 *string（nil-safe）
func StrPtrField(fields map[string]interface{}, key string) *string {
	if v, ok := fields[key]; ok && v != nil {
		if s, ok := v.(string); ok {
			return &s
		}
	}
	return nil
}

// StrPtrDeref 解引用 *string，nil 回傳空字串
func StrPtrDeref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Int64StrField 取出整數欄位並轉為字串（timestamp 用）
func Int64StrField(fields map[string]interface{}, key string) string {
	v := Int64Field(fields, key)
	if v == 0 {
		if s := strField(fields, key); s != "" {
			return s
		}
		return ""
	}
	return fmt.Sprintf("%d", v)
}
