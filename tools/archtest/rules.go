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

		// spec/v0 の Envelope は Raw (元のフレーム。path・設定・応答の本文を含みうる) を持つ。UI・API の層 (cmd/goronation の web など)
		// が Envelope を import すると、Public() を通さずに返せてしまう (v0.Envelope.Public の doc の約束)。Envelope・Command を
		// 触ってよいのは、変換の側 (agent/**・core/**・spec/**) と、UI・API に渡す形 (UIEnvelope の JSON) に閉じ込める
		// cmd/internal/chat だけ (ADR 0009)。規則を広げるときは、新しい ADR を書き、この表だけを変える。
		{
			ID:      "v0-only-in-chat",
			Imports: mods("spec/v0/**"),
			OnlyIn:  []string{"agent/**", "core/**", "spec/**", "cmd/internal/chat/**"},
		},

		// Envelope は spec/v0 を import しなくても、core/agent の Stream.DecodeFrame の戻り値 (型は推論で決まる) から手に入る。
		// v0-only-in-chat だけでは、cmd/goronation が core/agent と agent の実装だけを import して Raw を返せてしまう。
		// Stream を触れる場所も、同じ範囲に限る (ADR 0009)。
		{
			ID:      "agent-only-in-chat",
			Imports: mods("core/agent/**", "agent/**"),
			OnlyIn:  []string{"agent/**", "core/**", "spec/**", "cmd/internal/chat/**"},
		},

		// 実装の配線 (build tag) は cmd/** だけが行う。
		{
			ID:      "impl-only-from-cmd",
			Imports: mods("sandbox/bwrap/**", "sandbox/seatbelt/**", "agent/claude/**", "agent/opencode/**", "vault/**"),
			OnlyIn:  []string{"cmd/**"},
		},

		// ADR 0004: 全 module 依存ゼロの方針の例外は、WebAuthn の署名検証・CBOR デコードに限る。この 2 つの
		// 外部 module (cbor が float16 に依存する) を import してよいのは、cmd/internal/webauthn とその配下
		// (結合テスト用の偽の認証器 webauthntest を含む) だけ。規則を広げるときは、新しい ADR を書き、この
		// 表だけを変える。OnlyIn は (Imports と違い) repo 相対のファイル path のパターンで書く。
		{
			ID:      "webauthn-only-dep",
			Imports: []string{"github.com/fxamacker/cbor/v2", "github.com/x448/float16"},
			OnlyIn:  []string{"cmd/internal/webauthn/**"},
		},

		// ADR 0005: 全 module 依存ゼロの方針のもう 1 つの例外は、端末ビューの WebSocket 終端に限る。
		// この module を import してよいのは、cmd/internal/termrelay とその配下だけ。規則を広げるときは、
		// 新しい ADR を書き、この表だけを変える。
		{
			ID:      "websocket-only-dep",
			Imports: []string{"github.com/coder/websocket"},
			OnlyIn:  []string{"cmd/internal/termrelay/**"},
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

	// ADR 0036: vault/** が import してよいのは、標準ライブラリと core/**・vault/** だけ (秘密を持つコードの依存を、固定する)。
	// 外部 module (cbor・websocket・sqlite など) も、agent・sandbox・egress・hostfs・cmd・spec も、使えない。
	OnlyImports: []OnlyImport{
		{ID: "dep-vault", In: []string{"vault/**"}, Allow: mods("core/**", "vault/**")},
	},

	// ADR 0036: vault/go.mod は、require も tool も持たない。
	NoRequires: []NoRequire{
		{ID: "vault-no-require", Dirs: []string{"vault"}},
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
		// MethodByName は、それらのメソッドを、セレクタを書かずに、名前の文字列から動的に呼べる (セレクタの名前では
		// 検出できない)。Method(i) のような添字での動的な呼び出しは、net/http の Request.Method など正当な名前と
		// 衝突して誤検出が多いので、規則にしない (doc.go の限界)。
		// メソッドは、構文解析だけで型情報が無いので、レシーバの型によらず名前だけで検出する。reflect と無関係な
		// 同名のメソッドやフィールドも検出する (誤検出。fail-closed 側で許容する。fixture reflect-unsafe の homonym.go)。
		{
			ID:      "reflect-unsafe",
			Funcs:   []Func{{Pkg: "reflect", Name: "NewAt"}},
			Methods: []string{"UnsafePointer", "UnsafeAddr", "SetPointer", "MethodByName"},
			OnlyIn:  execAllowed,
		},
	},
}
