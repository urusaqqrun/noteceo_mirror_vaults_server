package model

// Card 卡片
type Card struct {
	ID            string  `json:"id" bson:"_id,omitempty"`
	ContributorID *string `json:"contributorId,omitempty" bson:"contributorId,omitempty"`
	ParentID      string  `json:"parentID" bson:"parentID"`
	Name          string  `json:"name" bson:"name"`
	Fields        *string `json:"fields,omitempty" bson:"fields,omitempty"`
	Reviews       *string `json:"reviews,omitempty" bson:"reviews,omitempty"`
	Coordinates   *string `json:"coordinates,omitempty" bson:"coordinates,omitempty"`
	OrderAt       *string `json:"orderAt,omitempty" bson:"orderAt,omitempty"`
	IsDeleted     bool    `json:"isDeleted" bson:"isDeleted"`
	CreatedAt     string  `json:"createdAt" bson:"createdAt"`
	UpdatedAt     string  `json:"updatedAt" bson:"updatedAt"`
	Usn           int     `json:"usn" bson:"usn"`
}
