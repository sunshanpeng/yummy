package workspace

import (
	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/infra/store/model"
	"gorm.io/gorm"
)

// applyWorkspaceListScope 装配空间列表的行级授权过滤，返回 (过滤后的查询, 是否可见)。
// 规则：
//   - 非 InstanceAdmin 必须是 workspace_members 的成员；
//   - Bearer（Agent PAT）只看到其应用读授权覆盖的空间：须为空间 admin 或
//     任一授权应用的 application_members 成员；
//   - 两者都按工作空间状态过滤 active。
//
// visible=false 表示该主体按策略无任何可见空间，调用方直接返回空列表（不报错）。
//
// 【方言兼容】全部为标准 SQL，PostgreSQL 与 MySQL 8 通用：EXISTS 相关子查询、
// IN 占位符由 gorm 参数化展开、role 等标识符在两个方言均非保留字。子查询刻意
// 引用外层别名（workspaces.id / a.id）做行级关联，gorm gen 的类型安全字段无法
// 自然表达这类相关引用，故按约定以参数化片段形式收口在本文件，不散落业务代码。
func applyWorkspaceListScope(query *gorm.DB, p authorization.Principal) (*gorm.DB, bool) {
	query = query.Model(&model.Workspace{}).Where("workspaces.status = ?", model.ResourceActive)
	if !p.InstanceAdmin {
		query = query.Where("EXISTS (SELECT 1 FROM workspace_members wm WHERE wm.workspace_id = workspaces.id AND wm.user_id = ?)", p.UserID)
	}
	if p.IsBearer() {
		if !p.AllowsAnyApplicationRead() || len(p.ApplicationIDs) == 0 {
			return query, false
		}
		appQuery := "EXISTS (SELECT 1 FROM applications a WHERE a.workspace_id = workspaces.id AND a.status = ? AND a.id IN ?"
		args := []any{model.ResourceActive, p.ApplicationIDs}
		if !p.InstanceAdmin {
			appQuery += " AND (EXISTS (SELECT 1 FROM workspace_members wm WHERE wm.workspace_id = workspaces.id AND wm.user_id = ? AND wm.role = 'admin') OR EXISTS (SELECT 1 FROM application_members am WHERE am.workspace_id = workspaces.id AND am.application_id = a.id AND am.user_id = ?))"
			args = append(args, p.UserID, p.UserID)
		}
		query = query.Where(appQuery+")", args...)
	}
	return query, true
}
