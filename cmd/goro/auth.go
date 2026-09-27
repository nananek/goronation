//go:build linux

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"unsafe"

	"github.com/nananek/goronation/cmd/internal/credfile"
	"github.com/nananek/goronation/core/credential"
)

// credentialKind は、goro auth が扱う資格情報 1 種類。名前 (表のキー) が、goro auth <名前> の引数と、保存先 <state>/credentials/<名前> になる。
// 資格情報の種類は、今後も増える: 表に 1 つ足すだけで、goro auth・usage・保存に、そのまま使える (最初の 1 つ (github) を特別扱いしない)。
type credentialKind struct {
	// name は、資格情報の名前 (credential.CheckName を満たす。表の中で重ならない)。
	name string
	// valid は、入力の形を確かめる (貼り間違いを見つけるだけ。有効かどうかは、確かめない)。値は、error にも表示にも出さない。
	valid func(v string) bool
	// invalid は、形が違うときの表示 (1 行。何が起きたか + 次にすること)。値は含めない。
	invalid string
	// usage は、goro auth -h の、この資格情報の説明 (どこで作るか・要る権限)。表示は、-h だけ。
	usage string
}

// githubTokenRE は、GitHub の Fine-grained トークンの形: github_pat_ + 英数字と _ (合計 40〜255 文字)。
var githubTokenRE = regexp.MustCompile(`^github_pat_[A-Za-z0-9_]{29,244}$`)

// credentialKinds は、goro auth NAME の NAME に指定できる資格情報の表。資格情報を足すときは、ここに足す。
var credentialKinds = []credentialKind{
	{
		name:    "github",
		valid:   githubTokenRE.MatchString,
		invalid: "形式が違う。github_pat_ で始まる Fine-grained トークンを貼り直してください (作り方: goro auth -h)。",
		usage: "Fine-grained トークン (github_pat_…)。作る場所: https://github.com/settings/personal-access-tokens/new\n" +
			"           権限: Contents と Pull requests を Read and write にする。Workflows は付けない。goro は形式だけを検査し、有効かは確かめない。",
	},
}

// credentialKindByName は、name の資格情報の種類。無ければ ok が false。
func credentialKindByName(name string) (k credentialKind, ok bool) {
	for _, c := range credentialKinds {
		if c.name == name {
			return c, true
		}
	}
	return credentialKind{}, false
}

// credentialNames は、goro auth に指定できる名前を、"a か b" の形に並べる。
func credentialNames() string {
	names := make([]string, len(credentialKinds))
	for i, c := range credentialKinds {
		names[i] = c.name
	}
	return strings.Join(names, " か ")
}

// authUsage は、goro auth -h の使い方。資格情報ごとの説明は、credentialKinds から作る。
func authUsage() string {
	var b strings.Builder
	b.WriteString(`使い方: goro auth NAME [--state-dir DIR]

資格情報を、ホストのファイル <DIR>/credentials/NAME (0600) に保存する。檻には入らない。
値は、画面・履歴・ログ・エラーに出さない。引数には書けない (シェルの履歴・ps に残るため)。

  NAME              資格情報の名前 (下の一覧)
  --state-dir DIR   状態を置く場所 (既定は $XDG_STATE_HOME/goro か ~/.local/state/goro)

入力: 端末なら、表示せずに 1 行。端末でなければ (パイプなど)、標準入力の 1 行。

名前:
`)
	for _, c := range credentialKinds {
		fmt.Fprintf(&b, "  %-8s %s\n", c.name, c.usage)
	}
	return b.String()
}

// maxSecretLine は、入力の 1 行の長さの上限 (バイト)。これを超える入力は、形が違うものとして扱う。
const maxSecretLine = 1024

