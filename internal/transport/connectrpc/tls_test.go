package connectrpc

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMutualTLSAndPeerIdentity(t *testing.T) {
	certificates := writeTestCertificates(t)
	serverTLS, err := LoadServerTLSConfig(certificates.serverCert, certificates.serverKey, certificates.caCert)
	if err != nil {
		t.Fatalf("LoadServerTLSConfig() error = %v", err)
	}
	clientTLS, err := LoadClientTLSConfig(
		certificates.workerCert,
		certificates.workerKey,
		certificates.caCert,
		"localhost",
	)
	if err != nil {
		t.Fatalf("LoadClientTLSConfig() error = %v", err)
	}

	handler := PeerIdentityMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, ok := PeerIdentity(r.Context())
		if !ok {
			http.Error(w, "missing identity", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(identity))
	}))
	server := httptest.NewUnstartedServer(handler)
	server.TLS = serverTLS
	server.StartTLS()
	defer server.Close()

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: clientTLS}}
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("GET with client certificate error = %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if string(body) != "worker-1" {
		t.Fatalf("peer identity = %q, want worker-1", body)
	}

	unauthenticatedTLS := clientTLS.Clone()
	unauthenticatedTLS.Certificates = nil
	_, err = (&http.Client{Transport: &http.Transport{TLSClientConfig: unauthenticatedTLS}}).Get(server.URL)
	if err == nil {
		t.Fatal("没有客户端证书的请求应失败")
	}
}

type testCertificates struct {
	caCert     string
	serverCert string
	serverKey  string
	workerCert string
	workerKey  string
}

func writeTestCertificates(t *testing.T) testCertificates {
	t.Helper()
	directory := t.TempDir()
	now := time.Now()
	caKey, caCert := createCertificate(t, nil, nil, &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Demo CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	})
	serverKey, serverCert := createCertificate(t, caKey, caCert, &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(time.Hour),
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		KeyUsage:     x509.KeyUsageDigitalSignature,
	})
	workerKey, workerCert := createCertificate(t, caKey, caCert, &x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject:      pkix.Name{CommonName: "worker-1"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(time.Hour),
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		KeyUsage:     x509.KeyUsageDigitalSignature,
	})

	files := testCertificates{
		caCert:     filepath.Join(directory, "ca.crt"),
		serverCert: filepath.Join(directory, "server.crt"),
		serverKey:  filepath.Join(directory, "server.key"),
		workerCert: filepath.Join(directory, "worker.crt"),
		workerKey:  filepath.Join(directory, "worker.key"),
	}
	writeCertificateFile(t, files.caCert, caCert.Raw)
	writeCertificateFile(t, files.serverCert, serverCert.Raw)
	writePrivateKeyFile(t, files.serverKey, serverKey)
	writeCertificateFile(t, files.workerCert, workerCert.Raw)
	writePrivateKeyFile(t, files.workerKey, workerKey)
	return files
}

func createCertificate(
	t *testing.T,
	parentKey ed25519.PrivateKey,
	parent *x509.Certificate,
	template *x509.Certificate,
) (ed25519.PrivateKey, *x509.Certificate) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	if parent == nil {
		parent = template
		parentKey = privateKey
	}
	der, err := x509.CreateCertificate(rand.Reader, template, parent, publicKey, parentKey)
	if err != nil {
		t.Fatalf("CreateCertificate() error = %v", err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("ParseCertificate() error = %v", err)
	}
	return privateKey, certificate
}

func writeCertificateFile(t *testing.T, path string, der []byte) {
	t.Helper()
	data := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
}

func writePrivateKeyFile(t *testing.T, path string, key ed25519.PrivateKey) {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey() error = %v", err)
	}
	data := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
}
