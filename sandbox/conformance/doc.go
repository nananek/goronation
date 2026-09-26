// Package conformance は、サンドボックスのバックエンド (core/sandbox) が契約を満たすことを、実際に檻を起動して確かめる、共有の適合テストで、コントローラは Run 1 つを呼ぶだけでよい。
//
// 確かめるのは、契約の文言 (秘密が見えない・環境が clean・外に届かない・書ける場所・親の死・終了・端末の注入など) と、Capabilities の宣言 (満たすと宣言した項目は満たし、
// 満たさないと宣言した項目は満たさない)。バックエンドの機構は見ない。檻の中の観測は、テストバイナリを檻の中で再実行した probe が JSON で返す。
//
// # 使い方
//
//	func TestConformance(t *testing.T) {
//		conformance.Run(t, func(h contract.Host) sandbox.Backend { return New(h) }, conformance.RequireEnv("GORO_REQUIRE_X"))
//	}
//
// # 規則
//
//   - p0: Cap の無い項目は、全バックエンドが満たす。1 つでも落ちれば、契約違反。
//   - caps: Cap つきの項目は、宣言が true なら満たし、false なら満たさないことを確かめる (skip しない。改善しても悪化しても赤くなる)。
//   - layout: PathRemap が true なら論理的な配置 (GuestPath あり) で、false ならホストと同じ path で、Spec を組む。バックエンドは、テストを変えずに走る。
//   - probe: probe は、テストバイナリの init が、第 1 引数の印で動く。TestMain は要らない。親の死・fd の役は、環境変数の印で再実行する。
//   - require: バックエンドを使えないとき、RequireEnv の環境変数が 1 なら fail、そうでなければ skip する (実行層の CI が、全部 skip で緑にならない)。
//
// # 限界
//
//   - 端末の注入の検査は、pty の用意 (pty_<OS>.go) が要る。Linux だけがある。無い OS では、その項目は落ちる (実装を足す)。
//   - 観測は、Go の標準ライブラリだけ (stat・ReadDir・write・dial・env・fd)。/proc・CapEff・エラー文言などの、バックエンド固有の性質は、バックエンドの側のテストが見る。
//
// # 関連
//
// docs/adr/0001-repository-layout.md
package conformance
