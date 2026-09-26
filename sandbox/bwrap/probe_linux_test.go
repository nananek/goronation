package bwrap

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// このファイルは、実際に bwrap を起動するテストの道具。檻の中で、テストバイナリ自身が probe として動き
// (TestMain が、第 1 引数の印で切り替える)、檻の中から見えるものを JSON で報告する。
const (
	probeArg  = "--goro-bwrap-probe"  // 檻の中: probe として動く
	parentArg = "--goro-bwrap-parent" // ホスト: 檻を起動して、そのまま待つ親として動く
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case probeArg:
			probeMain(os.Args[2:])
			os.Exit(0)
		case parentArg:
			parentMain(os.Args[2:])
			os.Exit(0)
		}
	}
	os.Exit(m.Run())
}

// probeReport は、檻の中から見えたもの。
type probeReport struct {
	Env        []string
	Cwd        string
	PID, UID   int
	CapEff     string
	ProcPIDs   int               // /proc の、数字の名前の項目数
	ProcSelf   bool              // /proc/self/status を読めた
	Dev        []string          // /dev の項目
	DevOpen    map[string]string // /dev/null・zero・urandom を読めたか ("" なら読めた)
	Ifaces     []string          // ネットワークインターフェース
	Loopback   string            // 檻の中の loopback で、listen して自分に接続できたか ("" なら成功)
	Dials      map[string]string // 接続を試した宛先 → "" (成功) か error
	Stats      map[string]string // stat を試した path → "" (在る) か error
	Writes     map[string]string // 書き込みを試した path → "" (書けた) か error
	MarkerHits []string          // marker を含むファイル
	UDSReply   string            // -uds で、socket から受け取った応答
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// probeMain は、檻の中の probe。引数で頼まれたことを試し、報告を標準出力に JSON で書く。
func probeMain(args []string) {
	var dials, stats, writes multiFlag
	fl := flag.NewFlagSet("probe", flag.ExitOnError)
	fl.Var(&dials, "dial", "接続を試す宛先")
	fl.Var(&stats, "stat", "stat を試す path")
	fl.Var(&writes, "write", "書き込みを試す path")
	marker := fl.String("marker", "", "この文字列を含むファイルを、/ から探す")
	uds := fl.String("uds", "", "この Unix ドメインソケットに ping を送り、応答を報告する")
	udsTwice := fl.String("uds-twice", "", "-uds を 2 回 (間に、標準入力の 1 行を待つ)")
	sleep := fl.String("sleep", "", "何もせず 1 分待つ (値は、プロセスの目印)")
	noop := fl.Bool("noop", false, "何もせず終わる")
	_ = fl.Parse(args)

	switch {
	case *noop:
		return
	case *sleep != "":
		time.Sleep(time.Minute)
		return
	case *udsTwice != "":
		reply, err := udsPing(*udsTwice)
		fmt.Printf("first:%s:%v\n", reply, err)
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
		reply, err = udsPing(*udsTwice)
		fmt.Printf("second:%s:%v\n", reply, err)
		return
	}

	r := probeReport{
		Env: os.Environ(), PID: os.Getpid(), UID: os.Getuid(),
		DevOpen: map[string]string{}, Dials: map[string]string{}, Stats: map[string]string{}, Writes: map[string]string{},
	}
	r.Cwd, _ = os.Getwd()
	if b, err := os.ReadFile("/proc/self/status"); err == nil {
		r.ProcSelf = true
		for _, l := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(l, "CapEff:"); ok {
				r.CapEff = strings.TrimSpace(v)
			}
		}
	}
	if ents, err := os.ReadDir("/proc"); err == nil {
		for _, e := range ents {
			if strings.Trim(e.Name(), "0123456789") == "" {
				r.ProcPIDs++
			}
		}
	}
	if ents, err := os.ReadDir("/dev"); err == nil {
		for _, e := range ents {
			r.Dev = append(r.Dev, e.Name())
		}
	}
	for _, d := range []string{"/dev/null", "/dev/zero", "/dev/urandom"} {
		r.DevOpen[d] = errString(readOne(d))
	}
	if ifs, err := net.Interfaces(); err == nil {
		for _, i := range ifs {
			r.Ifaces = append(r.Ifaces, i.Name)
		}
	}
	r.Loopback = errString(selfLoopback())
	for _, d := range dials {
		c, err := net.DialTimeout("tcp", d, 3*time.Second)
		if err == nil {
			c.Close()
		}
		r.Dials[d] = errString(err)
	}
	for _, p := range stats {
		_, err := os.Lstat(p)
		r.Stats[p] = errString(err)
	}
	for _, p := range writes {
		err := os.WriteFile(p, []byte("x"), 0o600)
		if err == nil {
			_ = os.Remove(p)
		}
		r.Writes[p] = errString(err)
	}
	if *marker != "" {
		r.MarkerHits = findMarker(*marker)
	}
	if *uds != "" {
		reply, err := udsPing(*uds)
		r.UDSReply = reply
		if err != nil {
			r.UDSReply = "error: " + err.Error()
		}
	}
	_ = json.NewEncoder(os.Stdout).Encode(r)
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// readOne は、path から 1 バイト読む。
func readOne(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err = f.Read(make([]byte, 1)); err == io.EOF { // /dev/null は、EOF を返す
		return nil
	}
	return err
}

// selfLoopback は、loopback で listen し、自分に接続する (檻の中の loopback が使えることの確認)。
func selfLoopback() error {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer l.Close()
	go func() {
		if c, err := l.Accept(); err == nil {
			c.Close()
		}
	}()
	c, err := net.DialTimeout("tcp", l.Addr().String(), 3*time.Second)
	if err != nil {
		return err
	}
	return c.Close()
}

// findMarker は、/ から、marker を含む通常ファイル (1 MiB 以下) を探す。/proc・/sys・/dev・/usr は見ない。
func findMarker(marker string) []string {
	var hits []string
	_ = filepath.WalkDir("/", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && (p == "/proc" || p == "/sys" || p == "/dev" || p == "/usr") {
			return filepath.SkipDir
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if fi, err := d.Info(); err != nil || fi.Size() > 1<<20 {
			return nil
		}
		if b, err := os.ReadFile(p); err == nil && bytes.Contains(b, []byte(marker)) {
			hits = append(hits, p)
		}
		return nil
	})
	return hits
}

// udsPing は、Unix ドメインソケットに "ping\n" を送り、1 行の応答を返す。
func udsPing(path string) (string, error) {
	c, err := net.DialTimeout("unix", path, 3*time.Second)
	if err != nil {
		return "", err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(c, "ping\n"); err != nil {
		return "", err
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	return strings.TrimSpace(line), err
}

// parentMain は、檻 (probe が 1 分待つ) を起動して、そのまま待つ、ホスト側の親。
// 親が死んだとき、檻も死ぬこと (--die-with-parent) を、外のテストが確かめる。
func parentMain(args []string) {
	exe, _ := os.Executable()
	s := Spec{
		Host:     Host{Home: "/nonexistent-home"},
		Symlinks: usrSymlinks(),
		Binds:    []Bind{{Src: "/usr", Dst: "/usr"}, {Src: exe, Dst: "/opt/probe/probe"}},
		Cmd:      []string{"/opt/probe/probe", probeArg, "-sleep", args[0]},
	}
	c, err := Start(context.Background(), s)
	if err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}
	fmt.Printf("started %d\n", c.Pid())
	select {}
}

// usrSymlinks は、/usr の外の標準の名前 (/lib など) を、/usr の中へ向ける symlink。
func usrSymlinks() []Symlink {
	return []Symlink{
		{Target: "usr/lib", Dst: "/lib"}, {Target: "usr/lib64", Dst: "/lib64"},
		{Target: "usr/bin", Dst: "/bin"}, {Target: "usr/sbin", Dst: "/sbin"},
	}
}

// fakeHome は、ホストの HOME に見立てた一時ディレクトリに、資格情報の偽物 (marker を含む) を作る。
type fakeHome struct {
	Home   string
	Marker string
	Files  []string // 偽の資格情報のファイル (絶対 path)
	Sock   string   // SSH_AUTH_SOCK に見立てたファイル
}

func newFakeHome(t *testing.T) fakeHome {
	t.Helper()
	fh := fakeHome{Home: filepath.Join(t.TempDir(), "home"), Marker: fmt.Sprintf("GORO-FAKE-SECRET-%d", time.Now().UnixNano())}
	for _, rel := range []string{
		".ssh/id_ed25519", ".claude/.credentials.json", ".claude.json", ".gnupg/private-keys-v1.d/k.key",
		".aws/credentials", ".config/gh/hosts.yml", ".netrc", ".git-credentials", "notes.txt",
	} {
		p := filepath.Join(fh.Home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(fh.Marker), 0o600); err != nil {
			t.Fatal(err)
		}
		fh.Files = append(fh.Files, p)
	}
	fh.Sock = filepath.Join(t.TempDir(), "agent.sock")
	if err := os.WriteFile(fh.Sock, []byte(fh.Marker), 0o600); err != nil {
		t.Fatal(err)
	}
	return fh
}

func (fh fakeHome) host() Host {
	return Host{Home: fh.Home, Secrets: []string{fh.Sock}}
}

// probeSpec は、probe を檻の中で動かす Spec (spike の cage-final と同じ形。/usr を ro、作業用 dir を rw)。
// work と home は、ホストの一時ディレクトリ。extra で、Spec を変える。
func probeSpec(t *testing.T, host Host, work, home string, args ...string) Spec {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Spec{
		Host:     host,
		Symlinks: usrSymlinks(),
		Tmpfs:    []string{"/tmp"},
		Binds: []Bind{
			{Src: "/usr", Dst: "/usr"},
			{Src: exe, Dst: "/opt/probe/probe"},
			{Src: home, Dst: "/home/goro", RW: true},
			{Src: work, Dst: "/work", RW: true},
		},
		Env:   []EnvVar{{"HOME", "/home/goro"}, {"PATH", "/usr/bin:/bin"}, {"LANG", "C.UTF-8"}},
		Chdir: "/work",
		Cmd:   append([]string{"/opt/probe/probe", probeArg}, args...),
	}
}

// runProbe は、s の檻を起動して終わるのを待ち、probe の報告を返す。
func runProbe(t *testing.T, s Spec) probeReport {
	t.Helper()
	needBwrap(t)
	var stdout, stderr bytes.Buffer
	s.Stdout, s.Stderr = &stdout, &stderr
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := Start(ctx, s)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := c.Wait(); err != nil {
		t.Fatalf("檻の中の probe が失敗した: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}
	var r probeReport
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatalf("probe の報告を読めない: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}
	return r
}
