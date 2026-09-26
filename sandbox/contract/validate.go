package contract

import (
	"fmt"
	"maps"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"

	"github.com/nananek/goronation/core/sandbox"
)

// Rules は、Spec の検証に使う規則: ホストの状態・OS の表・バックエンドの Capabilities。
type Rules struct {
	Host   Host
	Policy Policy
	Caps   sandbox.Capabilities
}

// Resolved は、Resolve の結果: Read と Write の各 Mount の HostPath を、symlink を辿った実体にしたもの (Spec と同じ順)。
type Resolved struct {
	Read, Write []string
}

// reject は、契約に反することを表す error (errors.Is(err, sandbox.ErrRejected))。
func reject(format string, a ...any) error {
	return fmt.Errorf("%w: %s", sandbox.ErrRejected, fmt.Sprintf(format, a...))
}

// Validate は、s を、文字列だけで検証する (ファイルシステムも環境も見ない純関数)。契約に反すれば、ErrRejected を包んだ error を返す。
// symlink を辿った実際の path の検証は、Resolve の役目。
func (r Rules) Validate(s sandbox.Spec) error {
	if err := r.Host.check(); err != nil {
		return reject("Host: %v", err)
	}
	// バックエンドが檻に足す環境変数の宣言 (ExtraEnv) も、資格情報らしい名前・不正な名前は断る (宣言で、資格情報を通させない)。
	for i, name := range r.Caps.ExtraEnv {
		if err := CheckEnv(name, ""); err != nil {
			return reject("Capabilities.ExtraEnv[%d]: %v (バックエンドの宣言。資格情報らしい名前は、宣言しても足せない)", i, err)
		}
	}
	if err := CheckPath(s.Exec); err != nil {
		return reject("Exec: %v (PATH は検索しない)", err)
	}
	for i, a := range s.Args {
		if strings.ContainsRune(a, 0) {
			return reject("Args[%d] が NUL を含む", i)
		}
	}
	if s.Dir != "" {
		if err := CheckPath(s.Dir); err != nil {
			return reject("Dir: %v", err)
		}
	}
	for i, e := range s.Env {
		if err := CheckEnv(e.Key, e.Value); err != nil {
			return reject("Env[%d]: %v", i, err)
		}
		if slices.ContainsFunc(s.Env[:i], func(p sandbox.EnvVar) bool { return p.Key == e.Key }) {
			return reject("Env[%d]: Key %q が重複している", i, e.Key)
		}
	}
	guests := map[string]string{} // 檻の中の path → 持ち主 (重複を断る)
	claim := func(what, guest string) error {
		if err := CheckPath(guest); err != nil {
			return reject("%s: 檻の中の path: %v", what, err)
		}
		if guest == "/" {
			return reject("%s: 檻の中の path が / (丸ごと置き換えられない)", what)
		}
		for _, p := range r.Policy.GuestReserved {
			if Under(guest, p) {
				return reject("%s: 檻の中の path %q が、バックエンドが常に作る %q の中 (作ったものを差し替えられない)", what, guest, p)
			}
		}
		if prev, dup := guests[guest]; dup {
			return reject("檻の中の path %q が重複している (%s と %s)", guest, prev, what)
		}
		guests[guest] = what
		return nil
	}
	// System が占める path (基盤の bind と symlink) を先に取る: 同じ path の Mount・Scratch は、重複で断る。
	var systemBinds, systemLinks, mountGuests []string
	if s.System {
		for _, p := range r.Policy.SystemPaths {
			if err := claim("System", p); err != nil {
				return err
			}
			systemBinds = append(systemBinds, p)
		}
		for _, p := range r.Policy.SystemLinks {
			if err := claim("System", p); err != nil {
				return err
			}
			systemLinks = append(systemLinks, p)
		}
	}
	for _, g := range []struct {
		name  string
		list  []sandbox.Mount
		write bool
	}{{"Read", s.Read, false}, {"Write", s.Write, true}} {
		for i, m := range g.list {
			what := fmt.Sprintf("%s[%d]", g.name, i)
			if err := CheckPath(m.HostPath); err != nil {
				return reject("%s: HostPath: %v", what, err)
			}
			if m.GuestPath != "" && m.GuestPath != m.HostPath && !r.Caps.PathRemap {
				return reject("%s: GuestPath %q は HostPath %q と違う (このバックエンドは、path を付け替えられない)", what, m.GuestPath, m.HostPath)
			}
			if err := claim(what, m.Guest()); err != nil {
				return err
			}
			mountGuests = append(mountGuests, m.Guest())
			if err := r.checkSource(m, m.HostPath, g.write, true, nil); err != nil {
				return reject("%s: %v", what, err)
			}
		}
	}
	for i, p := range s.Scratch {
		what := fmt.Sprintf("Scratch[%d]", i)
		if !r.Caps.PathRemap {
			return reject("%s: 檻専用の path を選べない (このバックエンドは、path を付け替えられない)", what)
		}
		if err := claim(what, p); err != nil {
			return err
		}
		// Scratch (tmpfs) は、Mount より先に作られる。Mount・基盤の bind の内側に置くと、後から bind される Mount が Scratch を隠す
		// (rw の Mount なら、書いたものがホストに残る。ro なら、書けない)。
		for _, g := range slices.Concat(mountGuests, systemBinds) {
			if p != g && Under(p, g) {
				return reject("%s: 檻の中の path %q が、Mount か基盤 (%q) の内側 (後から bind される Mount が Scratch を隠し、ホストに残らない、に反する)", what, p, g)
			}
		}
	}
	// System の symlink の下には、何も置けない。
	for _, g := range slices.Sorted(maps.Keys(guests)) {
		for _, link := range systemLinks {
			if g != link && Under(g, link) {
				return reject("%s: 檻の中の path %q が、System の symlink %q の下 (symlink を辿って、別の path に届く)", guests[g], g, link)
			}
		}
	}
	if s.Egress != "" {
		if err := CheckPath(s.Egress); err != nil {
			return reject("Egress: %v", err)
		}
		dir := filepath.Dir(s.Egress)
		// Write に、同じ GuestPath の Mount は置けない (重複で断る)ので、Read で見えれば、ro になる (檻が、ソケットを差し替えられない)。
		if !slices.ContainsFunc(s.Read, func(m sandbox.Mount) bool { return m.Guest() == dir }) {
			return reject("Egress %q の親ディレクトリ %q が、Read に無い (ソケットは作り直されるので、ディレクトリを見せる)", s.Egress, dir)
		}
	}
	for i, l := range s.Loopback {
		if ap, err := netip.ParseAddrPort(l); err != nil || !ap.Addr().IsLoopback() {
			return reject("Loopback[%d]: %q は loopback の IP リテラル:ポートではない", i, l)
		}
	}
	return nil
}

