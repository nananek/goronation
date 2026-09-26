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

// 壊れたバックエンド (変異) を、適合テストが赤にするかを確かめる。適合テストが合格させると、seatbelt を書く人が、同じ壊れ方をしても合格する。
// 変異は、環境変数 mutantEnv で選ぶ。変異を入れた適合テストは別プロセス (このテストバイナリの再実行) で走らせ、失敗することを確かめる。
// 適合テストの再実行 (親の死・fd の役) も、同じ環境変数を引き継ぐので、変異は役にも入る。
const mutantEnv = "GORO_CONFORMANCE_MUTANT"

// leakyEnv は、ホストの環境変数 GORO_HOST_ONLY を檻に漏らし、それを ExtraEnv として宣言するバックエンド (宣言で、env-clean を免れようとする)。
type leakyEnv struct{ *Backend }

func (b leakyEnv) Capabilities() sandbox.Capabilities {
	c := b.Backend.Capabilities()
	c.ExtraEnv = append(slices.Clone(c.ExtraEnv), "GORO_HOST_ONLY")
	return c
}

func (b leakyEnv) Start(ctx context.Context, s sandbox.Spec) (sandbox.Cage, error) {
	s.Env = append(slices.Clone(s.Env), sandbox.EnvVar{Key: "GORO_HOST_ONLY", Value: os.Getenv("GORO_HOST_ONLY")})
	return b.Backend.Start(ctx, s)
}

// TestConformanceMutantInner は、mutantEnv の変異を入れた bwrap のバックエンドで、適合テストを走らせる。mutantEnv が無ければ何もしない
// (TestConformanceCatchesMutants が、別プロセスで、これを失敗させる)。
func TestConformanceMutantInner(t *testing.T) {
	mutant := os.Getenv(mutantEnv)
	if mutant == "" {
		t.Skipf("%s が無い (TestConformanceCatchesMutants が使う)", mutantEnv)
	}
	nb := func(h contract.Host) sandbox.Backend { return New(h) }
	switch mutant {
	case "declared-env-leak":
		nb = func(h contract.Host) sandbox.Backend { return leakyEnv{New(h)} }
	case "fd-3to9":
		// 継承した fd を、3〜9 だけ close-on-exec にする (10 以降は、檻に届く)。
		closeInheritedFDs = func() error {
			for fd := 3; fd <= 9; fd++ {
				syscall.CloseOnExec(fd)
			}
			return nil
		}
	default:
		t.Fatalf("未知の変異 %q", mutant)
	}
	conformance.Run(t, nb, conformance.RequireEnv(requireEnv))
}

// TestConformanceCatchesMutants は、危険な壊れ方をしたバックエンドが、適合テストに合格しない (指定の項目が赤くなる) ことを確認する。
func TestConformanceCatchesMutants(t *testing.T) {
	needBwrap(t)
	for mutant, item := range map[string]string{
		"declared-env-leak": "env-clean",    // 漏らした環境変数を ExtraEnv に宣言しても、漏れは漏れ
		"fd-3to9":           "inherited-fd", // fd 9 だけを見ていると、10 以降の漏れを見逃す
	} {
		t.Run(mutant, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestConformanceMutantInner$", "-test.count=1", "-test.v")
			cmd.Env = append(os.Environ(), mutantEnv+"="+mutant)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("壊れたバックエンド (%s) が、適合テストに合格した: 項目 %s が赤にならない", mutant, item)
			}
			if !strings.Contains(string(out), "--- FAIL: TestConformanceMutantInner/"+item) {
				t.Errorf("適合テストは失敗したが、項目 %s ではない:\n%s", item, out)
			}
		})
	}
}
