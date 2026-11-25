package buildrunner

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// ValidateBuildArgs限制buildctl的受支持子集；不允许用户注入addr/entitlement/cache/ssh/executor。
// 参数仍是数组，即使普通build-arg含空格/分号也不会作为Shell执行。
// 两种构建方式各有固定形态：dockerfile.v0 构建 Dockerfile，gateway.v0 只用于固定的 Railpack 前端。
func ValidateBuildArgs(args []string) error { return validateBuildArgs(args, fixedPaths()) }
func validateBuildArgs(args []string, p paths) error {
	if len(args) < 2 || len(args) > 256 || args[0] != "build" {
		return ErrInvalid
	}
	for _, arg := range args[1:] {
		if value, ok := strings.CutPrefix(arg, "--frontend="); ok {
			switch value {
			case "dockerfile.v0":
				return validateDockerfileBuildArgs(args, p)
			case "gateway.v0":
				return validateRailpackBuildArgs(args, p)
			}
			return ErrInvalid
		}
	}
	return ErrInvalid
}

// validateRailpackBuildArgs 只接受 Railpack 前端的固定形态：计划目录固定为 RailpackPlanDir，
// 前端镜像必须 digest 钉住，只允许 secrets-hash 与 cache-key 两个前端参数，不允许目标阶段或任意 build-arg。
func validateRailpackBuildArgs(args []string, p paths) error {
	unique := map[string]bool{}
	secrets := map[string]bool{}
	secretFiles := map[string]bool{}
	contextPath, planDir := "", ""
	for _, arg := range args[1:] {
		if len(arg) > 16384 || strings.ContainsRune(arg, 0) {
			return ErrInvalid
		}
		key, value, ok := strings.Cut(arg, "=")
		if !ok {
			return ErrInvalid
		}
		switch key {
		case "--frontend":
			if value != "gateway.v0" || unique[key] {
				return ErrInvalid
			}
			unique[key] = true
		case "--metadata-file":
			if value != p.metadata || unique[key] {
				return ErrInvalid
			}
			unique[key] = true
		case "--local":
			kind, path, ok := strings.Cut(value, "=")
			if !ok || (kind != "context" && kind != "dockerfile") || unique[key+kind] || strings.ContainsAny(path, "\x00\r\n") {
				return ErrInvalid
			}
			unique[key+kind] = true
			clean := filepath.Clean(path)
			source := filepath.Join(p.workspace, "source")
			if kind == "context" {
				if clean != source && !strings.HasPrefix(clean, source+string(filepath.Separator)) {
					return ErrInvalid
				}
				contextPath = clean
			} else {
				// 计划文件所在目录固定，不能指向源码或其他位置。
				if clean != p.plan {
					return ErrInvalid
				}
				planDir = clean
			}
		case "--opt":
			kind, val, ok := strings.Cut(value, "=")
			if !ok || unique[kind] {
				return ErrInvalid
			}
			switch kind {
			case "source":
				if !railpackImagePattern.MatchString(val) {
					return fmt.Errorf("%w: Railpack 前端镜像必须以 digest 钉住", ErrInvalid)
				}
			case "filename":
				if val != RailpackPlanFile {
					return ErrInvalid
				}
			case "platform":
				if val != "linux/amd64" {
					return ErrInvalid
				}
			case "build-arg:secrets-hash":
				if !digestHexPattern.MatchString(val) {
					return ErrInvalid
				}
			case "build-arg:cache-key":
				if !cacheKeyPattern.MatchString(val) {
					return ErrInvalid
				}
			default:
				return fmt.Errorf("%w: 不允许的BuildKit opt", ErrInvalid)
			}
			unique[kind] = true
		case "--output":
			const prefix = "type=image,name="
			const suffix = ",push=true"
			if unique[key] || !strings.HasPrefix(value, prefix) || !strings.HasSuffix(value, suffix) {
				return ErrInvalid
			}
			unique[key] = true
			image := strings.TrimSuffix(strings.TrimPrefix(value, prefix), suffix)
			if strings.ContainsAny(image, " \t\r\n,@;") || strings.LastIndex(image, ":") <= strings.LastIndex(image, "/") || strings.HasSuffix(image, ":latest") {
				return ErrInvalid
			}
		case "--secret":
			parts := strings.Split(value, ",")
			if len(parts) != 2 || !strings.HasPrefix(parts[0], "id=") || !strings.HasPrefix(parts[1], "src=") {
				return ErrInvalid
			}
			id := strings.TrimPrefix(parts[0], "id=")
			file := strings.TrimPrefix(parts[1], "src=")
			base := filepath.Base(file)
			if !railpackEnvName.MatchString(id) || secrets[id] || secretFiles[file] || filepath.Dir(file) != p.buildAuth || !validMountFile(base) {
				return ErrInvalid
			}
			secrets[id] = true
			secretFiles[file] = true
		default:
			return fmt.Errorf("%w: 不允许的BuildKit参数", ErrInvalid)
		}
	}
	for _, key := range []string{"--frontend", "--metadata-file", "--localcontext", "--localdockerfile", "source", "filename", "platform", "build-arg:secrets-hash", "build-arg:cache-key", "--output"} {
		if !unique[key] {
			return ErrInvalid
		}
	}
	if contextPath == "" || planDir == "" {
		return ErrInvalid
	}
	return nil
}

