package archtest

// module は、このリポジトリの module path の共通の接頭辞。
const module = "github.com/nananek/goronation"

// mods は、module 配下の path のパターンを作る。
func mods(rel ...string) []string {
	out := make([]string, len(rel))
	for i, r := range rel {
		out[i] = module + "/" + r
	}
	return out
}

// execAllowed は、プロセスを起動してよい場所 (不変条件 I1)。
// 規則を緩めるときは ADR を書き、この表だけを変える。
var execAllowed = []string{"sandbox/**", "cmd/**"}

// DefaultRules は、このリポジトリに適用する規則の表。
//
// パターンは、完全一致か "P/**" (P とその配下、セグメント単位)。
// 3 つ目以降の dep-* / impl-* 規則は、対象パッケージが未作成でも書いておく
// (作った時点から効く)。
var DefaultRules = Rules{
	Module: module,

	AllowImports: []AllowImport{
		// I1: os/exec を import してよいのは sandbox/** と cmd/** だけ。
		// 別名・blank・dot import も、_test.go も、全ビルドタグも対象。
		{ID: "exec-import", Imports: []string{"os/exec/**"}, OnlyIn: execAllowed},

		// 単一バイナリ / CGO_ENABLED=0 の方針。許可される場所は無い。
		{ID: "cgo", Imports: []string{"C"}},

		// 実装の配線 (build tag) は cmd/** だけが行う。
		{
			ID:      "impl-only-from-cmd",
			Imports: mods("sandbox/bwrap/**", "sandbox/seatbelt/**", "agent/claude/**", "agent/opencode/**"),
			OnlyIn:  []string{"cmd/**"},
		},
	},

	ForbidImports: []ForbidImport{
		// core は、他のトップ module を import しない。
		{
			ID:      "dep-core",
			In:      []string{"core/**"},
			Imports: mods("control/**", "agent/**", "sandbox/**", "egress/**", "vault/**", "hostfs/**", "gateway/**", "cmd/**"),
		},

		// agent と sandbox は、互いに import しない (独立にテスト・保守できることの担保)。
		{ID: "dep-agent-sandbox", In: []string{"agent/**"}, Imports: mods("sandbox/**")},
		{ID: "dep-agent-sandbox", In: []string{"sandbox/**"}, Imports: mods("agent/**")},
	},

	Calls: []CallRule{
		// I1: プロセス起動の関数。import 別名は解決して判定する。
		{
			ID: "exec-call",
			Funcs: []Func{
				{Pkg: "os", Name: "StartProcess"},
				{Pkg: "syscall", Name: "Exec"},
				{Pkg: "syscall", Name: "ForkExec"},
				{Pkg: "syscall", Name: "StartProcess"},
				{Pkg: "golang.org/x/sys/unix", Name: "Exec"},
				{Pkg: "golang.org/x/sys/unix", Name: "ForkExec"},
			},
			OnlyIn: execAllowed,
		},
	},
}