// runAuth は goro auth の本体で、終了コードを返す (成功 0・保存できない 1・使い方の誤り 2・中止 130)。
// stdin が端末なら、表示せずに 1 行読む (プロンプトは stderr)。値は、どこにも出さない。
func runAuth(args []string, stdin *os.File, stdout, stderr io.Writer) int {
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "goro auth: "+format+"\n", a...)
		return 1
	}
	usageErr := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "goro auth: "+format+"\n", a...)
		return exitUsage
	}
	name, rest := "", args // NAME は、オプションの前でも後ろでも書ける
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, rest = args[0], args[1:]
	}
	flags := flag.NewFlagSet("goro auth", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {}
	stateDir := flags.String("state-dir", "", "")
	if err := flags.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stderr, authUsage())
			return 0
		}
		fmt.Fprintln(stderr, "使い方: goro auth -h")
		return exitUsage
	}
	positional := flags.Args()
	if name == "" && len(positional) > 0 {
		name, positional = positional[0], positional[1:]
	}
	if len(positional) > 0 {
		return usageErr("余計な引数 %q", positional[0])
	}
	if name == "" {
		return usageErr("名前を 1 つ指定する。使える名前: %s", credentialNames())
	}
	kind, ok := credentialKindByName(name)
	if !ok {
		return usageErr("未知の名前 %q。使える名前: %s", name, credentialNames())
	}
	dir, err := resolveStateDir(*stateDir)
	if err != nil {
		return fail("%v", err)
	}
	store, err := credfile.New(dir)
	if err != nil {
		return fail("%v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	line, err := readSecretLine(ctx, stdin, "トークンを貼ってください (表示されません): ", stderr)
	switch {
	case errors.Is(err, context.Canceled):
		fmt.Fprintln(stderr, "goro auth: 中止した。")
		return 130
	case err != nil:
		return fail("入力を読めない。もう一度: goro auth %s", name)
	}
	value := strings.TrimSpace(string(line)) // 貼り付けの前後の空白・改行を除く
	clear(line)
	if value == "" {
		return fail("入力が空。もう一度: goro auth %s", name)
	}
	if !kind.valid(value) { // ディスクに触る前に断る。値は、表示しない
		return fail("%s", kind.invalid)
	}
	if err := store.Save(kind.name, credential.New(value)); err != nil {
		return fail("%v", err)
	}
	fmt.Fprintf(stdout, "保存した: %s\n", sanitize(store.Path(kind.name)))
	return 0
}

// readSecretLine は、in から 1 行 (改行を除く) を読む。in が端末なら、prompt を out に出し、入力を表示しない (ECHO を切る。終わったら戻す)。
// ctx が取り消されたら (Ctrl-C など)、端末を戻して、ctx.Err() を返す。行は maxSecretLine バイトまで: 超えたら、超えた分を捨てて、上限までを返す
// (呼び手の形の検査が、長すぎるものを断る)。読んだバイト列は、呼び手が clear する。
func readSecretLine(ctx context.Context, in *os.File, prompt string, out io.Writer) ([]byte, error) {
	ts, err := saveTermios(int(in.Fd())) // 端末でなければ nil
	if err != nil {
		return nil, err
	}
	if ts != nil {
		noEcho := ts.t
		noEcho.Lflag &^= syscall.ECHO
		if err := ioctl(ts.fd, syscall.TCSETS, unsafe.Pointer(&noEcho)); err != nil {
			return nil, err
		}
		defer func() {
			if err := ts.restore(); err != nil && !errors.Is(err, syscall.EIO) { // 端末が切れた (EIO) ときは、戻す先が無い
				fmt.Fprintf(out, "goro auth: 端末の設定を戻せない (stty sane で戻す): %v\n", err)
			}
		}()
		fmt.Fprint(out, prompt)
	}
	type result struct {
		line []byte
		err  error
	}
	done := make(chan result, 1)
	go func() { // read(2) は、ctx で止められないので、別の goroutine で読む (取り消されたら、待たずに戻る。プロセスは、すぐ終わる)
		var line []byte
		one := make([]byte, 1)
		for {
			n, err := in.Read(one)
			if n == 1 {
				if one[0] == '\n' {
					done <- result{line, nil}
					return
				}
				if len(line) <= maxSecretLine {
					line = append(line, one[0])
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) && len(line) > 0 { // 改行の無い最後の行
					done <- result{line, nil}
				} else {
					done <- result{line, err}
				}
				return
			}
		}
	}()
	select {
	case r := <-done:
		if ts != nil {
			fmt.Fprintln(out) // ECHO を切っているので、Enter の改行が表示されない
		}
		return r.line, r.err
	case <-ctx.Done():
		if ts != nil {
			fmt.Fprintln(out)
		}
		return nil, ctx.Err()
	}
}