// validMountFile 判断 Railpack 挂载文件名：构建密钥 secret-<序号>，非敏感构建参数 arg-<序号>。
func validMountFile(base string) bool {
	for _, prefix := range []string{"secret-", "arg-"} {
		if digits, ok := strings.CutPrefix(base, prefix); ok {
			index, err := strconv.Atoi(digits)
			return err == nil && index >= 0 && index < 100 && digits == strconv.Itoa(index)
		}
	}
	return false
}

func validateDockerfileBuildArgs(args []string, p paths) error {
	unique := map[string]bool{}
	buildArgs := map[string]bool{}
	secrets := map[string]bool{}
	secretFiles := map[string]bool{}
	contextPath, dockerfileDir, filename := "", "", ""
	for _, arg := range args[1:] {
		if len(arg) > 16384 || strings.ContainsRune(arg, 0) {
			return ErrInvalid
		}
		key, value, ok := strings.Cut(arg, "=")
		if !ok {
			return ErrInvalid
		}
		switch key {
		case "--frontend":
			if value != "dockerfile.v0" || unique[key] {
				return ErrInvalid
			}
			unique[key] = true
		case "--metadata-file":
			if value != p.metadata || unique[key] {
				return ErrInvalid
			}
			unique[key] = true
		case "--local":
			kind, path, ok := strings.Cut(value, "=")
			if !ok || (kind != "context" && kind != "dockerfile") || unique[key+kind] {
				return ErrInvalid
			}
			unique[key+kind] = true
			clean := filepath.Clean(path)
			source := filepath.Join(p.workspace, "source")
			if clean != source && !strings.HasPrefix(clean, source+string(filepath.Separator)) {
				return ErrInvalid
			}
			if strings.ContainsAny(path, "\x00\r\n") {
				return ErrInvalid
			}
			if kind == "context" {
				contextPath = clean
			} else {
				dockerfileDir = clean
			}
		case "--opt":
			kind, val, ok := strings.Cut(value, "=")
			if !ok {
				return ErrInvalid
			}
			switch {
			case kind == "filename":
				if !safeRelative(val) || strings.Contains(val, "/") || unique[kind] {
					return ErrInvalid
				}
				filename = val
				unique[kind] = true
			case kind == "platform":
				if val != "linux/amd64" || unique[kind] {
					return ErrInvalid
				}
				unique[kind] = true
			case kind == "target":
				if !identifierPattern.MatchString(val) || unique[kind] {
					return ErrInvalid
				}
				unique[kind] = true
			case strings.HasPrefix(kind, "build-arg:"):
				name := strings.TrimPrefix(kind, "build-arg:")
				if !identifierPattern.MatchString(name) || buildArgs[name] || len(val) > 8192 {
					return ErrInvalid
				}
				buildArgs[name] = true
			default:
				return fmt.Errorf("%w: 不允许的BuildKit opt", ErrInvalid)
			}
		case "--output":
			const prefix = "type=image,name="
			const suffix = ",push=true"
			if unique[key] || !strings.HasPrefix(value, prefix) || !strings.HasSuffix(value, suffix) {
				return ErrInvalid
			}
			unique[key] = true
			image := strings.TrimSuffix(strings.TrimPrefix(value, prefix), suffix)
			if strings.ContainsAny(image, " \t\r\n,@;") || strings.LastIndex(image, ":") <= strings.LastIndex(image, "/") || strings.HasSuffix(image, ":latest") {
				return ErrInvalid
			}
		case "--secret":
			parts := strings.Split(value, ",")
			if len(parts) != 2 || !strings.HasPrefix(parts[0], "id=") || !strings.HasPrefix(parts[1], "src=") {
				return ErrInvalid
			}
			id := strings.TrimPrefix(parts[0], "id=")
			file := strings.TrimPrefix(parts[1], "src=")
			base := filepath.Base(file)
			index, err := strconv.Atoi(strings.TrimPrefix(base, "secret-"))
			if !identifierPattern.MatchString(id) || secrets[id] || secretFiles[file] || filepath.Dir(file) != p.buildAuth || err != nil || index < 0 || index >= 50 || base != "secret-"+strconv.Itoa(index) {
				return ErrInvalid
			}
			secrets[id] = true
			secretFiles[file] = true
		default:
			return fmt.Errorf("%w: 不允许的BuildKit参数", ErrInvalid)
		}
	}
	for _, key := range []string{"--frontend", "--metadata-file", "--localcontext", "--localdockerfile", "filename", "platform", "--output"} {
		if !unique[key] {
			return ErrInvalid
		}
	}
	if contextPath == "" || dockerfileDir == "" || filename == "" || dockerfileDir != contextPath && !strings.HasPrefix(dockerfileDir, contextPath+string(filepath.Separator)) {
		return ErrInvalid
	}
	return nil
}
