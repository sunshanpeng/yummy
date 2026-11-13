package application

import "time"

type View struct {
	ID           string    `json:"id"`
	WorkspaceID  string    `json:"workspaceId"`
	Name         string    `json:"name"`
	Description  string    `json:"description"`
	Status       string    `json:"status"`
	Revision     int64     `json:"revision"`
	Capabilities []string  `json:"capabilities"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

type Page struct {
	Items      []View `json:"items"`
	NextCursor string `json:"nextCursor,omitempty"`
}
type CreateInput struct{ Name, IdempotencyKey string }
type UpdateInput struct {
	Name, Description string
	ExpectedRevision  int64
}

type MemberView struct {
	UserID     string `json:"userId"`
	Username   string `json:"username"`
	UserStatus string `json:"userStatus"`
	Role       string `json:"role"`
	Inherited  bool   `json:"inherited"`
}

type MemberPage struct {
	Items      []MemberView `json:"items"`
	NextCursor string       `json:"nextCursor,omitempty"`
}