// checkSource は、ホストの path src (m の HostPath、または symlink を辿った実際の path) を、檻に見せてよいかを確かめる。
// lexical は、src が m.HostPath そのものか (InHome の付け忘れだけでなく、付けすぎも見る)。trees は、Resolve が足す、拒否する path の実体。
func (r Rules) checkSource(m sandbox.Mount, src string, write, lexical bool, trees []string) error {
	if err := CheckPath(src); err != nil {
		return fmt.Errorf("HostPath: %w", err)
	}
	h := r.Host
	switch {
	case Under(h.Home, src):
		return fmt.Errorf("HostPath %q は、ホストの HOME 自体か、それを含む path (機密の path)", src)
	case Under(src, h.Home):
		rel := strings.TrimPrefix(src, h.Home+"/")
		if IsHomeSecret(rel) {
			return fmt.Errorf("HostPath %q は、HOME の下の機密の path (資格情報・ログイン状態)", src)
		}
		if !m.InHome {
			return fmt.Errorf("HostPath %q は HOME の下にある。作業用の dir なら InHome を明示する", src)
		}
	default:
		if lexical && m.InHome {
			return fmt.Errorf("HostPath %q に InHome を付けたが、HOME (%s) の下ではない", src, h.Home)
		}
		for _, p := range r.Policy.ProtectedTrees {
			if Overlap(src, p) {
				return fmt.Errorf("HostPath %q は、機密の path %q と重なる (配下・親を含む)", src, p)
			}
		}
		if slices.Contains(r.Policy.WholeDenied, src) {
			return fmt.Errorf("HostPath %q は、ホスト全体で共有する dir (丸ごとは見せない。中の path は可)", src)
		}
	}
	for _, sec := range h.Secrets {
		if Overlap(src, sec) {
			return fmt.Errorf("HostPath %q は、ホストの認証用 socket・鍵 %q と重なる (機密の path)", src, sec)
		}
	}
	for _, t := range trees {
		if Overlap(src, t) {
			return fmt.Errorf("HostPath %q は、機密の path の実体 %q (symlink の先) と重なる", src, t)
		}
	}
	if write {
		for _, p := range r.Policy.ReadOnlyTrees {
			if Under(src, p) {
				return fmt.Errorf("HostPath %q は、システムの path %q の下。Read でだけ見せられる", src, p)
			}
		}
	}
	return nil
}

