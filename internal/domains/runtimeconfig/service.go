package runtimeconfig

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sunshanpeng/yummy/internal/domains/audit"
	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/infra/store/model"
	"github.com/sunshanpeng/yummy/internal/lib/security"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type record struct {
	ID            string `gorm:"primaryKey"`
	WorkspaceID   string
	ApplicationID string
	Revision      int64
	Confirmed     bool
	Spec          []byte `gorm:"type:jsonb"`
	CreatedBy     string
	CreatedAt     time.Time
}

func (record) TableName() string { return "runtime_config_revisions" }

type secretValue struct {
	Name       string `json:"name"`
	Value      string `json:"value,omitempty"`
	Sensitive  bool   `json:"sensitive"`
	Ciphertext []byte `json:"ciphertext,omitempty"`
}
type storedSpec struct {
	Type      string        `json:"type"`
	Port      int32         `json:"port"`
	Probe     Probe         `json:"probe"`
	Command   []string      `json:"command"`
	Args      []string      `json:"args"`
	Variables []secretValue `json:"variables"`
	Files     []secretValue `json:"files"`
	Volumes   []Volume      `json:"volumes"`
	Domains   []string      `json:"domains"`
	Resources Resources     `json:"resources"`
}

type Service struct {
	db     *gorm.DB
	access *authorization.Access
	box    *security.SecretBox
}

func NewService(db *gorm.DB, access *authorization.Access, box *security.SecretBox) *Service {
	return &Service{db: db, access: access, box: box}
}

func suggested(workspaceID, applicationID string) View {
	input := DefaultInput()
	return View{WorkspaceID: workspaceID, ApplicationID: applicationID, Type: input.Type, Port: input.Port, Probe: input.Probe, Resources: input.Resources, Variables: []Variable{}, Files: []ConfigFile{}, Volumes: []Volume{}, Domains: []string{}, Command: []string{}, Args: []string{}}
}

func latest(tx *gorm.DB, workspaceID, applicationID string) (record, storedSpec, error) {
	var value record
	var spec storedSpec
	err := tx.Where("workspace_id = ? AND application_id = ?", workspaceID, applicationID).Order("revision DESC").First(&value).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return record{}, spec, nil
	}
	if err != nil {
		return value, spec, err
	}
	if err := json.Unmarshal(value.Spec, &spec); err != nil {
		return value, spec, errors.New("运行配置持久化格式无效")
	}
	return value, spec, nil
}

func asView(value record, spec storedSpec) View {
	result := View{WorkspaceID: value.WorkspaceID, ApplicationID: value.ApplicationID, Revision: value.Revision, Confirmed: value.Confirmed, Type: spec.Type, Port: spec.Port, Probe: spec.Probe, Command: spec.Command, Args: spec.Args, Resources: spec.Resources, Domains: spec.Domains, Volumes: spec.Volumes, CreatedAt: value.CreatedAt, Variables: make([]Variable, 0, len(spec.Variables)), Files: make([]ConfigFile, 0, len(spec.Files))}
	for _, v := range spec.Variables {
		plain := v.Value
		if v.Sensitive {
			plain = ""
		}
		result.Variables = append(result.Variables, Variable{Name: v.Name, Value: plain, Sensitive: v.Sensitive, Configured: true})
	}
	for _, f := range spec.Files {
		plain := f.Value
		if f.Sensitive {
			plain = ""
		}
		result.Files = append(result.Files, ConfigFile{Path: f.Name, Content: plain, Sensitive: f.Sensitive, Configured: true})
	}
	return result
}

func (s *Service) Get(ctx context.Context, p authorization.Principal, workspaceID, applicationID string) (View, error) {
	var result View
	err := s.access.Do(ctx, p, false, func(tx *gorm.DB, current authorization.Principal) error {
		if _, _, err := s.access.Application(tx, current, workspaceID, applicationID, authorization.ConfigRead); err != nil {
			return err
		}
		value, spec, err := latest(tx, workspaceID, applicationID)
		if err != nil {
			return err
		}
		if value.ID == "" {
			result = suggested(workspaceID, applicationID)
		} else {
			result = asView(value, spec)
		}
		return nil
	})
	return result, err
}

