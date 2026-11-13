package workspace

import "time"

type View struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Slug         string    `json:"slug"`
	Description  string    `json:"description"`
	Status       string    `json:"status"`
	Revision     int64     `json:"revision"`
	Role         string    `json:"role"`
	Capabilities []string  `json:"capabilities"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

type Page struct {
	Items      []View `json:"items"`
	NextCursor string `json:"nextCursor,omitempty"`
}

type CreateInput struct{ Name, Slug, Description string }
type UpdateInput struct {
	Name, Slug, Description string
	ExpectedRevision        int64
}

type ApplicationGrant struct {
	ApplicationID   string `json:"applicationId"`
	ApplicationName string `json:"applicationName"`
	Role            string `json:"role"`
}

type MemberView struct {
	UserID                  string             `json:"userId"`
	Username                string             `json:"username"`
	UserStatus              string             `json:"userStatus"`
	Role                    string             `json:"role"`
	Applications            []ApplicationGrant `json:"applications"`
	InheritsAllApplications bool               `json:"inheritsAllApplications"`
	ApplicationsTruncated   bool               `json:"applicationsTruncated"`
}

type MemberPage struct {
	Items      []MemberView `json:"items"`
	NextCursor string       `json:"nextCursor,omitempty"`
}
