package executor

import (
	"github.com/urusaqqrun/vault-mirror-service/mirror"
)

// BuildPathIDMap 掃描用戶 Vault 目錄，建立 path→docID 映射。
// 用於刪除回寫：AI 刪除檔案後無法讀取內容取得 ID，需事前建立映射。
func BuildPathIDMap(vaultFS mirror.VaultFS, userID string) map[string]string {
	_, idMap, err := TakeSnapshotAndPathIDMap(vaultFS, userID)
	if err != nil {
		return map[string]string{}
	}
	return idMap
}
