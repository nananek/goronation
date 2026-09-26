package conformance

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// probeArg は、檻の中で、テストバイナリが probe として動く印 (第 1 引数)。
const probeArg = "--goro-conformance-probe"

// init は、第 1 引数が probeArg なら、probe として動いて終わる (TestMain より前に動くので、バックエンドのテストは、何も書かなくてよい)。
func init() {
	if len(os.Args) > 1 && os.Args[1] == probeArg {
		probeMain(os.Args[2:])
		os.Exit(0)
	}
}

// report は、檻の中から見えたもの (probe が、標準出力に JSON で書く)。
type report struct {
	Env        []string
	Cwd        string
	Stats      map[string]string // stat を試した path → "" (在る) か error
	Lists      map[string]string // 一覧を試した path → "ok:a,b" か error
	Writes     map[string]string // 書き込みを試した path → "" (書けた) か error (書いたファイルは、消さない)
	Dials      map[string]string // 接続を試した宛先 (tcp) → "" (成功) か error
	UDS        map[string]string // socket に ping を送った結果 → 応答か error
	MarkerHits []string          // marker を含むファイル
	FdRead     string            // -fdread の結果 (読めた中身。読めなければ "err: ...")
	Tiocsti    string            // -tiocsti の結果 ("" = 注入できた。それ以外は、失敗の理由)
	Kill0      string            // -kill0 の結果 ("" = そのプロセスが見える。"esrch" = 見えない)
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// probeMain は、檻の中の probe。引数で頼まれたことを試し、報告を標準出力に JSON で書く。
func probeMain(args []string) {
	var stats, lists, writes, dials, udss multiFlag
	fl := flag.NewFlagSet("probe", flag.ExitOnError)
	fl.Var(&stats, "stat", "stat を試す path")
	fl.Var(&lists, "list", "一覧を試す path")
	fl.Var(&writes, "write", "書き込みを試す path (書いたファイルは消さない)")
	fl.Var(&dials, "dial", "接続を試す宛先 (tcp)")
	fl.Var(&udss, "uds", "ping を送る Unix ドメインソケット")
	marker := fl.String("marker", "", "この文字列を含むファイルを、/ から探す")
	fdread := fl.Int("fdread", -1, "この fd から読む")
	tiocsti := fl.Bool("tiocsti", false, "標準入力の端末に、TIOCSTI を試す")
	kill0 := fl.Int("kill0", 0, "この pid に、シグナル 0 を送る")
	exit := fl.Int("exit", -1, "何もせず、この終了コードで終わる")
	selfKill := fl.Bool("selfkill", false, "自分に SIGKILL を送る")
	sleep := fl.String("sleep", "", "何もせず 1 分待つ (値は、プロセスの目印)")
	hold := fl.String("hold", "", "ready を出して、殺されるまで待つ (値は、プロセスの目印)")
	spawn := fl.String("spawn", "", "setsid した子 (-sleep の値) を起こして、自分は終わる")
	udsTwice := fl.String("udstwice", "", "-uds を 2 回 (間に、標準入力の 1 行を待つ)")
	listen := fl.Bool("listen", false, "loopback で待ち受け、port を出して、標準入力が閉じるまで待つ")
	_ = fl.Parse(args)

	switch {
	case *exit >= 0:
		os.Exit(*exit)
	case *selfKill:
		killSelf()
		time.Sleep(time.Minute)
		return
	case *sleep != "":
		time.Sleep(time.Minute)
		return
	case *hold != "":
		fmt.Println("ready")
		select {}
	case *spawn != "":
		if err := spawnDetached(*spawn); err != nil {
			fmt.Println("spawn-error:", err)
			os.Exit(1)
		}
		return
	case *udsTwice != "":
		reply, err := udsPing(*udsTwice)
		fmt.Printf("first:%s:%v\n", reply, err)
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
		reply, err = udsPing(*udsTwice)
		fmt.Printf("second:%s:%v\n", reply, err)
		return
	case *listen:
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			fmt.Println("listen-error:", err)
			os.Exit(1)
		}
		fmt.Printf("port %d\n", l.Addr().(*net.TCPAddr).Port)
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				c.Close()
			}
		}()
		_, _ = io.Copy(io.Discard, os.Stdin)
		return
	}

	r := report{
		Env: os.Environ(), Stats: map[string]string{}, Lists: map[string]string{}, Writes: map[string]string{},
		Dials: map[string]string{}, UDS: map[string]string{},
	}
	r.Cwd, _ = os.Getwd()
	for _, p := range stats {
		_, err := os.Lstat(p)
		r.Stats[p] = errString(err)
	}
	for _, p := range lists {
		ents, err := os.ReadDir(p)
		if err != nil {
			r.Lists[p] = "err: " + err.Error()
			continue
		}
		names := make([]string, 0, len(ents))
		for _, e := range ents {
			names = append(names, e.Name())
		}
		r.Lists[p] = "ok:" + strings.Join(names, ",")
	}
	for _, p := range writes {
		r.Writes[p] = errString(os.WriteFile(p, []byte("x"), 0o600))
	}
	for _, d := range dials {
		c, err := net.DialTimeout("tcp", d, 3*time.Second)
		if err == nil {
			c.Close()
		}
		r.Dials[d] = errString(err)
	}
	for _, p := range udss {
		reply, err := udsPing(p)
		if err != nil {
			reply = "error: " + err.Error()
		}
		r.UDS[p] = reply
	}
	if *marker != "" {
		r.MarkerHits = findMarker(*marker)
	}
	if *fdread >= 0 {
		r.FdRead = readFd(*fdread)
	}
	if *tiocsti {
		r.Tiocsti = tiocstiOn(0)
	}
	if *kill0 != 0 {
		r.Kill0 = kill0Result(*kill0)
	}
	_ = json.NewEncoder(os.Stdout).Encode(r)
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// readFd は、fd から読む (継承した fd が、檻に届いていないことの確認)。読めなければ "err: ..."。
func readFd(fd int) string {
	f := os.NewFile(uintptr(fd), "inherited")
	if f == nil {
		return "err: fd が無い"
	}
	buf := make([]byte, 256)
	n, err := f.Read(buf)
	if err != nil && n == 0 {
		return "err: " + err.Error()
	}
	return string(buf[:n])
}

// skipDirs は、marker の探索で見ない、システムのディレクトリ。
var skipDirs = []string{"/proc", "/sys", "/dev", "/usr", "/System", "/Library", "/Applications", "/private/var/db"}

// findMarker は、/ から、marker を含む通常ファイル (1 MiB 以下) を探す。
func findMarker(marker string) []string {
	var hits []string
	_ = filepath.WalkDir("/", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			for _, s := range skipDirs {
				if p == s {
					return filepath.SkipDir
				}
			}
			return nil
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

// kill0Result は、pid にシグナル 0 を送った結果 ("" = 見える・"esrch" = 見えない)。
func kill0Result(pid int) string {
	err := kill0(pid)
	if err != nil && isESRCH(err) {
		return "esrch"
	}
	return "" // nil か EPERM など: プロセスは在る (見える)
}

// portOf は、"port N" の行から、N を取り出す。
func portOf(line string) (int, bool) {
	s, ok := strings.CutPrefix(line, "port ")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	return n, err == nil
}
