package mirror

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	htmltomd "github.com/JohannesKaufmann/html-to-markdown/v2"
	"gopkg.in/yaml.v3"
)

// NoteMeta frontmatter 元資料
type NoteMeta struct {
	ID        string   `yaml:"id" json:"id"`
	ParentID  string   `yaml:"parentID" json:"parentID"`
	Title     string   `yaml:"title" json:"title"`
	USN       int      `yaml:"usn" json:"usn"`
	Tags      []string `yaml:"tags,omitempty" json:"tags,omitempty"`
	CreatedAt string   `yaml:"createdAt" json:"createdAt"`
	UpdatedAt string   `yaml:"updatedAt" json:"updatedAt"`
	HTMLHash  string   `yaml:"htmlHash" json:"htmlHash"`
}

// FolderMeta _folder.json 元資料
type FolderMeta struct {
	ID         string           `json:"ID"`
	MemberID   string           `json:"memberID"`
	FolderName string           `json:"folderName"`
	Type       *string          `json:"type,omitempty"`
	ParentID   *string          `json:"parentID,omitempty"`
	OrderAt    *string          `json:"orderAt,omitempty"`
	Icon       *string          `json:"icon,omitempty"`
	CreatedAt  string           `json:"createdAt,omitempty"`
	UpdatedAt  string           `json:"updatedAt,omitempty"`
	USN        int              `json:"usn,omitempty"`
	NoteNum    int64            `json:"noteNum,omitempty"`
	Fields     []CardFieldMeta  `json:"fields,omitempty"`
	ChartKind  *string          `json:"chartKind,omitempty"`
}

type CardFieldMeta struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	Options []string `json:"options,omitempty"`
}

// CardMeta Card JSON 元資料
type CardMeta struct {
	ID        string  `json:"id"`
	MemberID  string  `json:"memberID,omitempty"`
	ParentID  string  `json:"parentID"`
	Name      string  `json:"name"`
	Fields    *string `json:"fields,omitempty"`
	Reviews   *string `json:"reviews,omitempty"`
	OrderAt   *string `json:"orderAt,omitempty"`
	IsDeleted bool    `json:"isDeleted,omitempty"`
	CreatedAt string  `json:"createdAt,omitempty"`
	UpdatedAt string  `json:"updatedAt,omitempty"`
	USN       int     `json:"usn,omitempty"`
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
	err := json.Unmarshal(data, &meta)
	return meta, err
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

func computeHash(content string) string {
	h := sha256.Sum256([]byte(content))
	return fmt.Sprintf("%x", h)
}