func (s *Service) GetRevision(ctx context.Context, p authorization.Principal, workspaceID, applicationID string, revision int64) (View, error) {
	var result View
	err := s.access.Do(ctx, p, false, func(tx *gorm.DB, current authorization.Principal) error {
		if _, _, err := s.access.Application(tx, current, workspaceID, applicationID, authorization.ConfigRead); err != nil {
			return err
		}
		value, spec, err := readRevision(tx, workspaceID, applicationID, revision)
		if err != nil {
			return err
		}
		result = asView(value, spec)
		return nil
	})
	return result, err
}

func readRevision(tx *gorm.DB, workspaceID, applicationID string, revision int64) (record, storedSpec, error) {
	var value record
	var spec storedSpec
	if err := tx.First(&value, "workspace_id = ? AND application_id = ? AND revision = ?", workspaceID, applicationID, revision).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return value, spec, authorization.ErrNotFound
		}
		return value, spec, err
	}
	if err := json.Unmarshal(value.Spec, &spec); err != nil {
		return value, spec, errors.New("运行配置持久化格式无效")
	}
	return value, spec, nil
}

func (s *Service) Save(ctx context.Context, p authorization.Principal, workspaceID, applicationID string, input Input) (View, error) {
	input, err := Validate(input)
	if err != nil {
		return View{}, err
	}
	if input.IdempotencyKey != "" && (len(input.IdempotencyKey) > 128 || input.IdempotencyKey != strings.TrimSpace(input.IdempotencyKey) || !utf8.ValidString(input.IdempotencyKey) || strings.ContainsAny(input.IdempotencyKey, "\x00\r\n")) {
		return View{}, invalid("幂等键无效")
	}
	payload, _ := json.Marshal(input)
	sum := sha256.Sum256(payload)
	fingerprint := hex.EncodeToString(sum[:])
	var result View
	err = s.access.Do(ctx, p, false, func(tx *gorm.DB, current authorization.Principal) error {
		if _, _, err := s.access.Application(tx, current, workspaceID, applicationID, authorization.ConfigWrite); err != nil {
			return err
		}
		var app model.Application
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&app, "id = ? AND workspace_id = ?", applicationID, workspaceID).Error; err != nil {
			return err
		}
		action := "runtime.config.save:" + applicationID
		if input.IdempotencyKey != "" {
			var key model.IdempotencyKey
			err := tx.First(&key, "workspace_id = ? AND actor_id = ? AND action = ? AND key = ?", workspaceID, current.UserID, action, input.IdempotencyKey).Error
			if err == nil {
				if key.Fingerprint != fingerprint {
					return authorization.ErrConflict
				}
				var saved record
				if err := tx.First(&saved, "id = ? AND workspace_id = ? AND application_id = ?", key.ResultID, workspaceID, applicationID).Error; err != nil {
					return err
				}
				var savedSpec storedSpec
				if err := json.Unmarshal(saved.Spec, &savedSpec); err != nil {
					return err
				}
				result = asView(saved, savedSpec)
				return nil
			}
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
		}
		previous, oldSpec, err := latest(tx, workspaceID, applicationID)
		if err != nil {
			return err
		}
		if previous.Revision != input.ExpectedRevision {
			return fmt.Errorf("%w: 运行配置已被其他操作更新", authorization.ErrConflict)
		}
		// 首次成功部署后的类型冻结由部署模块在同一应用锁下核对。
		revision := previous.Revision + 1
		spec := storedSpec{Type: input.Type, Port: input.Port, Probe: input.Probe, Command: input.Command, Args: input.Args, Resources: input.Resources, Domains: input.Domains, Volumes: input.Volumes}
		for _, variable := range input.Variables {
			value, err := s.prepareSecret(workspaceID, applicationID, "variable", variable.Name, revision, previous.Revision, variable.Value, variable.Sensitive, variable.KeepExisting, oldSpec.Variables)
			if err != nil {
				return err
			}
			spec.Variables = append(spec.Variables, value)
		}
		for _, file := range input.Files {
			value, err := s.prepareSecret(workspaceID, applicationID, "file", file.Path, revision, previous.Revision, file.Content, file.Sensitive, file.KeepExisting, oldSpec.Files)
			if err != nil {
				return err
			}
			spec.Files = append(spec.Files, value)
		}
		// KeepExisting 展开后按最终明文计算总量，不能用小请求累积绕过配额。
		if err := s.validateFinalSize(workspaceID, applicationID, revision, spec); err != nil {
			return err
		}
		data, err := json.Marshal(spec)
		if err != nil {
			return err
		}
		id, err := security.NewID("cfg")
		if err != nil {
			return err
		}
		value := record{ID: id, WorkspaceID: workspaceID, ApplicationID: applicationID, Revision: revision, Confirmed: input.Confirmed, Spec: data, CreatedBy: current.UserID}
		if err := tx.Create(&value).Error; err != nil {
			return err
		}
		if input.IdempotencyKey != "" {
			key := model.IdempotencyKey{WorkspaceID: workspaceID, ActorID: current.UserID, Action: action, Key: input.IdempotencyKey, Fingerprint: fingerprint, ResultID: value.ID}
			if err := tx.Create(&key).Error; err != nil {
				return err
			}
		}
		if err := audit.Record(tx, current.UserID, "application.config.save", "runtime_config", id, &workspaceID, &applicationID); err != nil {
			return err
		}
		result = asView(value, spec)
		return nil
	})
	return result, err
}

