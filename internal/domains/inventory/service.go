// Package inventory 管理实例执行资源和受限分配，不新增常驻进程。
package inventory

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/lib/naming"
	"github.com/sunshanpeng/yummy/internal/lib/security"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Service struct {
	db     *gorm.DB
	access *authorization.Access
	box    *security.SecretBox
}

func NewService(db *gorm.DB, access *authorization.Access, box *security.SecretBox) *Service {
	return &Service{db: db, access: access, box: box}
}

func instance(tx *gorm.DB) (instanceRecord, error) {
	// 持久实例 ID 与每次启动的控制面连接身份无关；所有初始化者共享事务锁。
	if err := lockInstance(tx); err != nil {
		return instanceRecord{}, err
	}
	var value instanceRecord
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&value, "singleton = TRUE").Error
	if err == nil {
		return value, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return value, err
	}
	id, err := security.NewID("ins")
	if err != nil {
		return value, err
	}
	value = instanceRecord{Singleton: true, ID: id}
	return value, tx.Create(&value).Error
}
func (s *Service) InstanceID(ctx context.Context, p authorization.Principal) (string, error) {
	var id string
	err := s.access.Do(ctx, p, true, func(tx *gorm.DB, current authorization.Principal) error {
		if err := s.access.RequireInstance(current); err != nil {
			return err
		}
		value, err := instance(tx)
		id = value.ID
		return err
	})
	return id, err
}
func invalid(message string) error { return fmt.Errorf("%w: %s", authorization.ErrInvalid, message) }
func missing(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return authorization.ErrNotFound
	}
	return err
}
func conflicting(err error) error {
	var pgError *pgconn.PgError
	if errors.As(err, &pgError) && pgError.Code == "23505" {
		return authorization.ErrConflict
	}
	return err
}
func validateName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if !utf8.ValidString(name) || utf8.RuneCountInString(name) < 1 || utf8.RuneCountInString(name) > 100 {
		return "", invalid("名称需要1～100字符")
	}
	return name, nil
}
func validateNode(input NodeInput) (NodeInput, error) {
	name, err := validateName(input.Name)
	input.Name = name
	if err != nil {
		return input, err
	}
	if !naming.ValidWorkerID(input.WorkerID) || input.Capacity < 1 || input.Capacity > 1000 {
		return input, invalid("worker身份或容量无效")
	}
	return input, nil
}

var prefixPattern = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*$`)

func validatePrefix(value string) error {
	if len(value) > 200 || value != strings.TrimSpace(value) || (value != "" && !prefixPattern.MatchString(value)) {
		return invalid("仓库前缀必须是规范小写repository路径")
	}
	return nil
}
func registryURL(value string) (string, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") || parsed.RawPath != "" || parsed.Opaque != "" {
		return "", invalid("仓库地址必须为HTTPS根地址，不能含凭据、查询或路径")
	}
	return "https://" + strings.ToLower(parsed.Host), nil
}
func validateRegistry(input RegistryInput) (RegistryInput, error) {
	var err error
	input.Name, err = validateName(input.Name)
	if err != nil {
		return input, err
	}
	input.URL, err = registryURL(input.URL)
	if err != nil {
		return input, err
	}
	if err := validatePrefix(input.RepositoryPrefix); err != nil {
		return input, err
	}
	if len(input.Username) > 256 || len(input.Password) > 65536 || !utf8.ValidString(input.Username) {
		return input, invalid("仓库凭据长度无效")
	}
	return input, nil
}
func credentialIdentity(instanceID, registryID string, version int64) string {
	return fmt.Sprintf("%s/registry/%s/credential/%d", instanceID, registryID, version)
}
