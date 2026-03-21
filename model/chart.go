package model

// Chart 圖表
type Chart struct {
	ID       string  `json:"id" bson:"_id,omitempty"`
	ParentID string  `json:"parentID" bson:"parentID"`
	Name      string  `json:"name" bson:"name"`
	Data      *string `json:"data,omitempty" bson:"data,omitempty"`
	IsDeleted bool    `json:"isDeleted" bson:"isDeleted"`
	CreatedAt string  `json:"createdAt" bson:"createdAt"`
	UpdatedAt string  `json:"updatedAt" bson:"updatedAt"`
	Usn       int     `json:"usn" bson:"usn"`
}
