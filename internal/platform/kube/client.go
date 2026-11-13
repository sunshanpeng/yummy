package kube

import (
	"fmt"
	"net/url"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// NewFromRESTConfig 仅使用调用者显式提供的受限凭据，不读取kubeconfig或环境默认集群。
// 安装凭据不允许执行本地插件，TLS必须验证；New保留显式客户端注入便于适配逻辑测试。
func NewFromRESTConfig(config *rest.Config, options Config) (*Adapter, error) {
	if config == nil {
		return nil, ErrInvalid
	}
	endpoint, err := url.Parse(config.Host)
	if err != nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || config.Insecure || config.ExecProvider != nil || config.AuthProvider != nil || config.Transport != nil || config.WrapTransport != nil || config.Proxy != nil || config.Dial != nil {
		return nil, fmt.Errorf("%w: 必须显式使用验证TLS的Kubernetes HTTPS受限凭据，禁止执行插件/自定义传输", ErrInvalid)
	}
	copy := rest.CopyConfig(config)
	copy.UserAgent = "yummy-worker-kube"
	if copy.Timeout == 0 {
		copy.Timeout = 30e9
	}
	client, err := kubernetes.NewForConfig(copy)
	if err != nil {
		return nil, err
	}
	return New(client, options)
}
