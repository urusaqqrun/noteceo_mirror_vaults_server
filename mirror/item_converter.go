package mirror

import (
	"github.com/urusaqqrun/vault-mirror-service/model"
)

// ItemToMirrorData 將 model.Item 轉換為 ItemMirrorData（新 JSON 鏡像格式）
// Name 保留原始值（可能為空），檔名推導 (name || id) 在 ExportItem 處理。
func ItemToMirrorData(item *model.Item) ItemMirrorData {
	name := item.GetName()
	skipFields := map[string]bool{"parentID": true, "createdAt": true, "updatedAt": true}
	fields := make(map[string]interface{}, len(item.Fields))
	for k, v := range item.Fields {
		if skipFields[k] {
			continue
		}
		fields[k] = v
	}
	return ItemMirrorData{
		ID:       item.ID,
		Name:     name,
		ItemType: item.Type,
		Fields:   fields,
	}
}
