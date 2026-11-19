package enrollment

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/hex"
	"encoding/pem"
	"io"
	"math/big"
	"os"
	"time"
)

func loadCA(config Config) (*x509.Certificate, crypto.Signer, []byte, error) {
	certificateData, err := readCAFile(config.CACertificatePath, false)
	if err != nil {
		return nil, nil, nil, ErrCA
	}
	keyData, err := readCAFile(config.CAPrivateKeyPath, true)
	if err != nil {
		return nil, nil, nil, ErrCA
	}
	defer clear(keyData)
	block, rest := pem.Decode(certificateData)
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, nil, nil, ErrCA
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil || !certificate.IsCA || !certificate.BasicConstraintsValid || certificate.KeyUsage&x509.KeyUsageCertSign == 0 {
		return nil, nil, nil, ErrCA
	}
	now := time.Now()
	if now.Before(certificate.NotBefore) || now.Add(CertificateLifetime).After(certificate.NotAfter) {
		return nil, nil, nil, ErrCA
	}
	block, rest = pem.Decode(keyData)
	if block == nil || len(bytes.TrimSpace(rest)) != 0 || x509.IsEncryptedPEMBlock(block) {
		return nil, nil, nil, ErrCA
	}
	defer clear(block.Bytes)
	var parsed any
	switch block.Type {
	case "PRIVATE KEY":
		parsed, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	case "RSA PRIVATE KEY":
		parsed, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	case "EC PRIVATE KEY":
		parsed, err = x509.ParseECPrivateKey(block.Bytes)
	default:
		return nil, nil, nil, ErrCA
	}
	if err != nil {
		return nil, nil, nil, ErrCA
	}
	signer, ok := parsed.(crypto.Signer)
	if !ok || !strongPublicKey(signer.Public()) {
		return nil, nil, nil, ErrCA
	}
	publicDER, err := x509.MarshalPKIXPublicKey(signer.Public())
	if err != nil || !bytes.Equal(publicDER, certificate.RawSubjectPublicKeyInfo) {
		return nil, nil, nil, ErrCA
	}
	return certificate, signer, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw}), nil
}

// 只接受单个普通文件；私钥至少 0600（或0400），不接受链接和组/其他用户权限。
func readCAFile(filename string, private bool) ([]byte, error) {
	before, err := os.Lstat(filename)
	if err != nil || !before.Mode().IsRegular() || (private && before.Mode().Perm()&0077 != 0) {
		return nil, ErrCA
	}
	file, err := openCAFile(filename)
	if err != nil {
		return nil, ErrCA
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) || !after.Mode().IsRegular() || after.Size() > 1<<20 || after.Size() == 0 {
		return nil, ErrCA
	}
	if private && (after.Mode().Perm()&0077 != 0 || !singleLink(after)) {
		return nil, ErrCA
	}
	content := make([]byte, after.Size()+1)
	n, err := io.ReadFull(file, content)
	if err != io.ErrUnexpectedEOF || int64(n) != after.Size() {
		clear(content)
		return nil, ErrCA
	}
	return content[:n], nil
}

func strongPublicKey(key any) bool {
	switch public := key.(type) {
	case *rsa.PublicKey:
		return public.N != nil && public.N.BitLen() >= 2048 && public.E >= 65537 && public.E%2 == 1
	case *ecdsa.PublicKey:
		return (public.Curve == elliptic.P256() || public.Curve == elliptic.P384() || public.Curve == elliptic.P521()) && public.X != nil && public.Y != nil && public.Curve.IsOnCurve(public.X, public.Y)
	case ed25519.PublicKey:
		return len(public) == ed25519.PublicKeySize
	}
	return false
}

func parseCSR(input []byte) (*x509.CertificateRequest, error) {
	if len(input) == 0 || len(input) > maxCSRBytes {
		return nil, ErrCSR
	}
	block, rest := pem.Decode(input)
	if block == nil || block.Type != "CERTIFICATE REQUEST" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, ErrCSR
	}
	request, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil || request.CheckSignature() != nil || !strongPublicKey(request.PublicKey) {
		return nil, ErrCSR
	}
	// 不复制 CSR 的身份或扩展。显式拒绝 SAN 和 CA 权限，避免调用者误认为请求被保留。
	if len(request.DNSNames) != 0 || len(request.IPAddresses) != 0 || len(request.EmailAddresses) != 0 || len(request.URIs) != 0 {
		return nil, ErrCSR
	}
	for _, extension := range request.Extensions {
		if extension.Id.Equal(asn1.ObjectIdentifier{2, 5, 29, 19}) {
			return nil, ErrCSR
		}
		if extension.Id.Equal(asn1.ObjectIdentifier{2, 5, 29, 15}) {
			var usage asn1.BitString
			if rest, err := asn1.Unmarshal(extension.Value, &usage); err != nil || len(rest) != 0 || usage.At(5) != 0 || usage.At(6) != 0 {
				return nil, ErrCSR
			}
		}
		if extension.Critical && !extension.Id.Equal(asn1.ObjectIdentifier{2, 5, 29, 15}) {
			return nil, ErrCSR
		}
	}
	return request, nil
}

func (s *Service) issue(node nodeRecord, request *x509.CertificateRequest, now time.Time, rotated *string) (certificateRecord, IssuedCertificate, error) {
	if now.Before(s.ca.NotBefore) || now.Add(CertificateLifetime).After(s.ca.NotAfter) {
		return certificateRecord{}, IssuedCertificate{}, ErrCA
	}
	serialBytes := make([]byte, 20)
	if _, err := rand.Read(serialBytes); err != nil {
		return certificateRecord{}, IssuedCertificate{}, err
	}
	serialBytes[0] &= 0x7f
	serial := new(big.Int).SetBytes(serialBytes)
	if serial.Sign() == 0 {
		serial.SetInt64(1)
	}
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: node.WorkerID}, NotBefore: now.Add(-time.Minute).Truncate(time.Second), NotAfter: now.Add(CertificateLifetime).Truncate(time.Second), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, s.ca, request.PublicKey, s.signer)
	if err != nil {
		return certificateRecord{}, IssuedCertificate{}, err
	}
	certificateHash := sha256.Sum256(der)
	publicDER, err := x509.MarshalPKIXPublicKey(request.PublicKey)
	if err != nil {
		return certificateRecord{}, IssuedCertificate{}, err
	}
	publicHash := sha256.Sum256(publicDER)
	record := certificateRecord{Serial: serial.Text(16), NodeID: node.ID, WorkerID: node.WorkerID, CertificateSHA256: hex.EncodeToString(certificateHash[:]), PublicKeySHA256: hex.EncodeToString(publicHash[:]), NotBefore: template.NotBefore, NotAfter: template.NotAfter, IssuedAt: now, RotatedFrom: rotated}
	issued := IssuedCertificate{Identity: identity(record), CertificatePEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), CACertificatePEM: bytes.Clone(s.caPEM), NotBefore: record.NotBefore, RotateAfter: record.NotAfter.Add(-RotationWindow)}
	return record, issued, nil
}
