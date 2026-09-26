package bwrap

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

// bwrapPath は、起動器の固定パス。PATH は検索しない。
const bwrapPath = "/usr/bin/bwrap"

// protectedTrees は、その配下 (と、それを含む親) を、bind できない path。HOME の下は、別の規則 (homeSecrets) で見る。
// "/" と "/etc" と "/var" は、これらの親なので、bind できない。
var protectedTrees = []string{
	"/root", "/home", "/run", "/var/run", "/proc", "/sys", "/dev", "/boot",
	"/etc/shadow", "/etc/gshadow", "/etc/ssh", "/etc/sudoers", "/etc/sudoers.d", "/etc/ssl/private",
	"/var/lib/docker", "/var/lib/containerd",
}

// wholeDenied は、丸ごとは bind できない path (ホスト全体で共有するので)。配下の path は bind できる。
var wholeDenied = []string{"/tmp", "/var/tmp"}

// readOnlyTrees は、ro でだけ bind できる path (システムの領域。/boot は protectedTrees が、rw も ro も拒否する)。
var readOnlyTrees = []string{"/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32", "/opt", "/etc", "/var"}

// homeSecrets は、HOME からの相対で、資格情報・ログイン状態を持つ path (その配下と親は bind できない)。
// このほかに、先頭の名前が .claude・.gnupg で始まるもの (.claude・.claude.json・.gnupg・.gnupg-vault など) も
// 機密として扱う (isHomeSecret)。
var homeSecrets = []string{
	".ssh", ".aws", ".azure", ".kube", ".docker",
	".netrc", ".git-credentials", ".npmrc", ".pypirc", ".password-store", ".codex", ".copilot", ".commandcode",
	".config/gh", ".config/git", ".config/gcloud", ".config/op", ".local/share/keyrings",
}

// credEnvKeys・credEnvPrefixes・credEnvParts は、資格情報らしい環境変数の名前 (大文字にして比べる)。
// credEnvKeys は、名前に credEnvParts を含まないものだけを並べる (GH_TOKEN や ANTHROPIC_API_KEY は、部分一致で拒否する)。
var (
	credEnvKeys     = []string{"SSH_AUTH_SOCK", "SSH_AGENT_PID", "GPG_AGENT_INFO", "GNUPGHOME", "KUBECONFIG"}
	credEnvPrefixes = []string{"AWS_", "AZURE_", "GOOGLE_", "GCP_", "CLOUDSDK_"}
	credEnvParts    = []string{"TOKEN", "SECRET", "PASSWORD", "PASSWD", "CREDENTIAL", "API_KEY", "APIKEY", "PRIVATE_KEY", "ACCESS_KEY"}
)

// Argv は、s を検証し、bwrap の argv (argv[0] は固定パスの bwrap) を作る。ファイルシステムも環境も見ない純関数で、
// 検証に通らない Spec は error にする (省略できる検証は無い)。symlink を辿った実際の path の検証は、Start が行う。
func (s Spec) Argv() ([]string, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	return s.argv(), nil
}

// argv は、検証済みの s から argv を作る。
func (s Spec) argv() []string {
	a := []string{bwrapPath, "--unshare-all", "--die-with-parent"}
	if s.NewSession {
		a = append(a, "--new-session")
	}
	for _, l := range s.Symlinks {
		a = append(a, "--symlink", l.Target, l.Dst)
	}
	a = append(a, "--proc", "/proc", "--dev", "/dev")
	for _, t := range s.Tmpfs {
		a = append(a, "--tmpfs", t)
	}
	for _, b := range s.Binds {
		if b.RW {
			a = append(a, "--bind", b.Src, b.Dst)
		} else {
			a = append(a, "--ro-bind", b.Src, b.Dst)
		}
	}
	if s.Chdir != "" {
		a = append(a, "--chdir", s.Chdir)
	}
	a = append(a, "--clearenv")
	for _, e := range s.Env {
		a = append(a, "--setenv", e.Key, e.Value)
	}
	a = append(a, "--")
	return append(a, s.Cmd...)
}