func secretIdentity(workspaceID, applicationID, kind, name string, revision int64) string {
	// 使用带长度语义的JSON数组，变量名或路径中的分隔符不能造成身份碰撞。
	data, _ := json.Marshal([]any{workspaceID, applicationID, kind, name, revision})
	return string(data)
}
func (s *Service) prepareSecret(workspaceID, applicationID, kind, name string, revision, oldRevision int64, value string, sensitive, keep bool, previous []secretValue) (secretValue, error) {
	result := secretValue{Name: name, Sensitive: sensitive}
	if keep {
		found := false
		for _, old := range previous {
			if old.Name == name && old.Sensitive {
				plain, err := s.box.Decrypt(old.Ciphertext, secretIdentity(workspaceID, applicationID, kind, name, oldRevision))
				if err != nil {
					return result, err
				}
				value = string(plain)
				found = true
				break
			}
		}
		if !found {
			return result, invalid("未找到可保留的旧敏感配置")
		}
	}
	if sensitive {
		encrypted, err := s.box.Encrypt([]byte(value), secretIdentity(workspaceID, applicationID, kind, name, revision))
		if err != nil {
			return result, err
		}
		result.Ciphertext = encrypted
	} else {
		result.Value = value
	}
	return result, nil
}

// MaterializeForDeployment 只供已完成对象授权的部署调度服务使用，禁止挂载为公共接口。
// 读取冻结revision而非最新草稿，确保构建并部署或回滚不带入后来保存的配置。
func (s *Service) MaterializeForDeployment(tx *gorm.DB, workspaceID, applicationID string, revision int64) (Materialized, error) {
	value, spec, err := readRevision(tx, workspaceID, applicationID, revision)
	if err != nil {
		return Materialized{}, err
	}
	if !value.Confirmed {
		return Materialized{}, invalid("首次部署前必须确认运行配置")
	}
	result := Materialized{View: asView(value, spec), Variables: make(map[string]string), Files: make(map[string]string)}
	for _, v := range spec.Variables {
		plain := v.Value
		if v.Sensitive {
			data, err := s.box.Decrypt(v.Ciphertext, secretIdentity(workspaceID, applicationID, "variable", v.Name, revision))
			if err != nil {
				return Materialized{}, err
			}
			plain = string(data)
		}
		result.Variables[v.Name] = plain
	}
	for _, f := range spec.Files {
		plain := f.Value
		if f.Sensitive {
			data, err := s.box.Decrypt(f.Ciphertext, secretIdentity(workspaceID, applicationID, "file", f.Name, revision))
			if err != nil {
				return Materialized{}, err
			}
			plain = string(data)
		}
		result.Files[f.Name] = plain
	}
	return result, nil
}
