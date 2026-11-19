package oauth

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/sunshanpeng/yummy/internal/domains/audit"
	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type ClientInput struct {
	ID, Name                        string
	RedirectURIs, Scopes, Resources []string
}

func (s *Service) RegisterClient(ctx context.Context, p authorization.Principal, input ClientInput) error {
	if input.ID == "" || len(input.ID) > 2048 || input.Name == "" || len(input.Name) > 200 || len(input.RedirectURIs) == 0 || len(input.RedirectURIs) > 20 || len(input.Resources) == 0 || len(input.Resources) > 100 {
		return ErrClient
	}
	for _, uri := range input.RedirectURIs {
		if !validRedirect(uri) {
			return ErrClient
		}
	}
	for _, scope := range input.Scopes {
		if scope != OfflineAccess && !slices.Contains(authorization.TokenScopes, scope) {
			return ErrClient
		}
	}
	for _, resource := range input.Resources {
		if _, err := s.resourceApplications(resource); err != nil {
			return ErrClient
		}
	}
	// proto3 空数组反序列化为 nil，GORM jsonb 会落 NULL 触发 23502；注册语义允许
	// 留空 scope（客户端可后续授权时再选），必须显式落空数组。
	if input.Scopes == nil {
		input.Scopes = []string{}
	}
	return s.access.Do(ctx, p, true, func(tx *gorm.DB, current authorization.Principal) error {
		if err := s.access.RequireInstance(current); err != nil {
			return err
		}
		client := clientRecord{ID: input.ID, Name: input.Name, Source: "pre-registered", RedirectURIs: input.RedirectURIs, Scopes: input.Scopes, Resources: input.Resources, Active: true}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&client).Error; err != nil {
			return err
		}
		return audit.Record(tx, current.UserID, "instance.oauth.client.register", "oauth_client", input.ID, nil, nil)
	})
}
func validRedirect(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Host != "" && u.User == nil && u.Fragment == "" && (u.Scheme == "https" || u.Scheme == "http" && isLoopback(u.Hostname()))
}
func (s *Service) ensureClient(ctx context.Context, id, resource string) error {
	var client clientRecord
	err := s.db.WithContext(ctx).First(&client, "id = ?", id).Error
	if err == nil {
		if !client.Active {
			return ErrClient
		}
		if !slices.Contains(client.Resources, resource) {
			if client.Source != "cimd-unverified-publisher" {
				return ErrClient
			}
			// CIMD 描述客户端身份与回调，不代表授权；只为本实例规范资源登记候选，
			// 最终范围仍须用户同意并由当前应用权限校验。
			return appendClientResources(s.db.WithContext(ctx), id, resource)
		}
		return nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	input, err := fetchCIMD(ctx, id)
	if err != nil {
		return err
	}
	if _, err := s.resourceApplications(resource); err != nil {
		return ErrClient
	}
	record := clientRecord{ID: id, Name: input.Name, Source: "cimd-unverified-publisher", RedirectURIs: input.RedirectURIs, Scopes: input.Scopes, Resources: []string{resource}, Active: true}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&record).Error
}

// fetchCIMD 使用专用传输器。DNS 解析结果逐个校验且直连已验 IP，阻断 DNS rebinding；
// 不继承环境代理，不跟重定向，不允许内网、metadata 或回环地址。
func fetchCIMD(ctx context.Context, id string) (ClientInput, error) {
	u, err := url.Parse(id)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" || len(id) > 2048 {
		return ClientInput{}, ErrClient
	}
	transport := &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, ErrClient
		}
		addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil || len(addresses) == 0 {
			return nil, ErrClient
		}
		for _, candidate := range addresses {
			if !publicIP(candidate.IP) {
				return nil, ErrClient
			}
		}
		dialer := net.Dialer{Timeout: 3 * time.Second}
		return dialer.DialContext(ctx, network, net.JoinHostPort(addresses[0].IP.String(), port))
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: 5 * time.Second, Transport: transport, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return ErrClient }}
	request, err := http.NewRequestWithContext(ctx, "GET", id, nil)
	if err != nil {
		return ClientInput{}, ErrClient
	}
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return ClientInput{}, ErrClient
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return ClientInput{}, ErrClient
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	if err != nil || len(raw) > 64<<10 {
		return ClientInput{}, ErrClient
	}
	return parseCIMD(id, raw)
}

func jsonArray(value string) string { raw, _ := json.Marshal([]string{value}); return string(raw) }
func parseCIMD(id string, raw []byte) (ClientInput, error) {
	if len(raw) > 64<<10 {
		return ClientInput{}, ErrClient
	}
	var metadata struct {
		ClientID      string   `json:"client_id"`
		ClientName    string   `json:"client_name"`
		RedirectURIs  []string `json:"redirect_uris"`
		Scope         string   `json:"scope"`
		AuthMethod    string   `json:"token_endpoint_auth_method"`
		GrantTypes    []string `json:"grant_types"`
		ResponseTypes []string `json:"response_types"`
	}
	if err := json.Unmarshal(raw, &metadata); err != nil || metadata.ClientID != id || metadata.ClientName == "" || len(metadata.ClientName) > 200 || len(metadata.RedirectURIs) == 0 || len(metadata.RedirectURIs) > 20 || metadata.AuthMethod != "none" {
		return ClientInput{}, ErrClient
	}
	for _, uri := range metadata.RedirectURIs {
		if !validRedirect(uri) {
			return ClientInput{}, ErrClient
		}
	}
	for _, grant := range metadata.GrantTypes {
		if grant != "authorization_code" && grant != "refresh_token" {
			return ClientInput{}, ErrClient
		}
	}
	for _, response := range metadata.ResponseTypes {
		if response != "code" {
			return ClientInput{}, ErrClient
		}
	}
	scopes := splitScopes(metadata.Scope)
	if len(scopes) == 0 {
		scopes = []string{"application:read", "build:read", "logs:read", "runtime:read"}
	}
	for _, scope := range scopes {
		if scope != OfflineAccess && !slices.Contains(authorization.TokenScopes, scope) {
			return ClientInput{}, ErrClient
		}
	}
	return ClientInput{ID: id, Name: metadata.ClientName, RedirectURIs: metadata.RedirectURIs, Scopes: scopes}, nil
}
func publicIP(ip net.IP) bool {
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return false
	}
	for _, prefix := range []string{"100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "2001:db8::/32", "2001::/32", "2002::/16"} {
		_, block, _ := net.ParseCIDR(prefix)
		if block.Contains(ip) {
			return false
		}
	}
	return true
}
