package model

// Note 筆記 / 待辦
type Note struct {
	ID       string   `bson:"_id,omitempty"`
	Title    *string  `bson:"title"`
	Content   *string  `bson:"content"`
	Tags      []string `bson:"tags"`
	FolderID  string   `bson:"folderID"`
	ParentID  *string  `bson:"parentID,omitempty"`
	Type      string   `bson:"_type,omitempty"`
	CreateAt  int64    `bson:"createAt"`
	UpdateAt  int64    `bson:"updateAt"`
	OrderAt   *string  `bson:"orderAt,omitempty"`
	Usn       int      `bson:"usn"`
	Status    *string  `bson:"status,omitempty"`
	AiTitle   *string  `bson:"aiTitle,omitempty"`
	AiTags    []string `bson:"aiTags,omitempty"`
	ImgURLs   []string `bson:"imgURLs"`
	IsNew     bool     `bson:"isNew"`
}

// GetTitle 回傳筆記標題，nil 時回傳 "Untitled"
func (n *Note) GetTitle() string {
	if n.Title == nil || *n.Title == "" {
		return "Untitled"
	}
	return *n.Title
}

// GetContent 回傳筆記內容（HTML），nil 時回傳空字串
func (n *Note) GetContent() string {
	if n.Content == nil {
		return ""
	}
	return *n.Content
}
