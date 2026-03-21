package sync

import (
	"context"
	"fmt"
	"log"
	"sync/atomic"

	"github.com/urusaqqrun/vault-mirror-service/mirror"
	"github.com/urusaqqrun/vault-mirror-service/model"
	"golang.org/x/sync/errgroup"
)

// FullExporter 全量匯出用戶 Vault 所需的讀取介面
type FullExporter interface {
	ListItemFolders(ctx context.Context, userID string) ([]*model.Item, error)
	ListAllItems(ctx context.Context, userID string) ([]*model.Item, error)
}

// ExportFullVault 將用戶所有資料從 Item collection 匯出到 Vault 檔案系統
func ExportFullVault(ctx context.Context, fs mirror.VaultFS, reader FullExporter, userID string) error {
	log.Printf("[FullExport] 開始匯出用戶 %s 的 Vault", userID)

	if err := fs.MkdirAll(userID); err != nil {
		return fmt.Errorf("mkdir user root: %w", err)
	}

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

	// 分離資料夾與非資料夾；資料夾必須先建立目錄，避免 leaf 寫入後被 cleanup 誤刪
	var folders, leaves []*model.Item
	for _, item := range allItems {
		if item == nil {
			continue
		}
		if model.IsFolder(item.Type) {
			folders = append(folders, item)
		} else {
			leaves = append(leaves, item)
		}
	}

	var itemCount, skippedCount int64

	// Phase 1: 並行匯出所有資料夾（建目錄 + 寫 metadata JSON）
	g1, ctx1 := errgroup.WithContext(ctx)
	g1.SetLimit(8)
	for _, item := range folders {
		item := item
		g1.Go(func() error {
			if ctx1.Err() != nil {
				return ctx1.Err()
			}
			if _, err := exporter.ExportItem(userID, item); err != nil {
				log.Printf("[FullExport] %s %s error: %v", item.Type, item.ID, err)
				atomic.AddInt64(&skippedCount, 1)
				return nil
			}
			atomic.AddInt64(&itemCount, 1)
			return nil
		})
	}
	if err := g1.Wait(); err != nil {
		return fmt.Errorf("export folders: %w", err)
	}

	// Phase 2: 並行匯出所有非資料夾項目（寫入已建立的目錄）
	g2, ctx2 := errgroup.WithContext(ctx)
	g2.SetLimit(8)
	for _, item := range leaves {
		item := item
		g2.Go(func() error {
			if ctx2.Err() != nil {
				return ctx2.Err()
			}
			if _, err := exporter.ExportItem(userID, item); err != nil {
				log.Printf("[FullExport] %s %s error: %v", item.Type, item.ID, err)
				atomic.AddInt64(&skippedCount, 1)
				return nil
			}
			atomic.AddInt64(&itemCount, 1)
			return nil
		})
	}
	if err := g2.Wait(); err != nil {
		return fmt.Errorf("export leaves: %w", err)
	}

	claudeMD := buildClaudeMD()
	if err := fs.WriteFile(userID+"/CLAUDE.md", []byte(claudeMD)); err != nil {
		log.Printf("[FullExport] write CLAUDE.md error: %v", err)
	}

	log.Printf("[FullExport] 用戶 %s 匯出完成: %d items (skipped: %d)",
		userID, itemCount, skippedCount)
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

1. 不要刪除任何 _folder.json 中的 ID 欄位
2. 修改 .md 檔案時保留 frontmatter 的 id 和 parentID
3. 搬移檔案時更新 frontmatter 中的 parentID
4. 新建資料夾時必須建立 _folder.json（至少含 folderName 和 type）
5. orderAt 為時間戳字串，決定同層排序
`
}
