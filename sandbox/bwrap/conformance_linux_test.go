package bwrap

import (
	"testing"

	"github.com/nananek/goronation/core/sandbox"
	"github.com/nananek/goronation/sandbox/conformance"
	"github.com/nananek/goronation/sandbox/contract"
)

// TestConformance は、bwrap の Backend が、サンドボックス契約の適合テスト (sandbox/conformance) に通ることを確認する。
// 実際に bwrap を起動する: 使えなければ skip し、GORO_REQUIRE_BWRAP=1 の CI leg では fail する。
func TestConformance(t *testing.T) {
	conformance.Run(t, func(h contract.Host) sandbox.Backend { return New(h) }, conformance.RequireEnv(requireEnv))
}

// identityBackend は、bwrap を、path を付け替えられない (PathRemap = false) バックエンドとして宣言し直した、偽のバックエンド。
// 契約の検証は、GuestPath を HostPath と変えた Spec を断る。適合テストは、ホストと同じ配置の Spec を組んで、同じ項目を確かめる
// (macOS の seatbelt が使う配置を、bwrap で先に確かめる)。
type identityBackend struct{ *Backend }

func (identityBackend) Name() string { return "bwrap-identity" }

func (b identityBackend) Capabilities() sandbox.Capabilities {
	c := b.Backend.Capabilities()
	c.PathRemap = false
	return c
}

// TestConformanceIdentityLayout は、PathRemap = false の偽のバックエンドでも、適合テストが通ることを確認する
// (Cap つきの項目は、path-remap が「満たさない」ことを、GuestPath の拒否で確かめる)。
func TestConformanceIdentityLayout(t *testing.T) {
	conformance.Run(t, func(h contract.Host) sandbox.Backend {
		b := New(h)
		b.rules.Caps.PathRemap = false
		return identityBackend{b}
	}, conformance.RequireEnv(requireEnv))
}
