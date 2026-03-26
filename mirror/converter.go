package mirror

import (
	"encoding/json"
	"fmt"
)

// ---- 統一 JSON 鏡像格式 ----

// ItemMirrorData 對應 vault 中每個 .json 檔案的內容（完整 Item）
type ItemMirrorData struct {
	ID       string                 `json:"id"`
	Name     string                 `json:"name"`
	ItemType string                 `json:"itemType"`
	Fields   map[string]interface{} `json:"fields"`
}

// ItemToMirrorJSON 將 ItemMirrorData 序列化為格式化 JSON
func ItemToMirrorJSON(data ItemMirrorData) ([]byte, error) {
	return json.MarshalIndent(data, "", "  ")
}

// MirrorJSONToItem 從 .json 反序列化為 ItemMirrorData
// 支援兩種格式：
// 1. nested: {"id":"x","name":"n","itemType":"T","fields":{"content":"..."}}
// 2. flat:   {"id":"x","name":"n","itemType":"T","content":"...","tags":[]}
// flat 格式會自動把非標準 key 收進 Fields
func MirrorJSONToItem(raw []byte) (*ItemMirrorData, error) {
	var data ItemMirrorData
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	if data.ID == "" || data.ItemType == "" {
		return nil, fmt.Errorf("invalid mirror json: missing id or itemType")
	}
	// 如果 Fields 為空，嘗試從 flat JSON 收集非標準 key 到 Fields
	if data.Fields == nil || len(data.Fields) == 0 {
		var flat map[string]interface{}
		json.Unmarshal(raw, &flat)
		fields := make(map[string]interface{})
		standardKeys := map[string]bool{
			"id": true, "name": true, "itemType": true, "fields": true,
		}
		for k, v := range flat {
			if !standardKeys[k] {
				fields[k] = v
			}
		}
		data.Fields = fields
	}
	return &data, nil
}

// ItemToJSON 通用匯出（序列化 map 為格式化 JSON）
func ItemToJSON(doc map[string]interface{}) ([]byte, error) {
	return json.MarshalIndent(doc, "", "  ")
}

// JSONToItem 通用匯入（反序列化 JSON 為 map）
func JSONToItem(data []byte) (map[string]interface{}, error) {
	var doc map[string]interface{}
	err := json.Unmarshal(data, &doc)
	return doc, err
}

// VaultFallbackName 產生 fallback 檔名（當 DB name 為空時使用）
func VaultFallbackName(id string) string {
	return "untitled_" + id
}

// IsVaultFallbackName 判斷 name 是否為 vault 自動產生的 fallback
func IsVaultFallbackName(name, id string) bool {
	return name == VaultFallbackName(id)
}
