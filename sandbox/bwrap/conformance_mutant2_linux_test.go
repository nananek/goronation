//go:build linux

package bwrap

import (
	"context"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/nananek/goronation/core/sandbox"
	"github.com/nananek/goronation/sandbox/conformance"
	"github.com/nananek/goronation/sandbox/contract"
)

// conformance_mutant_linux_test.go (レビューで取り込んだもの) の続き: 適合テストの修正 (B3・B4) が、もっと広い壊れ方も赤にすることを確かめる。
// 変異は、環境変数 mutant2Env で選ぶ。変異を入れた適合テストは、別プロセスで走らせて、指定の項目が赤になることを確かめる。
const mutant2Env = "GORO_CONFORMANCE_MUTANT2"

// declaresCredEnv は、資格情報らしい名前 (GH_TOKEN) を、ExtraEnv に宣言するバックエンド。
type declaresCredEnv struct{ *Backend }

func (b declaresCredEnv) Capabilities() sandbox.Capabilities {
	c := b.Backend.Capabilities()
	c.ExtraEnv = append(slices.Clone(c.ExtraEnv), "GH_TOKEN")
	return c
}

// leaksCredEnv は、ホストの GH_TOKEN を、Spec.Env に足して起動するバックエンド (宣言なし。契約の検証が断るはずだが、断らない実装の見立て)。
type leaksCredEnv struct{ *Backend }

func (b leaksCredEnv) Start(ctx context.Context, s sandbox.Spec) (sandbox.Cage, error) {
	s.Env = append(slices.Clone(s.Env), sandbox.EnvVar{Key: "GH_TOKEN", Value: os.Getenv("GH_TOKEN")})
	return b.Backend.Start(ctx, s)
}

// TestConformanceMutant2Inner は、mutant2Env の変異を入れた bwrap のバックエンドで、適合テストを走らせる (変異が無ければ何もしない)。
func TestConformanceMutant2Inner(t *testing.T) {
	mutant := os.Getenv(mutant2Env)
	if mutant == "" {
		t.Skipf("%s が無い (TestConformanceCatchesMutants2 が使う)", mutant2Env)
	}
	nb := func(h contract.Host) sandbox.Backend { return New(h) }
	switch mutant {
	case "fd-3to255":
		// 継承した fd を、3〜255 だけ close-on-exec にする (256 以降は、檻に届く)。
		closeInheritedFDs = func() error {
			for fd := 3; fd <= 255; fd++ {
				syscall.CloseOnExec(fd)
			}
			return nil
		}
	case "fd-10plus":
		// 継承した fd を、10〜1023 だけ close-on-exec にする (3〜9 は、檻に届く)。
		closeInheritedFDs = func() error {
			for fd := 10; fd <= 1023; fd++ {
				syscall.CloseOnExec(fd)
			}
			return nil
		}
	case "declared-cred-env":
		nb = func(h contract.Host) sandbox.Backend { return declaresCredEnv{New(h)} }
	case "leaks-cred-env":
		nb = func(h contract.Host) sandbox.Backend { return leaksCredEnv{New(h)} }
	default:
		t.Fatalf("未知の変異 %q", mutant)
	}
	conformance.Run(t, nb, conformance.RequireEnv(requireEnv))
}

// TestConformanceCatchesMutants2 は、壊れたバックエンドが、適合テストに合格しない (指定の項目が赤くなる) ことを確認する。
func TestConformanceCatchesMutants2(t *testing.T) {
	needBwrap(t)
	for mutant, item := range map[string]string{
		"fd-3to255":         "inherited-fd", // 固定の範囲だけを閉じる実装 (256 以降の fd が届く)
		"fd-10plus":         "inherited-fd", // 低い番号の fd (3〜9) を閉じ忘れる実装
		"declared-cred-env": "env-clean",    // 資格情報らしい名前を、ExtraEnv に宣言しても許されない
		"leaks-cred-env":    "env-clean",    // ホストの資格情報を、檻に渡す
	} {
		t.Run(mutant, func(t *testing.T) {
			// 内側では、指定の項目だけを動かす (適合テスト全体は、-race で 1 回 約 16 秒かかる)。親の死・fd の役の再実行は、Run を呼んだテストの名前だけを指すので、影響しない。
			cmd := exec.Command(os.Args[0], "-test.run=^TestConformanceMutant2Inner$/^"+item+"$", "-test.count=1", "-test.v")
			cmd.Env = append(os.Environ(), mutant2Env+"="+mutant, "GH_TOKEN=ghp_host_secret")
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("壊れたバックエンド (%s) が、適合テストに合格した: 項目 %s が赤にならない", mutant, item)
			}
			if !strings.Contains(string(out), "--- FAIL: TestConformanceMutant2Inner/"+item) {
				t.Errorf("適合テストは失敗したが、項目 %s ではない:\n%s", item, out)
			}
		})
	}
}
