//go:build linux

package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"
)

// selfSignedCert は、fake サーバーが TLS で応答するための、使い捨ての自己署名 CA + leaf 証明書
// (host 1 つだけの SAN)。egress・cmd/goronation のどちらのホストの信頼ストアも汚染しない: CA の PEM は、
// 檻の中の子プロセスに NODE_EXTRA_CA_CERTS で個別に教える (ホストの /etc/ssl/certs には触れない)。
type selfSignedCert struct {
	// CAPEM は、CA 証明書 (PEM)。子プロセスの NODE_EXTRA_CA_CERTS に渡すファイルの中身。
	CAPEM []byte
	// LeafCert は、fake サーバーが tls.Config.Certificates に使う、host 用の証明書 (leaf + CA を鎖にした PEM)。
	LeafCert tls.Certificate
}

// newSelfSignedCert は、host (SAN に 1 つだけ入れる DNS 名) 用の CA + leaf 証明書を、その場で作る。
// 有効期限は短く (1 時間)、鍵は ECDSA P-256 (生成が速い)。M0 スパイクの使い捨て証明書で、どこにも
// 保存しない。
func newSelfSignedCert(host string) (*selfSignedCert, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("CA 鍵を生成できない: %w", err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "framecapture-test-ca"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("CA 証明書を作れない: %w", err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})

	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, fmt.Errorf("CA 証明書を読めない: %w", err)
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("leaf 鍵を生成できない: %w", err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("leaf 証明書を作れない: %w", err)
	}
	leafKeyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		return nil, fmt.Errorf("leaf 鍵を PKCS 化できない: %w", err)
	}
	leafKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: leafKeyDER})
	leafCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})

	tlsCert, err := tls.X509KeyPair(leafCertPEM, leafKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("tls.Certificate を組み立てられない: %w", err)
	}
	return &selfSignedCert{CAPEM: caPEM, LeafCert: tlsCert}, nil
}
