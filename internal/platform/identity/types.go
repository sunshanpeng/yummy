package identity

import "time"

type UserView struct {
	ID            string `json:"id"`
	Username      string `json:"username"`
	Status        string `json:"status"`
	InstanceAdmin bool   `json:"instanceAdmin"`
	AuthVersion   int64  `json:"authVersion"`
}

type TokenView struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	Scopes         []string   `json:"scopes"`
	ApplicationIDs []string   `json:"applicationIds"`
	ExpiresAt      time.Time  `json:"expiresAt"`
	CreatedAt      time.Time  `json:"createdAt"`
	RevokedAt      *time.Time `json:"revokedAt,omitempty"`
	LastUsedAt     *time.Time `json:"lastUsedAt,omitempty"`
}

type NewToken struct {
	Token     TokenView `json:"token"`
	Plaintext string    `json:"plaintext"`
}

type CreateTokenInput struct {
	Name           string
	Scopes         []string
	ApplicationIDs []string
	ExpiresAt      time.Time
}

type InvitationView struct {
	ID          string     `json:"id"`
	WorkspaceID string     `json:"workspaceId"`
	Username    string     `json:"username"`
	Role        string     `json:"role"`
	ExpiresAt   time.Time  `json:"expiresAt"`
	ConsumedAt  *time.Time `json:"consumedAt,omitempty"`
	RevokedAt   *time.Time `json:"revokedAt,omitempty"`
}

type NewInvitation struct {
	Invitation InvitationView `json:"invitation"`
	Plaintext  string         `json:"plaintext"`
}
