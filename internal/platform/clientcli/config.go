// Package clientcli 提供通过正式产品 API 操作的按需客户端。
package clientcli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const maxConfigBytes = 1 << 20

var profilePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

type profile struct {
	Server       string    `json:"server"`
	Workspace    string    `json:"workspace,omitempty"`
	Application  string    `json:"application,omitempty"`
	Token        string    `json:"token,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	TokenExpiry  time.Time `json:"token_expiry,omitempty"`
}

type publicProfile struct {
	Name          string `json:"name"`
	Server        string `json:"server"`
	Workspace     string `json:"workspace,omitempty"`
	Application   string `json:"application,omitempty"`
	HasCredential bool   `json:"has_credential"`
}

type configStore struct{ dir string }

func defaultConfigDir() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "yummy"), nil
}

func validProfile(name string) bool { return profilePattern.MatchString(name) }

// validateServer 只接受明确实例根地址；拒绝用户信息和可能混淆令牌受众的路径。
func validateServer(raw string, allowLocalHTTP bool) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") || u.RawPath != "" || u.Opaque != "" {
		return "", errors.New("实例地址必须是不含路径、凭据、查询或片段的 URL")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", errors.New("实例地址必须使用 HTTPS")
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || strings.Contains(host, "%") {
		return "", errors.New("实例地址主机无效")
	}
	if u.Scheme == "http" {
		ip := net.ParseIP(host)
		if !allowLocalHTTP || !(host == "localhost" || (ip != nil && (ip.Equal(net.ParseIP("127.0.0.1")) || ip.Equal(net.ParseIP("::1"))))) {
			return "", errors.New("HTTP 仅可在显式启用时连接回环地址")
		}
	}
	if port := u.Port(); port != "" {
		value, err := strconv.Atoi(port)
		if err != nil || value < 1 || value > 65535 {
			return "", errors.New("实例地址端口无效")
		}
	}
	u.Path = ""
	u.Host = strings.ToLower(u.Host)
	return u.String(), nil
}

func (s configStore) read(name string) (profile, error) {
	var result profile
	if !validProfile(name) {
		return result, errors.New("profile 名称只能包含字母、数字、下划线和连字符")
	}
	err := s.withLock(func(root *os.Root) error {
		file, err := openSecureFile(root, name+".json", os.O_RDONLY, false)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, maxConfigBytes+1))
		if err != nil {
			return err
		}
		if len(data) > maxConfigBytes {
			return errors.New("profile 文件过大")
		}
		return json.Unmarshal(data, &result)
	})
	return result, err
}

func (s configStore) update(name string, update func(*profile) error) error {
	_, err := s.change(name, update)
	return err
}

func (s configStore) change(name string, update func(*profile) error) (profile, error) {
	var current profile
	if !validProfile(name) {
		return current, errors.New("profile 名称只能包含字母、数字、下划线和连字符")
	}
	err := s.withLock(func(root *os.Root) error {
		file, err := openSecureFile(root, name+".json", os.O_RDONLY, false)
		if err == nil {
			data, e := io.ReadAll(io.LimitReader(file, maxConfigBytes+1))
			_ = file.Close()
			if e != nil {
				return e
			}
			if len(data) > maxConfigBytes {
				return errors.New("profile 文件过大")
			}
			if e = json.Unmarshal(data, &current); e != nil {
				return e
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := update(&current); err != nil {
			return err
		}
		data, err := json.Marshal(current)
		if err != nil {
			return err
		}
		return atomicSecureWrite(root, name+".json", data)
	})
	return current, err
}

func (s configStore) list() ([]publicProfile, error) {
	result := []publicProfile{}
	var names []string
	err := s.withLock(func(root *os.Root) error {
		dir, err := root.Open(".")
		if err != nil {
			return err
		}
		defer dir.Close()
		entries, err := dir.ReadDir(-1)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			name, ok := strings.CutSuffix(entry.Name(), ".json")
			if ok && validProfile(name) {
				names = append(names, name)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		value, err := s.read(name)
		if err != nil {
			return nil, err
		}
		result = append(result, publicProfile{name, value.Server, value.Workspace, value.Application, value.Token != ""})
	}
	return result, nil
}

func publicView(name string, p profile) publicProfile {
	return publicProfile{name, p.Server, p.Workspace, p.Application, p.Token != ""}
}

func readToken(r io.Reader) (string, error) {
	data, err := io.ReadAll(io.LimitReader(r, 8193))
	if err != nil {
		return "", err
	}
	if len(data) > 8192 {
		return "", errors.New("令牌过长")
	}
	token := strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	if token == "" || strings.ContainsAny(token, "\r\n \t") {
		return "", errors.New("令牌不能为空或包含空白字符")
	}
	return token, nil
}

func configError(err error) error {
	return fail("CONFIG_ERROR", 2, false, fmt.Sprintf("配置无效：%v", err))
}
