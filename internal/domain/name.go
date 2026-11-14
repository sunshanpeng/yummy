// 统一命名规范（2026-10-26 设计，负责人已确认）：应用名与工作空间的空间名共用
// 同一规范——小写字母、数字和中划线，字母开头，字母或数字结尾，最长 30 位，
// 不能有连续的 --；正则无前瞻语法，Go regexp、浏览器与 Zod 可共用。
package domain

import "regexp"

var NamePattern = regexp.MustCompile(`^[a-z](-?[a-z0-9])*$`)

const NameMaxLen = 30

// ValidName 报告名称是否符合统一命名规范（规范本身已排除连续 -- 与结尾 -）。
func ValidName(name string) bool {
	return len(name) >= 1 && len(name) <= NameMaxLen && NamePattern.MatchString(name)
}
