package mirror

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	htmltomd "github.com/JohannesKaufmann/html-to-markdown/v2"
	"github.com/yuin/goldmark"
	"gopkg.in/yaml.v3"
)

// NoteMeta frontmatter 元資料（完整映射 model.Note 所有欄位）
type NoteMeta struct {
	ID        string   `yaml:"id" json:"id"`
	ParentID  string   `yaml:"parentID" json:"parentID"`
	FolderID  string   `yaml:"folderID,omitempty" json:"folderID,omitempty"`
	Title     string   `yaml:"title" json:"title"`
	Type      string   `yaml:"type,omitempty" json:"type,omitempty"`
	USN       int      `yaml:"usn" json:"usn"`
	Tags      []string `yaml:"tags,omitempty" json:"tags,omitempty"`
	OrderAt   string   `yaml:"orderAt,omitempty" json:"orderAt,omitempty"`
	Status    string   `yaml:"status,omitempty" json:"status,omitempty"`
	AiTitle   string   `yaml:"aiTitle,omitempty" json:"aiTitle,omitempty"`
	AiTags    []string `yaml:"aiTags,omitempty" json:"aiTags,omitempty"`
	ImgURLs   []string `yaml:"imgURLs,omitempty" json:"imgURLs,omitempty"`
	IsNew     bool     `yaml:"isNew,omitempty" json:"isNew,omitempty"`
	CreatedAt string   `yaml:"createdAt" json:"createdAt"`
	UpdatedAt string   `yaml:"updatedAt" json:"updatedAt"`
	HTMLHash  string   `yaml:"htmlHash" json:"htmlHash"`
}

// FolderMeta _folder.json 元資料（完整映射 model.Folder 所有欄位）
type FolderMeta struct {
	ID         string  `json:"id"`
	FolderName string  `json:"folderName"`
	Type       *string `json:"type,omitempty"`
	ParentID   *string `json:"parentID,omitempty"`
	OrderAt    *string `json:"orderAt,omitempty"`
	Icon       *string `json:"icon,omitempty"`
	CreatedAt  string  `json:"createdAt,omitempty"`
	UpdatedAt  string  `json:"updatedAt,omitempty"`
	USN        int     `json:"usn,omitempty"`
	NoteNum    int64   `json:"noteNum,omitempty"`
	IsTemp     bool    `json:"isTemp,omitempty"`

	// NOTE/TODO 專用
	Indexes             []IndexMeta `json:"indexes,omitempty"`
	FolderSummary       *string     `json:"folderSummary,omitempty"`
	AiFolderName        *string     `json:"aiFolderName,omitempty"`
	AiFolderSummary     *string     `json:"aiFolderSummary,omitempty"`
	AiInstruction       *string     `json:"aiInstruction,omitempty"`
	AutoUpdateSummary   bool        `json:"autoUpdateSummary,omitempty"`
	IsSummarizedNoteIds []*string   `json:"isSummarizedNoteIds,omitempty"`

	// CARD 專用
	Fields          []CardFieldMeta       `json:"fields,omitempty"`
	TemplateHTML    *string               `json:"templateHtml,omitempty"`
	TemplateCSS     *string               `json:"templateCss,omitempty"`
	UIPrompt        *string               `json:"uiPrompt,omitempty"`
	TemplateHistory []TemplateHistoryMeta `json:"templateHistory,omitempty"`
	IsShared        bool                  `json:"isShared,omitempty"`
	Searchable      bool                  `json:"searchable,omitempty"`
	AllowContribute bool                  `json:"allowContribute,omitempty"`
	Sharers         []SharerMeta          `json:"sharers,omitempty"`

	// CHART 專用
	ChartKind *string `json:"chartKind,omitempty"`
}

type IndexMeta struct {
	Name       string   `json:"name"`
	Notes      []string `json:"notes,omitempty"`
	IsReserved bool     `json:"isReserved,omitempty"`
}

type CardFieldMeta struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	Options []string `json:"options,omitempty"`
}

type TemplateHistoryMeta struct {
	HTML      string `json:"html"`
	CSS       string `json:"css"`
	Timestamp string `json:"timestamp"`
}

type SharerMeta struct {
	UserID string `json:"userId"`
	Role   string `json:"role"`
}