// validate は、s が I2 の規則 (檻に持ち込むものの制限) を満たすかを、文字列だけで確かめる。
func (s Spec) validate() error {
	if err := s.Host.check(); err != nil {
		return fmt.Errorf("bwrap: Host: %w", err)
	}
	dsts := map[string]string{}
	claim := func(what, dst string) error {
		if err := checkDst(dst); err != nil {
			return err
		}
		if prev, dup := dsts[dst]; dup {
			return fmt.Errorf("Dst %q が重複している (%s と %s)", dst, prev, what)
		}
		dsts[dst] = what
		return nil
	}
	for i, l := range s.Symlinks {
		what := fmt.Sprintf("Symlinks[%d]", i)
		if err := claim(what, l.Dst); err != nil {
			return fmt.Errorf("bwrap: %s: %w", what, err)
		}
		if l.Target == "" || hasControl(l.Target) {
			return fmt.Errorf("bwrap: %s: Target %q が空か、制御文字を含む", what, l.Target)
		}
	}
	for i, t := range s.Tmpfs {
		what := fmt.Sprintf("Tmpfs[%d]", i)
		if err := claim(what, t); err != nil {
			return fmt.Errorf("bwrap: %s: %w", what, err)
		}
	}
	for i, b := range s.Binds {
		what := fmt.Sprintf("Binds[%d]", i)
		if err := claim(what, b.Dst); err != nil {
			return fmt.Errorf("bwrap: %s: %w", what, err)
		}
		if err := s.Host.checkSrc(b, b.Src, true); err != nil {
			return fmt.Errorf("bwrap: %s: %w", what, err)
		}
	}
	for i, e := range s.Env {
		if err := checkEnv(e); err != nil {
			return fmt.Errorf("bwrap: Env[%d]: %w", i, err)
		}
		if slices.ContainsFunc(s.Env[:i], func(p EnvVar) bool { return p.Key == e.Key }) {
			return fmt.Errorf("bwrap: Env[%d]: Key %q が重複している", i, e.Key)
		}
	}
	if s.Chdir != "" {
		if err := checkPath(s.Chdir); err != nil {
			return fmt.Errorf("bwrap: Chdir: %w", err)
		}
	}
	if len(s.Cmd) == 0 {
		return fmt.Errorf("bwrap: Cmd が空")
	}
	if err := checkPath(s.Cmd[0]); err != nil {
		return fmt.Errorf("bwrap: Cmd[0]: %w (PATH は検索しない)", err)
	}
	for i, c := range s.Cmd {
		if strings.ContainsRune(c, 0) {
			return fmt.Errorf("bwrap: Cmd[%d] が NUL を含む", i)
		}
	}
	return nil
}

// check は、h が検証に使える値かを確かめる。
func (h Host) check() error {
	if err := checkPath(h.Home); err != nil {
		return fmt.Errorf("Home: %w (CurrentHost を使う)", err)
	}
	if h.Home == "/" {
		return fmt.Errorf("Home が / (HOME の下の規則が意味を失う)")
	}
	for i, p := range h.Secrets {
		if err := checkPath(p); err != nil {
			return fmt.Errorf("Secrets[%d]: %w", i, err)
		}
	}
	return nil
}

// checkSrc は、ホストの path src (b の Src、または symlink を辿った実際の path) を、檻に見せてよいかを確かめる。
// lexical は、src が b.Src そのものか (InHome の付け忘れだけでなく、付けすぎも見る)。
func (h Host) checkSrc(b Bind, src string, lexical bool) error {
	if err := checkPath(src); err != nil {
		return fmt.Errorf("Src: %w", err)
	}
	switch {
	case under(h.Home, src):
		return fmt.Errorf("Src %q は、ホストの HOME 自体か、それを含む path (機密の path)", src)
	case under(src, h.Home):
		rel := strings.TrimPrefix(src, h.Home+"/")
		if isHomeSecret(rel) {
			return fmt.Errorf("Src %q は、HOME の下の機密の path (資格情報・ログイン状態)", src)
		}
		if !b.InHome {
			return fmt.Errorf("Src %q は HOME の下にある。作業用の dir なら InHome を明示する", src)
		}
	default:
		if lexical && b.InHome {
			return fmt.Errorf("Src %q に InHome を付けたが、HOME (%s) の下ではない", src, h.Home)
		}
		for _, p := range protectedTrees {
			if overlap(src, p) {
				return fmt.Errorf("Src %q は、機密の path %q と重なる (配下・親を含む)", src, p)
			}
		}
		if slices.Contains(wholeDenied, src) {
			return fmt.Errorf("Src %q は、ホスト全体で共有する dir (丸ごとは見せない。中の path は可)", src)
		}
	}
	for _, sec := range h.Secrets {
		if overlap(src, sec) {
			return fmt.Errorf("Src %q は、ホストの認証用 socket・鍵 %q と重なる (機密の path)", src, sec)
		}
	}
	for _, r := range h.resolvedTrees {
		if overlap(src, r) {
			return fmt.Errorf("Src %q は、機密の path の実体 %q (symlink の先) と重なる", src, r)
		}
	}
	if b.RW {
		for _, p := range readOnlyTrees {
			if under(src, p) {
				return fmt.Errorf("Src %q は、システムの path %q の下。ro でだけ bind できる", src, p)
			}
		}
	}
	return nil
}

