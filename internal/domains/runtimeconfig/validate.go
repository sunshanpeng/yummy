package runtimeconfig

import (
	"fmt"
	"net"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"golang.org/x/net/idna"
)

var variableName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
var volumeName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
var domainLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

func DefaultInput() Input {
	return Input{Type: TypeHTTP, Port: 8080, Probe: Probe{Path: "/health", PeriodSeconds: 10, TimeoutSeconds: 2, FailureThreshold: 3}, Resources: Resources{CPURequestMilli: 100, CPULimitMilli: 500, MemoryRequestMiB: 128, MemoryLimitMiB: 512}}
}

func invalid(message string) error { return fmt.Errorf("%w: %s", authorization.ErrInvalid, message) }

func Validate(input Input) (Input, error) {
	if input.Type != TypeHTTP && input.Type != TypeWorker {
		return input, invalid("运行类型必须为HTTP或后台进程")
	}
	if input.ExpectedRevision < 0 {
		return input, invalid("配置版本不能为负数")
	}
	if input.Type == TypeHTTP {
		if input.Port < 1 || input.Port > 65535 {
			return input, invalid("HTTP容器端口必须为1～65535")
		}
		if input.Probe.Path == "" || !strings.HasPrefix(input.Probe.Path, "/") || strings.ContainsAny(input.Probe.Path, "?#\r\n\x00") || len(input.Probe.Path) > 1024 {
			return input, invalid("探针路径必须为不含查询或片段的绝对HTTP路径")
		}
		if input.Probe.InitialDelaySeconds < 0 || input.Probe.InitialDelaySeconds > 600 || input.Probe.PeriodSeconds < 1 || input.Probe.PeriodSeconds > 300 || input.Probe.TimeoutSeconds < 1 || input.Probe.TimeoutSeconds > 30 || input.Probe.FailureThreshold < 1 || input.Probe.FailureThreshold > 30 {
			return input, invalid("探针时间或失败阈值超出允许范围")
		}
	} else if input.Port != 0 || input.Probe.Path != "" || len(input.Domains) > 0 {
		return input, invalid("后台进程不能配置HTTP端口、探针或域名")
	}
	r := input.Resources
	if r.CPURequestMilli < 10 || r.CPULimitMilli < r.CPURequestMilli || r.CPULimitMilli > 16000 || r.MemoryRequestMiB < 32 || r.MemoryLimitMiB < r.MemoryRequestMiB || r.MemoryLimitMiB > 32768 {
		return input, invalid("资源请求必须小于限制，CPU最多16核、内存最多32GiB；实际还受实例配额约束")
	}
	if len(input.Command) > 64 || len(input.Args) > 128 {
		return input, invalid("启动命令或参数过多")
	}
	for _, part := range append(slices.Clone(input.Command), input.Args...) {
		if len(part) > 8192 || !utf8.ValidString(part) || strings.ContainsRune(part, 0) {
			return input, invalid("启动参数格式无效")
		}
	}
	if len(input.Variables) > 200 || len(input.Files) > 50 || len(input.Volumes) > 10 || len(input.Domains) > 10 {
		return input, invalid("变量、文件、卷或域名数量超过限制")
	}
	seen := make(map[string]bool)
	var total int
	for _, v := range input.Variables {
		if !variableName.MatchString(v.Name) || seen[v.Name] {
			return input, invalid("变量名称格式无效或重复")
		}
		seen[v.Name] = true
		if !utf8.ValidString(v.Value) || strings.ContainsRune(v.Value, 0) || len(v.Value) > 65536 {
			return input, invalid("变量值格式无效或超过64KiB")
		}
		if v.KeepExisting && (!v.Sensitive || v.Value != "") {
			return input, invalid("保留旧秘密时不得传入新值，且必须为敏感变量")
		}
		total += len(v.Value)
	}
	mounts := make([]string, 0, len(input.Files)+len(input.Volumes))
	for _, f := range input.Files {
		if !validMount(f.Path) || !utf8.ValidString(f.Content) || strings.ContainsRune(f.Content, 0) || len(f.Content) > 1<<20 {
			return input, invalid("配置文件路径或内容无效，单文件最多1MiB")
		}
		if f.KeepExisting && (!f.Sensitive || f.Content != "") {
			return input, invalid("保留旧秘密文件时不能传入内容")
		}
		mounts = append(mounts, f.Path)
		total += len(f.Content)
	}
	if total > 2<<20 {
		return input, invalid("配置内容总量超过2MiB")
	}
	seen = make(map[string]bool)
	for _, v := range input.Volumes {
		if !volumeName.MatchString(v.Name) || strings.HasSuffix(v.Name, "-") || seen[v.Name] || !validMount(v.MountPath) || v.SizeGiB < 1 || v.SizeGiB > 100 {
			return input, invalid("卷名称、路径或容量无效")
		}
		seen[v.Name] = true
		mounts = append(mounts, v.MountPath)
	}
	seenMounts := make(map[string]bool, len(mounts))
	for _, mount := range mounts {
		if seenMounts[mount] {
			return input, invalid("文件和卷挂载路径存在重叠")
		}
		seenMounts[mount] = true
	}
	for _, mount := range mounts {
		for parent := path.Dir(mount); parent != "/"; parent = path.Dir(parent) {
			if seenMounts[parent] {
				return input, invalid("文件和卷挂载路径存在重叠")
			}
		}
	}
	seen = make(map[string]bool)
	for i, domain := range input.Domains {
		canonical, err := CanonicalDomain(domain)
		if err != nil {
			return input, err
		}
		if seen[canonical] {
			return input, invalid("域名重复")
		}
		seen[canonical] = true
		input.Domains[i] = canonical
	}
	return input, nil
}

func validMount(value string) bool {
	if !path.IsAbs(value) || path.Clean(value) != value || value == "/" || len(value) > 1024 || strings.ContainsAny(value, "\\:\x00") {
		return false
	}
	for _, forbidden := range []string{"/proc", "/sys", "/dev", "/var/run/secrets"} {
		if value == forbidden || strings.HasPrefix(value, forbidden+"/") {
			return false
		}
	}
	return true
}

// CanonicalDomain 统一IDNA、小写和尾点，禁止URL/IP/通配域名防止任意探测目标。
func CanonicalDomain(value string) (string, error) {
	value = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(value)), ".")
	if strings.ContainsAny(value, "/:@*?#\x00") || net.ParseIP(value) != nil {
		return "", invalid("域名必须为完整DNS名称，不支持IP、通配符或URL")
	}
	ascii, err := idna.Lookup.ToASCII(value)
	if err != nil || len(ascii) > 253 || net.ParseIP(ascii) != nil {
		return "", invalid("域名格式无效")
	}
	labels := strings.Split(ascii, ".")
	if len(labels) < 2 {
		return "", invalid("域名必须为完整DNS名称")
	}
	for _, label := range labels {
		if !domainLabel.MatchString(label) {
			return "", invalid("域名格式无效")
		}
	}
	if slices.Contains([]string{"localhost", "local", "internal", "test", "invalid"}, labels[len(labels)-1]) {
		return "", invalid("不支持保留或本地域名")
	}
	return ascii, nil
}
