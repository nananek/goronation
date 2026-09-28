//go:build linux

package main

import (
	"crypto/tls"
	"crypto/x509"
	"testing"
)

func TestNewSelfSignedCertVerifiesForHost(t *testing.T) {
	cert, err := newSelfSignedCert("fake-openai.test")
	if err != nil {
		t.Fatalf("newSelfSignedCert: %v", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(cert.CAPEM) {
		t.Fatal("CAPEM を CertPool に追加できない")
	}

	leaf, err := x509.ParseCertificate(cert.LeafCert.Certificate[0])
	if err != nil {
		t.Fatalf("leaf 証明書を読めない: %v", err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: "fake-openai.test", Roots: pool}); err != nil {
		t.Fatalf("leaf.Verify: %v", err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: "other-host.test", Roots: pool}); err == nil {
		t.Fatal("別のホスト名でも検証が通ってしまった (SAN が広すぎる)")
	}
}

func TestNewSelfSignedCertRejectedByHostWithoutCA(t *testing.T) {
	cert, err := newSelfSignedCert("fake-openai.test")
	if err != nil {
		t.Fatalf("newSelfSignedCert: %v", err)
	}
	leaf, err := x509.ParseCertificate(cert.LeafCert.Certificate[0])
	if err != nil {
		t.Fatalf("leaf 証明書を読めない: %v", err)
	}
	// ホストの既定の CA プールでは検証できない (自己署名の CA を明示的に信頼させないと、他のプロセスは
	// 信頼しないはず)。
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: "fake-openai.test"}); err == nil {
		t.Fatal("ホストの既定の CA プールだけで検証が通ってしまった (自己署名のはずなのに)")
	}
}

func TestNewSelfSignedCertUsableForTLSServer(t *testing.T) {
	cert, err := newSelfSignedCert("fake-openai.test")
	if err != nil {
		t.Fatalf("newSelfSignedCert: %v", err)
	}
	// tls.Config.Certificates にそのまま渡せる形であることを確かめる (ServeTLS の前提)。
	_ = tls.Config{Certificates: []tls.Certificate{cert.LeafCert}}
	if len(cert.LeafCert.Certificate) == 0 {
		t.Fatal("LeafCert.Certificate が空")
	}
	if cert.LeafCert.PrivateKey == nil {
		t.Fatal("LeafCert.PrivateKey が nil")
	}
}
