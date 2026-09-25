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

// execImports は、import するだけで子プロセスを起動できる package。os/exec と、標準ライブラリの中で
// os/exec を使う package (net/http/cgi など。標準ライブラリの中の import は exec-import の対象外)。
// 後者は網羅しにくいため、標準ライブラリの側から求めた全数を、テスト (TestStdExecDependents) が確かめる。
//
// go/build/constraint など、os/exec に依存しない子 package は含めないため、"/**" を付けずに 1 つずつ書く。
var execImports = []string{"os/exec/**", "go/build", "go/importer", "net/http/cgi", "net/http/fcgi"}

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
		{ID: "exec-import", Imports: execImports, OnlyIn: execAllowed},

		// 単一バイナリ / CGO_ENABLED=0 の方針。許可される場所は無い。
		{ID: "cgo", Imports: []string{"C"}},

		// plugin は、事前ビルドした .so を実行時にロードでき、os/exec も表の関数も要らない
		// (リポジトリの .so は archtest には見えない)。許可される場所は無い。
		{ID: "plugin", Imports: []string{"plugin"}},

		// unsafe は、実行可能メモリ (syscall.Mmap / Mprotect) に書いた機械語を、関数として呼べる (funcval を
		// 自作する)。.s も .c も linkname も表の関数の呼び出しも要らない。os/exec と同じ場所にだけ許す。
		{ID: "unsafe-import", Imports: []string{"unsafe"}, OnlyIn: execAllowed},

		// debug/gosym は、実行中のバイナリの pclntab から、関数 (exec-call の表の syscall.Syscall など) の
		// アドレスを引ける。reflect-unsafe と組み合わせると、表に無い名前で、表の関数をアドレスで呼べる。
		// os/exec と同じ場所にだけ許す。
		{ID: "gosym-import", Imports: []string{"debug/gosym"}, OnlyIn: execAllowed},

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
		// I1: プロセス起動の関数、生 syscall (execve を番号で直接呼べる)、実行可能メモリを作る関数。
		// import 別名は解決して判定する。
		{
			ID: "exec-call",
			Funcs: []Func{
				{Pkg: "os", Name: "StartProcess"},
				{Pkg: "syscall", Name: "Exec"},
				{Pkg: "syscall", Name: "ForkExec"},
				{Pkg: "syscall", Name: "StartProcess"},
				{Pkg: "syscall", Name: "Syscall"},
				{Pkg: "syscall", Name: "Syscall6"},
				{Pkg: "syscall", Name: "RawSyscall"},
				{Pkg: "syscall", Name: "RawSyscall6"},
				{Pkg: "syscall", Name: "AllThreadsSyscall"}, // linux
				{Pkg: "syscall", Name: "AllThreadsSyscall6"},
				{Pkg: "syscall", Name: "Syscall9"}, // darwin・BSD・linux/mips
				{Pkg: "golang.org/x/sys/unix", Name: "Exec"},
				{Pkg: "golang.org/x/sys/unix", Name: "ForkExec"},
				{Pkg: "golang.org/x/sys/unix", Name: "Syscall"},
				{Pkg: "golang.org/x/sys/unix", Name: "Syscall6"},
				{Pkg: "golang.org/x/sys/unix", Name: "RawSyscall"},
				{Pkg: "golang.org/x/sys/unix", Name: "RawSyscall6"},
				// 実行可能メモリを作れる (機械語を書いて、関数として呼べる。unsafe-import と合わせて塞ぐ)。
				{Pkg: "syscall", Name: "Mmap"},
				{Pkg: "syscall", Name: "Mprotect"},
				{Pkg: "golang.org/x/sys/unix", Name: "Mmap"},
				{Pkg: "golang.org/x/sys/unix", Name: "Mprotect"},
			},
			OnlyIn: execAllowed,
		},

		// reflect は、unsafe を import せずに、unsafe.Pointer を得て (UnsafePointer・UnsafeAddr)、任意のアドレスを
		// 書き換える (NewAt・SetPointer) 入口になる。unsafe-import と同じ理由で、os/exec と同じ場所にだけ許す。
		// メソッドは、構文解析だけで型情報が無いので、レシーバの型によらず名前だけで検出する。reflect と無関係な
		// 同名のメソッドやフィールドも検出する (誤検出。fail-closed 側で許容する。fixture reflect-unsafe の homonym.go)。
		{
			ID:      "reflect-unsafe",
			Funcs:   []Func{{Pkg: "reflect", Name: "NewAt"}},
			Methods: []string{"UnsafePointer", "UnsafeAddr", "SetPointer"},
			OnlyIn:  execAllowed,
		},
	},
}
