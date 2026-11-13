package buildrunner

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// ValidateBuildArgs限制buildctl的受支持子集；不允许用户注入addr/entitlement/cache/ssh/executor。
// 参数仍是数组，即使普通build-arg含空格/分号也不会作为Shell执行。
func ValidateBuildArgs(args []string) error { return validateBuildArgs(args, fixedPaths()) }
func validateBuildArgs(args []string, p paths) error {
	if len(args) < 2 || len(args) > 256 || args[0] != "build" {
		return ErrInvalid
	}
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
