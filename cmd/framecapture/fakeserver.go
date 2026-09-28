//go:build linux

package main

import (
	"crypto/tls"
	"net"
	"net/http"
)

// fakeTLSServer は、ホストの loopback (OS が割り当てたポート) で、cert を使って TLS 終端する、
// 起動済みの fake サーバー。
type fakeTLSServer struct {
	Port int
	stop func()
}

// Close は、fake サーバーを止める。
func (f *fakeTLSServer) Close() { f.stop() }

// startFakeTLS は、hs (すでに Handler・ReadHeaderTimeout・ReadTimeout を設定済みの *http.Server。
// tools/fakeproviders/{anthropic,openai} の Server.NewHTTPServer が返すもの) を、cert で TLS 終端
// しながら、ホストの loopback (OS 割り当てポート) で起動する。
func startFakeTLS(cert *selfSignedCert, hs *http.Server) (*fakeTLSServer, error) {
	hs.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert.LeafCert}}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	go hs.ServeTLS(l, "", "") // 証明書は TLSConfig 経由で渡す済みなので、path は空でよい
	port := l.Addr().(*net.TCPAddr).Port
	return &fakeTLSServer{Port: port, stop: func() { hs.Close() }}, nil
}
