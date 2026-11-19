package enrollment

import "time"

// nodeRecord 是 inventory 的只读身份投影；本模块唯一写入是明确的停用/资格撤销动作。
type nodeRecord struct {
	ID       string `gorm:"primaryKey"`
	WorkerID string
	Status   string
	Revision int64
}

func (nodeRecord) TableName() string { return "execution_nodes" }

type registrationRecord struct {
	ID         string `gorm:"primaryKey"`
	NodeID     string
	TokenHash  string
	CreatedBy  string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	ConsumedAt *time.Time
	RevokedAt  *time.Time
}

func (registrationRecord) TableName() string { return "worker_registration_tokens" }

type certificateRecord struct {
	Serial            string `gorm:"primaryKey"`
	NodeID            string
	WorkerID          string
	CertificateSHA256 string
	PublicKeySHA256   string
	NotBefore         time.Time
	NotAfter          time.Time
	IssuedAt          time.Time
	RevokedAt         *time.Time
	RotatedFrom       *string
}

func (certificateRecord) TableName() string { return "worker_certificates" }

type credentialEvent struct {
	ID        string `gorm:"primaryKey"`
	NodeID    string
	WorkerID  string
	Serial    string
	Action    string
	TraceID   string
	CreatedAt time.Time
}

func (credentialEvent) TableName() string { return "worker_credential_events" }

func identity(value certificateRecord) Identity {
	return Identity{NodeID: value.NodeID, WorkerID: value.WorkerID, Serial: value.Serial, NotAfter: value.NotAfter}
}