// CardMeta Card/Chart JSON 元資料
type CardMeta struct {
	ID            string  `json:"id"`
	ContributorID *string `json:"contributorId,omitempty"`
	ParentID      string  `json:"parentID"`
	Name          string  `json:"name"`
	Fields        *string `json:"fields,omitempty"`
	Reviews       *string `json:"reviews,omitempty"`
	Coordinates   *string `json:"coordinates,omitempty"`
	OrderAt       *string `json:"orderAt,omitempty"`
	IsDeleted     bool    `json:"isDeleted,omitempty"`
	CreatedAt     string  `json:"createdAt,omitempty"`
	UpdatedAt     string  `json:"updatedAt,omitempty"`
	USN           int     `json:"usn,omitempty"`
}

// NoteToMarkdown 將 Note 的 HTML content 轉為帶 frontmatter 的 Markdown
func NoteToMarkdown(meta NoteMeta, html string) (string, error) {
	meta.HTMLHash = computeHash(html)

	var buf bytes.Buffer

	buf.WriteString("---\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(meta); err != nil {
		return "", fmt.Errorf("encode frontmatter: %w", err)
	}
	enc.Close()
	buf.WriteString("---\n\n")

	if html != "" {
		md, err := htmltomd.ConvertString(html)
		if err != nil {
			return "", fmt.Errorf("html to markdown: %w", err)
		}
		buf.WriteString(md)
	}

	return buf.String(), nil
}

// MarkdownToNote 解析帶 frontmatter 的 Markdown，回傳 meta 和 body（Markdown 文字）
func MarkdownToNote(md string) (NoteMeta, string, error) {
	var meta NoteMeta

	if !strings.HasPrefix(md, "---\n") {
		return meta, md, nil
	}

	endIdx := strings.Index(md[4:], "\n---")
	if endIdx < 0 {
		return meta, md, nil
	}

	frontmatter := md[4 : 4+endIdx]
	body := strings.TrimPrefix(md[4+endIdx+4:], "\n")

	if err := yaml.Unmarshal([]byte(frontmatter), &meta); err != nil {
		return meta, body, fmt.Errorf("parse frontmatter: %w", err)
	}

	return meta, body, nil
}

// FolderToJSON 將 FolderMeta 序列化為 _folder.json 格式
func FolderToJSON(meta FolderMeta) ([]byte, error) {
	return json.MarshalIndent(meta, "", "  ")
}

// JSONToFolder 從 _folder.json 反序列化
func JSONToFolder(data []byte) (FolderMeta, error) {
	var meta FolderMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return meta, err
	}
	// 向後相容：舊版 _folder.json 使用大寫 "ID"
	if meta.ID == "" {
		var legacy struct {
			ID string `json:"ID"`
		}
		if err := json.Unmarshal(data, &legacy); err == nil && legacy.ID != "" {
			meta.ID = legacy.ID
		}
	}
	return meta, nil
}

// CardToJSON 將 CardMeta 序列化為 JSON
func CardToJSON(meta CardMeta) ([]byte, error) {
	return json.MarshalIndent(meta, "", "  ")
}

// JSONToCard 從 JSON 反序列化
func JSONToCard(data []byte) (CardMeta, error) {
	var meta CardMeta
	err := json.Unmarshal(data, &meta)
	return meta, err
}

// MarkdownToHTML 將 Markdown 文字轉為 HTML（用於回寫資料庫的 content 欄位）
func MarkdownToHTML(md string) (string, error) {
	var buf bytes.Buffer
	if err := goldmark.Convert([]byte(md), &buf); err != nil {
		return "", fmt.Errorf("markdown to html: %w", err)
	}
	return buf.String(), nil
}

func computeHash(content string) string {
	h := sha256.Sum256([]byte(content))
	return fmt.Sprintf("%x", h)
}

// ---- 新格式：統一 JSON 鏡像 ----

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
func MirrorJSONToItem(raw []byte) (*ItemMirrorData, error) {
	var data ItemMirrorData
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	if data.ID == "" || data.ItemType == "" {
		return nil, fmt.Errorf("invalid mirror json: missing id or itemType")
	}
	if data.Fields == nil {
		data.Fields = make(map[string]interface{})
	}
	return &data, nil
}

// VaultFallbackName 產生 fallback 檔名（當 DB name 為空時使用）
func VaultFallbackName(id string) string {
	return "untitled_" + id
}

// IsVaultFallbackName 判斷 name 是否為 vault 自動產生的 fallback
func IsVaultFallbackName(name, id string) bool {
	return name == VaultFallbackName(id)
}
