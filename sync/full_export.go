package sync

import (
	"context"
	"fmt"
	"log"

	"github.com/urusaqqrun/vault-mirror-service/mirror"
	"github.com/urusaqqrun/vault-mirror-service/model"
)

// FullExporter 全量匯出用戶 Vault 所需的讀取介面
type FullExporter interface {
	ListItemFolders(ctx context.Context, userID string) ([]*model.Item, error)
	ListAllItems(ctx context.Context, userID string) ([]*model.Item, error)
}

// ExportFullVault 將用戶所有資料從 Item collection 匯出到 Vault 檔案系統
func ExportFullVault(ctx context.Context, fs mirror.VaultFS, reader FullExporter, userID string) error {
	log.Printf("[FullExport] 開始匯出用戶 %s 的 Vault", userID)

	fs.MkdirAll(userID)

	folderItems, err := reader.ListItemFolders(ctx, userID)
	if err != nil {
		return fmt.Errorf("list item folders: %w", err)
	}
	resolver := buildPathResolverFromItems(folderItems)
	exporter := mirror.NewExporter(fs, resolver)

	allItems, err := reader.ListAllItems(ctx, userID)
	if err != nil {
		return fmt.Errorf("list all items: %w", err)
	}

	var folderCount, noteCount, cardCount, chartCount, skippedCount int
	for _, item := range allItems {
		if item == nil {
			continue
		}
		switch {
		case model.IsFolder(item.Type):
			meta := mirror.ItemToFolderMeta(item)
			if err := exporter.ExportFolder(userID, meta); err != nil {
				log.Printf("[FullExport] folder %s error: %v", item.ID, err)
			}
			folderCount++
		case item.Type == model.ItemTypeNote || item.Type == model.ItemTypeTodo:
			meta, content := mirror.ItemToNoteMeta(item)
			if err := exporter.ExportNote(userID, meta, content); err != nil {
				log.Printf("[FullExport] note %s error: %v", item.ID, err)
			}
			noteCount++
		case item.Type == model.ItemTypeCard:
			meta := mirror.ItemToCardMeta(item)
			if err := exporter.ExportCard(userID, meta); err != nil {
				log.Printf("[FullExport] card %s error: %v", item.ID, err)
			}
			cardCount++
		case item.Type == model.ItemTypeChart:
			meta := mirror.ItemToChartMeta(item)
			if err := exporter.ExportChart(userID, meta); err != nil {
				log.Printf("[FullExport] chart %s error: %v", item.ID, err)
			}
			chartCount++
		default:
			log.Printf("[FullExport] skip unknown itemType %q for %s", item.Type, item.ID)
			skippedCount++
		}
	}

	claudeMD := buildClaudeMD()
	fs.WriteFile(userID+"/CLAUDE.md", []byte(claudeMD))

	log.Printf("[FullExport] 用戶 %s 匯出完成: %d folders, %d notes, %d cards, %d charts (skipped: %d)",
		userID, folderCount, noteCount, cardCount, chartCount, skippedCount)
	return nil
}

// buildClaudeMD 產生 CLAUDE.md 專案描述檔
func buildClaudeMD() string {
	return `# NoteCEO Vault

你是 NoteCEO Vault 的 AI 助手，正在操作一個包含用戶筆記、卡片、圖表的檔案系統。

## 目錄結構

- NOTE/  — 筆記資料夾（.md 檔案，含 YAML frontmatter）
- TODO/  — 待辦資料夾（.md 檔案，含 YAML frontmatter）
- CARD/  — 卡片畫廊（.json 檔案）
- CHART/ — 圖表（.json 檔案）

每個資料夾都有 _folder.json 存放元資料（ID, parentID, orderAt 等）。

## 規則

1. 不要刪除任何 _folder.json 中的 ID、memberID 欄位
2. 修改 .md 檔案時保留 frontmatter 的 id 和 parentID
3. 搬移檔案時更新 frontmatter 中的 parentID
4. 新建資料夾時必須建立 _folder.json（至少含 folderName 和 type）
5. orderAt 為時間戳字串，決定同層排序
`
}
