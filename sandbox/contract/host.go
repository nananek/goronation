package contract

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
)

// Host は、検証に使う、ホストの状態。
type Host struct {
	// Home は、ホストの HOME (絶対・クリーン。"/" でない)。
	Home string
	// Secrets は、ホストの認証用 socket・鍵の実体 (SSH_AUTH_SOCK の値など)。これと、その配下・親は、見せられない。
	Secrets []string
}

// CurrentHost は、実環境 (HOME・SSH_AUTH_SOCK・GNUPGHOME・XDG_RUNTIME_DIR・GPG_AGENT_INFO・DBUS_SESSION_BUS_ADDRESS) から Host を作る。
// HOME を決められないときは、Home が空になり、検証が error にする。HOME 環境変数が、passwd の HOME と違うときは、後者も Secrets に入れる
// (環境変数を差し替えて、本当の HOME を作業用 dir に見せかけさせない)。
func CurrentHost() Host {
	var h Host
	if home, err := os.UserHomeDir(); err == nil {
		h.Home = filepath.Clean(home)
	}
	add := func(p string) {
		if filepath.IsAbs(p) {
			h.Secrets = append(h.Secrets, filepath.Clean(p))
		}
	}
	if u, err := user.Current(); err == nil && filepath.Clean(u.HomeDir) != h.Home {
		add(u.HomeDir)
	}
	add(os.Getenv("SSH_AUTH_SOCK"))
	add(os.Getenv("GNUPGHOME"))
	add(os.Getenv("XDG_RUNTIME_DIR"))
	add(strings.SplitN(os.Getenv("GPG_AGENT_INFO"), ":", 2)[0])
	for _, part := range strings.Split(os.Getenv("DBUS_SESSION_BUS_ADDRESS"), ",") {
		if p, ok := strings.CutPrefix(part, "unix:path="); ok {
			add(p)
		}
	}
	return h
}

// check は、h が検証に使える値かを確かめる。
func (h Host) check() error {
	if err := CheckPath(h.Home); err != nil {
		return fmt.Errorf("Home: %w (CurrentHost を使う)", err)
	}
	if h.Home == "/" {
		return fmt.Errorf("Home が / (HOME の下の規則が意味を失う)")
	}
	for i, p := range h.Secrets {
		if err := CheckPath(p); err != nil {
			return fmt.Errorf("Secrets[%d]: %w", i, err)
		}
	}
	return nil
}

// Policy は、OS ごとの、システムの path の表。
type Policy struct {
	// ProtectedTrees は、その配下 (と、それを含む親) を、見せられない path。
	ProtectedTrees []string
	// WholeDenied は、丸ごとは見せられない path (ホスト全体で共有するので)。配下の path は見せられる。
	WholeDenied []string
	// ReadOnlyTrees は、Read でだけ見せられる path (システムの領域)。
	ReadOnlyTrees []string
}

// LinuxPolicy は、Linux の Policy。呼ぶたびに、新しい slice を返す。
func LinuxPolicy() Policy {
	return Policy{
		ProtectedTrees: []string{
			"/root", "/home", "/run", "/var/run", "/proc", "/sys", "/dev", "/boot",
			"/etc/shadow", "/etc/gshadow", "/etc/ssh", "/etc/sudoers", "/etc/sudoers.d", "/etc/ssl/private",
			"/var/lib/docker", "/var/lib/containerd",
		},
		WholeDenied:   []string{"/tmp", "/var/tmp"},
		ReadOnlyTrees: []string{"/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32", "/opt", "/etc", "/var"},
	}
}

// homeSecrets は、HOME からの相対で、資格情報・ログイン状態を持つ path (その配下と親は、見せられない)。
var homeSecrets = []string{
	".ssh", ".aws", ".azure", ".kube", ".docker",
	".netrc", ".git-credentials", ".npmrc", ".pypirc", ".password-store", ".codex", ".copilot", ".commandcode",
	".config/gh", ".config/git", ".config/gcloud", ".config/op", ".local/share/keyrings",
	".local/share/opencode", ".config/opencode",
}

// HomeSecrets は、HOME からの相対の、機密の path の表の写しを返す。このほかに、先頭の名前が .claude・.gnupg で始まるものも、機密 (IsHomeSecret)。
func HomeSecrets() []string { return slices.Clone(homeSecrets) }

// IsHomeSecret は、HOME からの相対 path rel が、機密の path と重なる (配下・親を含む) か。
func IsHomeSecret(rel string) bool {
	first, _, _ := strings.Cut(rel, "/")
	if strings.HasPrefix(first, ".claude") || strings.HasPrefix(first, ".gnupg") {
		return true
	}
	return slices.ContainsFunc(homeSecrets, func(s string) bool { return Overlap(rel, s) })
}
