// 命令 generate 仅从已审查的持久化模型生成类型化 DAO；不读取生产库，不执行迁移。
package main

import (
	"github.com/sunshanpeng/yummy/internal/infra/store/model"
	"gorm.io/gen"
)

func main() {
	generator := gen.NewGenerator(gen.Config{
		OutPath: "internal/infra/store/query",
		Mode:    gen.WithQueryInterface,
	})
	generator.ApplyBasic(
		model.User{},
		model.Workspace{},
		model.WorkspaceMember{},
		model.Application{},
		model.ApplicationMember{},
		model.Invitation{},
		model.AccessToken{},
		model.AccessTokenApplication{},
		model.PasswordReset{},
		model.AuditEvent{},
		model.IdempotencyKey{},
	)
	generator.Execute()
}
