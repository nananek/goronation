//go:build linux

package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"strings"

	"github.com/nananek/goronation/hostfs"
)

const (
	// bundleName は、export/ の中の bundle のファイル名 (固定。名前を、檻に決めさせない)。
	bundleName = "goro.bundle"
	// maxHeader は、bundle のヘッダ (refs の一覧) を読む上限。
	maxHeader = 64 << 10
	// maxHeads は、取り込みのコマンドに書くブランチの数の上限。
	maxHeads = 100
)

// Bundle は、Export の結果。
type Bundle struct {
	// Path は、ホスト上の bundle のファイル。
	Path string
	// Size は、bundle の大きさ (バイト)。
	Size int64
	// Heads は、bundle に入っているブランチ (refs/heads/...)。名前が安全なものだけ。
	Heads []string
	// Skipped は、名前が安全でなく (檻が決めた名前)、除いたブランチの数。
	Skipped int
	// Fetch は、利用者が、自分のリポジトリで実行する、取り込みのコマンド (シェルの 1 行)。ブランチは refs/heads/goro/<ID>/ の下に入る。
	Fetch string
}

// Export は、ID のセッションの clone を、bundle にして取り出す。
//
// bundle は、使い捨ての檻の中で作る (clone を ro で /work に、export/ を rw で /out に bind して git bundle create)。
// ホストは、clone の中で git を実行しない (clone の .git/config と hooks は、檻が書けるので、ホストで git を動かすと、
// fsmonitor・hooks・alias・textconv などで、任意のコードを実行させられる)。ホストが読むのは bundle だけで、hostfs で
// 通常のファイル・上限・ヘッダを確かめる。
func (s *Store) Export(ctx context.Context, id string) (b *Bundle, err error) {
	sess, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(gitBin); err != nil {
		return nil, fmt.Errorf("session: git を使えない (%s): %w", gitBin, err)
	}
	exportRoot, err := os.OpenRoot(sess.Export)
	if err != nil {
		return nil, fmt.Errorf("session: %w", err)
	}
	defer exportRoot.Close()
	// 前の bundle を消す (git bundle create は、既存のファイルを置き換えるが、名前が symlink などなら、その先へ書きうる)。
	if err := exportRoot.Remove(bundleName); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("session: 前の bundle を消せない: %w", err)
	}
	// 受け取れなかった bundle は、残さない (拒否したものを、あとから、検査なしで使わせない)。
	defer func() {
		if err != nil {
			exportRoot.Remove(bundleName)
		}
	}()
	if err := s.runGit(ctx, s.exportBinds(sess), "-C", "/work", "bundle", "create", "/out/"+bundleName, "--all"); err != nil {
		return nil, err
	}
	if s.afterBundle != nil {
		s.afterBundle(sess.Export)
	}
	f, err := hostfs.Open(exportRoot, bundleName, s.maxBundle)
	if err != nil {
		return nil, fmt.Errorf("session: bundle を受け取れない: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("session: %w", err)
	}
	head, err := io.ReadAll(io.LimitReader(f, maxHeader))
	if err != nil {
		return nil, fmt.Errorf("session: bundle を読めない: %w", err)
	}
	heads, skipped, err := parseBundleHeader(head)
	if err != nil {
		return nil, fmt.Errorf("session: bundle が正しくない: %w", err)
	}
	if len(heads) == 0 {
		return nil, fmt.Errorf("session: bundle に、取り込めるブランチ (refs/heads/...) が無い (名前が安全でなく除いたもの: %d)", skipped)
	}
	path := sess.Export + "/" + bundleName
	return &Bundle{Path: path, Size: fi.Size(), Heads: heads, Skipped: skipped, Fetch: fetchCommand(path, id, heads)}, nil
}

// oidRE・headNameRE は、bundle のヘッダの ref の行 ("<oid> <ref>") の、oid と、ブランチ名 (refs/heads/ の後ろ) の形。
var (
	oidRE      = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
	headNameRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._/-]{0,199}$`)
)

// parseBundleHeader は、bundle のヘッダ (先頭の、空行までの部分) を読み、ブランチの名前を返す。ヘッダは、檻が書いたもので、
// 敵対入力: 形が正しくなければ error にし、名前が、決めた文字と、git の ref の規則 (.. を含まない・要素が空でなく、. で始まらず、
// . や .lock で終わらない) を満たさないブランチは、Skipped に数えて、除く。上限を超えた分も、Skipped に数える。
func parseBundleHeader(b []byte) (heads []string, skipped int, err error) {
	lines := strings.Split(string(b), "\n")
	if lines[0] != "# v2 git bundle" && lines[0] != "# v3 git bundle" {
		return nil, 0, fmt.Errorf("先頭が bundle のヘッダではない")
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if lines[i] == "" {
			end = i
			break
		}
	}
	// 空行は、pack のデータの前にある。末尾の空の要素 (改行で終わる入力の、split の余り) は、空行ではない。
	if end < 0 || end == len(lines)-1 {
		return nil, 0, fmt.Errorf("ヘッダが、空行で終わらない (読んだのは先頭の %d バイトまで)", maxHeader)
	}
	for _, l := range lines[1:end] {
		if strings.HasPrefix(l, "@") || strings.HasPrefix(l, "-") { // v3 の capability・前提となるコミット
			continue
		}
		oid, ref, ok := strings.Cut(l, " ")
		if !ok || !oidRE.MatchString(oid) {
			return nil, 0, fmt.Errorf("ヘッダの行の形が正しくない")
		}
		if !strings.HasPrefix(ref, "refs/heads/") {
			continue // HEAD・タグなど
		}
		if !safeHeadName(ref) || len(heads) >= maxHeads {
			skipped++
			continue
		}
		heads = append(heads, ref)
	}
	return heads, skipped, nil
}

// safeHeadName は、ref (refs/heads/...) が、決めた文字だけで、git の ref の規則に合う名前か。
func safeHeadName(ref string) bool {
	name, ok := strings.CutPrefix(ref, "refs/heads/")
	if !ok || !headNameRE.MatchString(name) || strings.Contains(name, "..") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

// fetchCommand は、利用者が、自分のリポジトリで実行する取り込みのコマンド。オブジェクトを fsck し、ブランチは、
// 現在のブランチを上書きしないよう、refs/heads/goro/<id>/ の下へ入れる。--no-tags: 指定した ref のほかに、タグ (名前は檻が決める) を、
// 自動で取り込まない。--no-recurse-submodules: 利用者の repo の submodule を、bundle の内容に応じて取りに行かない。
// 名前は、すべて、シェルの引用をつける。
func fetchCommand(bundle, id string, heads []string) string {
	parts := []string{"git", "-c", "transfer.fsckObjects=true", "fetch", "--no-tags", "--no-recurse-submodules", shellQuote(bundle)}
	for _, h := range heads {
		parts = append(parts, shellQuote(h+":refs/heads/goro/"+id+"/"+strings.TrimPrefix(h, "refs/heads/")))
	}
	return strings.Join(parts, " ")
}

// shellQuote は、s を、シェルの 1 語 (単一引用符で囲んだもの) にする。
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