// Resolve は、s の各 Mount の HostPath を、symlink を辿った実際の path にし、同じ規則で検証する (機密の path を指す symlink を、
// 機密でない名前で見せないため)。ファイルシステムを見る。Validate に通った Spec に使う。辿れない・機密に重なるときは、ErrRejected を包んだ error を返す。
// 起動器に、字面の path でなく、返した実際の path を渡すのは、バックエンドの責任 (bwrap のアダプタは、bwrap の Start が解決し直すので、この結果は検査にだけ使う)。
func (r Rules) Resolve(s sandbox.Spec) (Resolved, error) {
	host := r.Host
	host.Home = evalOrSelf(host.Home)
	host.Secrets = slices.Clone(r.Host.Secrets)
	for _, sec := range r.Host.Secrets {
		host.Secrets = append(host.Secrets, evalOrSelf(sec))
	}
	// 拒否する path (Policy の保護する path と、HOME の機密) 自体が symlink のとき、その先も拒否する。
	deny := slices.Clone(r.Policy.ProtectedTrees)
	for _, rel := range slices.Concat(homeSecrets, []string{".claude", ".claude.json", ".gnupg", ".gnupg-vault"}) {
		deny = append(deny, r.Host.Home+"/"+rel, host.Home+"/"+rel)
	}
	var trees []string
	for _, p := range deny {
		if real, err := filepath.EvalSymlinks(p); err == nil && real != p {
			trees = append(trees, real)
		}
	}
	rr := Rules{Host: host, Policy: r.Policy, Caps: r.Caps}
	var out Resolved
	for _, g := range []struct {
		name  string
		list  []sandbox.Mount
		write bool
		dst   *[]string
	}{{"Read", s.Read, false, &out.Read}, {"Write", s.Write, true, &out.Write}} {
		for i, m := range g.list {
			real, err := filepath.EvalSymlinks(m.HostPath)
			if err != nil {
				return Resolved{}, reject("%s[%d]: HostPath %q を解決できない: %v", g.name, i, m.HostPath, err)
			}
			if err := rr.checkSource(m, real, g.write, false, trees); err != nil {
				return Resolved{}, reject("%s[%d]: HostPath %q は %q に解決され、拒否: %v", g.name, i, m.HostPath, real, err)
			}
			*g.dst = append(*g.dst, real)
		}
	}
	return out, nil
}

// evalOrSelf は、p の symlink を辿った path (辿れなければ p)。
func evalOrSelf(p string) string {
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	return p
}
