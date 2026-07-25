package connectrpc

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
)

type peerIdentityKey struct{}

func LoadServerTLSConfig(certFile, keyFile, caFile string) (*tls.Config, error) {
	certificate, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("加载服务端证书: %w", err)
	}
	clientCAs, err := loadCertificatePool(caFile)
	if err != nil {
		return nil, fmt.Errorf("加载客户端 CA: %w", err)
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{certificate},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientCAs,
	}, nil
}

func LoadClientTLSConfig(certFile, keyFile, caFile, serverName string) (*tls.Config, error) {
	certificate, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("加载客户端证书: %w", err)
	}
	rootCAs, err := loadCertificatePool(caFile)
	if err != nil {
		return nil, fmt.Errorf("加载服务端 CA: %w", err)
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{certificate},
		RootCAs:      rootCAs,
		ServerName:   serverName,
	}, nil
}

func loadCertificatePool(path string) (*x509.CertPool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return nil, fmt.Errorf("证书文件 %q 不包含有效证书", path)
	}
	return pool, nil
}

func PeerIdentityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			http.Error(w, "需要客户端证书", http.StatusUnauthorized)
			return
		}
		identity := r.TLS.PeerCertificates[0].Subject.CommonName
		if identity == "" {
			http.Error(w, "客户端证书缺少 Common Name", http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), peerIdentityKey{}, identity)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func PeerIdentity(ctx context.Context) (string, bool) {
	identity, ok := ctx.Value(peerIdentityKey{}).(string)
	return identity, ok && identity != ""
}