// isHomeSecret は、HOME からの相対 path rel が、homeSecrets と重なる (配下・親を含む) か。
func isHomeSecret(rel string) bool {
	first, _, _ := strings.Cut(rel, "/")
	if strings.HasPrefix(first, ".claude") || strings.HasPrefix(first, ".gnupg") {
		return true
	}
	return slices.ContainsFunc(homeSecrets, func(s string) bool { return overlap(rel, s) })
}

// checkEnv は、環境変数 1 件を検証する。名前は英数字と _ だけ (= ・NUL・改行を含まない)、値は NUL と改行を含まない。
// 資格情報らしい名前は、呼び出し側が渡しても error にする。
func checkEnv(e EnvVar) error {
	valid := e.Key != ""
	for i, r := range e.Key {
		if !(r == '_' || 'A' <= r && r <= 'Z' || 'a' <= r && r <= 'z' || i > 0 && '0' <= r && r <= '9') {
			valid = false
		}
	}
	if !valid {
		return fmt.Errorf("Key %q は、英数字と _ だけの名前ではない", e.Key)
	}
	if strings.ContainsAny(e.Value, "\x00\n\r") {
		return fmt.Errorf("Key %q の値が、NUL か改行を含む", e.Key)
	}
	u := strings.ToUpper(e.Key)
	if slices.Contains(credEnvKeys, u) ||
		slices.ContainsFunc(credEnvPrefixes, func(p string) bool { return strings.HasPrefix(u, p) }) ||
		slices.ContainsFunc(credEnvParts, func(p string) bool { return strings.Contains(u, p) }) {
		return fmt.Errorf("Key %q は、資格情報らしい名前 (環境変数で運ばない)", e.Key)
	}
	return nil
}

// checkDst は、檻の中の path を検証する。/proc と /dev は、Spec が常に作るので、その中と、/ 自体は使えない。
func checkDst(dst string) error {
	if err := checkPath(dst); err != nil {
		return fmt.Errorf("Dst: %w", err)
	}
	if dst == "/" || under(dst, "/proc") || under(dst, "/dev") {
		return fmt.Errorf("Dst %q は使えない (/ と、/proc・/dev の中)", dst)
	}
	return nil
}

// checkPath は、p が絶対・クリーンで、制御文字を含まない path かを確かめる。
func checkPath(p string) error {
	switch {
	case p == "":
		return fmt.Errorf("path が空")
	case !filepath.IsAbs(p):
		return fmt.Errorf("path %q が絶対ではない", p)
	case hasControl(p):
		return fmt.Errorf("path %q が制御文字を含む", p)
	case filepath.Clean(p) != p:
		return fmt.Errorf("path %q がクリーンではない (%q)", p, filepath.Clean(p))
	}
	return nil
}

// hasControl は、s が制御文字 (NUL・改行・DEL を含む) を含むか。
func hasControl(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

// under は、p が root と等しいか、root の下にあるか (どちらもクリーンな絶対 path)。
func under(p, root string) bool {
	return root == "/" || p == root || strings.HasPrefix(p, root+"/")
}

// overlap は、a と b が、等しいか、どちらかがどちらかの下にあるか。
func overlap(a, b string) bool {
	return under(a, b) || under(b, a)
}
