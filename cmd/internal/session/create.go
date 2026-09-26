//go:build linux

package session

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// 名義の既定。
const (
	DefaultName  = "goro"
	DefaultEmail = "goro@localhost.invalid"
)

// CreateOptions は、Create の引数。
type CreateOptions struct {
	// Repo は、元のリポジトリの path (ローカルだけ。URL は受け付けない)。
	Repo string
	// Name・Email は、clone の user.name・user.email (空なら既定)。
	Name, Email string
	// Agent は、このセッションで動かすエージェントの名前 (小文字・数字・- の、32 文字以下)。セッションのディレクトリに記録し、
	// Store.Agent で読める。空なら記録しない。clone は、エージェントが rw で使い、そこに置かれた設定 (.claude/・opencode.json など) は、
	// 次にそこで動くエージェントに読まれる。別のエージェントで使い回さないための、呼び手の判断の材料にする。
	Agent string
}

// Create は、Repo の private clone を持つ新しいセッションを作る。
//
// git は、使い捨ての檻の中で実行する (ホストでは実行しない): 元のリポジトリを ro で /src に bind し、clone 先を rw で /work に
// bind して、git clone --no-local --no-hardlinks する。clone されるのは、コミット済みの内容だけで、元のリポジトリと object を
// 共有しない。そのあと origin を外す。失敗したら、作ったディレクトリを消す。
func (s *Store) Create(ctx context.Context, o CreateOptions) (sess *Session, err error) {
	repo, err := checkRepo(o.Repo)
	if err != nil {
		return nil, err
	}
	name, email, err := identity(o.Name, o.Email)
	if err != nil {
		return nil, err
	}
	if o.Agent != "" {
		if err := checkAgent(o.Agent); err != nil {
			return nil, err
		}
	}
	if _, err := os.Stat(gitBin); err != nil {
		return nil, fmt.Errorf("session: git を使えない (%s): %w", gitBin, err)
	}
	id, err := s.newID()
	if err != nil {
		return nil, err
	}
	sess = s.layout(id)
	dir := sess.Dir
	// セッションのディレクトリは、自分で作れたときだけ、失敗で消す (作れなければ、同じ ID の別のセッションがある。消さない)。
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, fmt.Errorf("session: %w", err)
	}
	// 失敗したら、作ったディレクトリを消す (return nil, err で、sess は nil になるので、dir を別に持つ)。
	defer func() {
		if err != nil {
			os.RemoveAll(dir)
		}
	}()
	for _, d := range []string{sess.Clone, sess.Run, sess.Export} {
		if err := os.Mkdir(d, 0o700); err != nil {
			return nil, fmt.Errorf("session: %w", err)
		}
	}
	if err := writeRepoLabel(sess, repo); err != nil {
		return nil, err
	}
	// エージェントの記録は、clone を作る前 (檻を起動する前) に書く: 書けなければ、ここで失敗し、ディレクトリごと消える。
	// 後で書くと、書けなかったセッションが、記録の無い (= 古い) セッションとして、別のエージェントで使われうる。
	if o.Agent != "" {
		if err := writeAgent(sess, o.Agent); err != nil {
			return nil, err
		}
	}
	if err := s.runGit(ctx, s.cloneBinds(repo, sess), "clone", "--no-local", "--no-hardlinks",
		"-c", "user.name="+name, "-c", "user.email="+email, "--", "/src", "/work"); err != nil {
		return nil, err
	}
	if err := s.runGit(ctx, s.workBinds(sess), "-C", "/work", "remote", "remove", "origin"); err != nil {
		return nil, err
	}
	return sess, nil
}

// checkRepo は、Repo がローカルのディレクトリの path であることを確かめ、絶対・クリーンな path にして返す。
func checkRepo(repo string) (string, error) {
	if repo == "" {
		return "", fmt.Errorf("session: Repo が空")
	}
	if u, err := url.Parse(repo); err == nil && u.Scheme != "" && strings.Contains(repo, "://") {
		return "", fmt.Errorf("session: Repo %q は URL (ローカルの path だけを受け付ける)", repo)
	}
	if strings.HasPrefix(repo, "-") {
		return "", fmt.Errorf("session: Repo %q が - で始まる", repo)
	}
	abs, err := filepath.Abs(repo)
	if err != nil {
		return "", fmt.Errorf("session: %w", err)
	}
	if err := checkAbs("Repo", abs); err != nil {
		return "", err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("session: %w", err)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("session: Repo %q がディレクトリではない", abs)
	}
	return abs, nil
}

// identity は、名義 (user.name・user.email) を確かめる。空なら既定。1 行で、制御文字を含まず、200 バイト以下。
func identity(name, email string) (string, string, error) {
	if name == "" {
		name = DefaultName
	}
	if email == "" {
		email = DefaultEmail
	}
	for what, v := range map[string]string{"Name": name, "Email": email} {
		if len(v) > 200 || strings.TrimSpace(v) != v || strings.ContainsFunc(v, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			return "", "", fmt.Errorf("session: %s %q が正しくない (200 バイト以下の 1 行。前後の空白と制御文字は不可)", what, v)
		}
	}
	if !strings.Contains(email, "@") || strings.ContainsAny(email, " <>") {
		return "", "", fmt.Errorf("session: Email %q が正しくない", email)
	}
	if strings.ContainsAny(name, "<>") {
		return "", "", fmt.Errorf("session: Name %q が < か > を含む", name)
	}
	return name, email, nil
}
