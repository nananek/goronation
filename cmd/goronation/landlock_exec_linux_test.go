//go:build linux

package main

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"unsafe"
)

// bpfRun は、seccomp の BPF を、最小の解釈器で走らせる (kernel を使わずに、規則の意味を確かめる)。args は seccomp_data.args[i] の値。
func bpfRun(t *testing.T, arch, nr uint32, args [6]uint64) uint32 {
	t.Helper()
	prog, err := seccompProgram()
	if err != nil {
		t.Fatal(err)
	}
	var a uint32
	for pc := 0; pc < len(prog); pc++ {
		in := prog[pc]
		switch in.Code {
		case bpfLdAbs:
			switch {
			case in.K == offNr:
				a = nr
			case in.K == offArch:
				a = arch
			default:
				i := (int(in.K) - 16) / 8
				if (int(in.K)-16)%8 != 0 || i < 0 || i > 5 {
					t.Fatalf("想定外の offset %d", in.K)
				}
				a = uint32(args[i])
			}
		case bpfJeq:
			if a == in.K {
				pc += int(in.Jt)
			} else {
				pc += int(in.Jf)
			}
		case bpfJge:
			if a >= in.K {
				pc += int(in.Jt)
			} else {
				pc += int(in.Jf)
			}
		case bpfJset:
			if a&in.K != 0 {
				pc += int(in.Jt)
			} else {
				pc += int(in.Jf)
			}
		case bpfRet:
			return in.K
		default:
			t.Fatalf("想定外の命令 %#x", in.Code)
		}
	}
	t.Fatal("ret に着かずに終わった")
	return 0
}

// seccomp の BPF の規則 (表駆動): 拒否するもの・通すもの (通常の socket・sendto・ほかの syscall)。
func TestSeccompProgramRules(t *testing.T) {
	const allow, deny = seccompRetAllow, seccompRetErrno | errnoEPERM
	x := func(v ...uint64) [6]uint64 { var a [6]uint64; copy(a[:], v); return a }
	for _, tc := range []struct {
		name    string
		arch, n uint32
		args    [6]uint64
		want    uint32
	}{
		{"socket MPTCP", auditArchX86_64, sysSocket, x(2, 1, ipprotoMPTCP), deny},
		{"socket TCP", auditArchX86_64, sysSocket, x(2, 1, 6), allow},
		{"socket protocol 0", auditArchX86_64, sysSocket, x(2, 1, 0), allow},
		{"socket MPTCP (上位 32 bit はごみ)", auditArchX86_64, sysSocket, x(2, 1, 0xdead<<32|ipprotoMPTCP), deny},
		{"sendto MSG_FASTOPEN", auditArchX86_64, sysSendto, x(3, 0, 0, msgFastopen), deny},
		{"sendto MSG_FASTOPEN と他", auditArchX86_64, sysSendto, x(3, 0, 0, msgFastopen|0x4000), deny},
		{"sendto 通常", auditArchX86_64, sysSendto, x(3, 0, 0, 0x4000), allow},
		{"sendmsg MSG_FASTOPEN", auditArchX86_64, sysSendmsg, x(3, 0, msgFastopen), deny},
		{"sendmsg 通常", auditArchX86_64, sysSendmsg, x(3, 0, 0), allow},
		{"sendmmsg MSG_FASTOPEN", auditArchX86_64, sysSendmmsg, x(3, 0, 1, msgFastopen), deny},
		{"sendmmsg 通常", auditArchX86_64, sysSendmmsg, x(3, 0, 1, 0), allow},
		{"sendto の flags は args[3] (args[2] に立てても通る)", auditArchX86_64, sysSendto, x(3, 0, msgFastopen, 0), allow},
		{"io_uring_setup", auditArchX86_64, 425, x(), deny},
		{"io_uring_enter", auditArchX86_64, 426, x(), deny},
		{"io_uring_register", auditArchX86_64, 427, x(), deny},
		{"隣の番号 (424 pidfd_send_signal)", auditArchX86_64, 424, x(), allow},
		{"隣の番号 (428 open_tree)", auditArchX86_64, 428, x(), allow},
		{"read", auditArchX86_64, 0, x(), allow},
		{"connect", auditArchX86_64, 42, x(), allow},
		{"x32 の syscall", auditArchX86_64, x32SyscallBit | sysSocket, x(2, 1, 6), deny},
		{"x32 の io_uring", auditArchX86_64, x32SyscallBit | 425, x(), deny},
		{"arch が i386", 0x40000003, 0, x(), deny},
		{"arch が aarch64", 0xc00000b7, 0, x(), deny},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := bpfRun(t, tc.arch, tc.n, tc.args); got != tc.want {
				t.Errorf("ret = %#x, want %#x", got, tc.want)
			}
		})
	}
}

func TestParsePorts(t *testing.T) {
	for in, want := range map[string]string{"3128": "[3128]", "3128,8080": "[3128 8080]", "80,80,81": "[80 81]", "65535": "[65535]"} {
		got, err := parsePorts(in)
		if err != nil || fmt.Sprint(got) != want {
			t.Errorf("parsePorts(%q) = %v, %v; want %s", in, got, err, want)
		}
	}
	for _, in := range []string{"", "0", "65536", "-1", "abc", "80,", ",80", "80,,81", " 80", "080", "0x50", "+80", "80 "} {
		if got, err := parsePorts(in); err == nil {
			t.Errorf("parsePorts(%q) = %v: error が要る", in, got)
		}
	}
}

// fail closed: 手順のどれかが失敗したら、その先 (後ろの手順と exec) に進まず、EXE を起動しない。終了コードは exitLockdown。
func TestLandlockExecFailsClosed(t *testing.T) {
	type calls struct{ steps []string }
	mk := func(c *calls, failAt string, abi int, arch string) lockdown {
		step := func(name string) error {
			c.steps = append(c.steps, name)
			if name == failAt {
				return errors.New("失敗を模す")
			}
			return nil
		}
		return lockdown{
			arch: func() string { return arch },
			abi: func() (int, error) {
				if err := step("abi"); err != nil {
					return 0, err
				}
				return abi, nil
			},
			noNewPrivs: func() error { return step("nnp") },
			landlock:   func([]int) error { return step("landlock") },
			seccomp:    func() error { return step("seccomp") },
			exec: func(string, []string, []string) error {
				c.steps = append(c.steps, "exec")
				return errors.New("exec を模す")
			},
		}
	}
	args := []string{"--allow-connect", "3128", "--", "/bin/true"}
	for _, tc := range []struct {
		name, failAt string
		abi          int
		arch         string
		want         string // 呼ばれた手順
		msg          string
	}{
		{"全部成功 (exec まで進む)", "", 6, "amd64", "abi nnp landlock seccomp exec", "起動できない"},
		{"ABI を得られない", "abi", 6, "amd64", "abi", "Landlock を使えない"},
		{"ABI が 3", "", 3, "amd64", "abi", "ABI が 3"},
		{"ABI が 0", "", 0, "amd64", "abi", "ABI が 0"},
		{"no_new_privs の失敗", "nnp", 6, "amd64", "abi nnp", "no_new_privs"},
		{"Landlock の失敗", "landlock", 6, "amd64", "abi nnp landlock", "Landlock の制限"},
		{"seccomp の失敗", "seccomp", 6, "amd64", "abi nnp landlock seccomp", "seccomp の制限"},
		{"arch が arm64", "", 6, "arm64", "", "x86_64"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var c calls
			var stderr bytes.Buffer
			code := runLandlockExecWith(args, &stderr, mk(&c, tc.failAt, tc.abi, tc.arch))
			if got := strings.Join(c.steps, " "); got != tc.want {
				t.Errorf("呼ばれた手順 = %q, want %q", got, tc.want)
			}
			if !strings.Contains(stderr.String(), tc.msg) {
				t.Errorf("stderr に %q が無い: %q", tc.msg, stderr.String())
			}
			wantCode := exitLockdown
			if tc.name == "全部成功 (exec まで進む)" {
				wantCode = exitNoExec // exec の失敗は、制限の失敗とは別の終了コード
			}
			if code != wantCode {
				t.Errorf("終了コード = %d, want %d", code, wantCode)
			}
		})
	}
}

func TestLandlockExecUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"--", "/bin/true"},
		{"--allow-connect", "3128"},
		{"--allow-connect", "3128", "--"},
		{"--allow-connect", "3128", "--", "true"}, // 相対 path
		{"--allow-connect", "0", "--", "/bin/true"},
		{"--bogus", "--allow-connect", "3128", "--", "/bin/true"},
	} {
		var stderr bytes.Buffer
		code := runLandlockExecWith(args, &stderr, lockdown{exec: func(string, []string, []string) error { t.Errorf("%v: exec が呼ばれた", args); return nil }})
		if code != exitUsage {
			t.Errorf("%v: 終了コード = %d, want %d\n%s", args, code, exitUsage, stderr.String())
		}
	}
}

// ---- 実機: 実際の kernel に、Landlock・seccomp を掛けて確かめる (landlock-exec が exec した子が、試す) ----

const requireLandlockEnv = "GORO_REQUIRE_BWRAP" // CI で必須にする環境変数は、bwrap のテストと同じにする

func requireLandlock(t *testing.T) {
	t.Helper()
	if runtime := landlockSupported(); runtime != "" {
		if os.Getenv(requireLandlockEnv) == "1" {
			t.Fatalf("%s=1 だが Landlock (ABI 4 以上) を使えない: %s", requireLandlockEnv, runtime)
		}
		t.Skipf("Landlock (ABI 4 以上) を使えないため skip する (%s=1 で必須になる): %s", requireLandlockEnv, runtime)
	}
}

func landlockSupported() string {
	abi, err := landlockABI()
	if err != nil {
		return err.Error()
	}
	if abi < landlockMinABI {
		return "ABI " + strconv.Itoa(abi)
	}
	return ""
}

func init() {
	helpers["landlock-probe"] = landlockProbe
}

// landlockProbe は、landlock-exec の下で動く子: 引数 (許可ポート・許可外ポート) に対して、試した結果を "名前 => 結果" の行で出す。
func landlockProbe(args []string) int {
	okPort, badPort := args[0], args[1]
	out := func(name string, err error) {
		r := "OK"
		if err != nil {
			r = err.Error()
			var en syscall.Errno
			if errors.As(err, &en) {
				r = en.Error() + "/" + strconv.Itoa(int(en))
			}
		}
		fmt.Printf("%s => %s\n", name, r)
	}
	dial := func(port string) error {
		c, err := net.Dial("tcp", "127.0.0.1:"+port)
		if err == nil {
			c.Close()
		}
		return err
	}
	// bind は制限しない (opencode は、自分のポート (固定) を待ち受ける)。ポートは、空いている値を探して、閉じてから bind し直す。
	var bindErr error
	if l0, err := net.Listen("tcp", "127.0.0.1:0"); err != nil {
		bindErr = err
	} else {
		addr := l0.Addr().String()
		l0.Close()
		if l, err := net.Listen("tcp", addr); err != nil {
			bindErr = err
		} else {
			l.Close()
		}
	}
	out("bind-listen", bindErr)
	out("connect-allowed", dial(okPort))
	out("connect-denied", dial(badPort))
	// MPTCP の socket (IPPROTO_MPTCP = 262)。
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 262)
	if err == nil {
		syscall.Close(fd)
	}
	out("socket-mptcp", err)
	// MSG_FASTOPEN の sendto (許可外のポートへ)。
	if fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0); err == nil {
		bp, _ := strconv.Atoi(badPort)
		sa := &syscall.SockaddrInet4{Port: bp, Addr: [4]byte{127, 0, 0, 1}}
		out("sendto-fastopen", syscall.Sendto(fd, []byte("x"), 0x20000000, sa))
		syscall.Close(fd)
	}
	// sendmsg・sendmmsg の MSG_FASTOPEN: 接続していない TCP socket に、生の syscall で。
	for _, c := range []struct {
		name string
		nr   uintptr
	}{{"sendmsg-fastopen", syscall.SYS_SENDMSG}, {"sendmmsg-fastopen", 307}} {
		fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
		if err != nil {
			continue
		}
		bp, _ := strconv.Atoi(badPort)
		sa := syscall.RawSockaddrInet4{Family: syscall.AF_INET, Port: uint16(bp>>8 | bp&0xff<<8), Addr: [4]byte{127, 0, 0, 1}}
		iov := syscall.Iovec{Base: &[]byte{'x'}[0], Len: 1}
		hdr := syscall.Msghdr{Name: (*byte)(unsafe.Pointer(&sa)), Namelen: uint32(unsafe.Sizeof(sa)), Iov: &iov, Iovlen: 1}
		var e syscall.Errno
		if c.nr == syscall.SYS_SENDMSG {
			_, _, e = syscall.Syscall(c.nr, uintptr(fd), uintptr(unsafe.Pointer(&hdr)), 0x20000000)
		} else {
			mm := struct {
				hdr syscall.Msghdr
				n   uint32
				_   [4]byte
			}{hdr: hdr}
			_, _, e = syscall.Syscall6(c.nr, uintptr(fd), uintptr(unsafe.Pointer(&mm)), 1, 0x20000000, 0, 0)
		}
		if e == 0 {
			out(c.name, nil)
		} else {
			out(c.name, e)
		}
		syscall.Close(fd)
	}
	// io_uring_setup (425)。
	var params [120]byte
	_, _, e := syscall.Syscall(425, 8, uintptr(unsafe.Pointer(&params)), 0)
	if e == 0 {
		out("io_uring_setup", nil)
	} else {
		out("io_uring_setup", e)
	}
	// 制限の状態 (/proc/self/status)。
	st, _ := os.ReadFile("/proc/self/status")
	for _, l := range strings.Split(string(st), "\n") {
		if strings.HasPrefix(l, "NoNewPrivs:") || strings.HasPrefix(l, "Seccomp:") {
			f := strings.Fields(l)
			fmt.Printf("%s => %s\n", strings.TrimSuffix(strings.ToLower(f[0]), ":"), f[1])
		}
	}
	// 孫も同じ制限を受ける (fork された子が、許可外のポートに繋げない)。
	cmd := exec.Command(os.Args[0], helperArg, "landlock-grandchild", badPort)
	o, _ := cmd.Output()
	fmt.Printf("grandchild-connect-denied => %s\n", strings.TrimSpace(string(o)))
	return 0
}

func init() {
	helpers["landlock-grandchild"] = func(args []string) int {
		c, err := net.Dial("tcp", "127.0.0.1:"+args[0])
		if err == nil {
			c.Close()
			fmt.Println("OK")
			return 0
		}
		var en syscall.Errno
		if errors.As(err, &en) {
			fmt.Println(en.Error())
		} else {
			fmt.Println(err.Error())
		}
		return 0
	}
}

// listenLocal は、127.0.0.1 の空きポートで待ち受け、接続を受けて閉じ続ける listener を返す。
func listenLocal(t *testing.T) (port string, accepted func() int) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	n := make(chan struct{}, 100)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
			n <- struct{}{}
		}
	}()
	return strconv.Itoa(l.Addr().(*net.TCPAddr).Port), func() int { return len(n) }
}

// probeUnder は、landlock-exec --allow-connect allow -- (probe) を実行し、probe の出力を name => value の表にして返す。
func probeUnder(t *testing.T, allow, okPort, badPort string) map[string]string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, helperArg, "goronation", "landlock-exec", "--allow-connect", allow, "--", exe, helperArg, "landlock-probe", okPort, badPort)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("landlock-exec が失敗: %v\nstdout: %s\nstderr: %s", err, out, stderr.String())
	}
	got := map[string]string{}
	for _, l := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(l, " => "); ok {
			got[k] = v
		}
	}
	return got
}

// 実機: 許可したポートには繋がり、許可外は EACCES・MPTCP・MSG_FASTOPEN (sendto・sendmsg・sendmmsg)・io_uring は EPERM。
// no_new_privs と seccomp (mode 2) が立ち、孫も同じ制限を受ける。許可外のポートの listener には、何も届かない。
func TestLandlockExecRestrictsRealProcess(t *testing.T) {
	requireLandlock(t)
	okPort, _ := listenLocal(t)
	badPort, badAccepted := listenLocal(t)
	got := probeUnder(t, okPort, okPort, badPort)
	want := map[string]string{
		"bind-listen":               "OK",
		"connect-allowed":           "OK",
		"connect-denied":            "permission denied/13",
		"socket-mptcp":              "operation not permitted/1",
		"sendto-fastopen":           "operation not permitted/1",
		"sendmsg-fastopen":          "operation not permitted/1",
		"sendmmsg-fastopen":         "operation not permitted/1",
		"io_uring_setup":            "operation not permitted/1",
		"nonewprivs":                "1",
		"seccomp":                   "2",
		"grandchild-connect-denied": "permission denied",
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %q, want %q (全体: %v)", k, got[k], w, got)
		}
	}
	if n := badAccepted(); n != 0 {
		t.Errorf("許可外のポートの listener に、%d 個の接続が届いた", n)
	}
}

// 許可するポートが 2 つなら、どちらにも繋がる。許可に無いポートだけが断られる。
func TestLandlockExecAllowsSeveralPorts(t *testing.T) {
	requireLandlock(t)
	p1, _ := listenLocal(t)
	p2, _ := listenLocal(t)
	bad, _ := listenLocal(t)
	if got := probeUnder(t, p1+","+p2, p2, bad); got["connect-allowed"] != "OK" || got["connect-denied"] != "permission denied/13" {
		t.Errorf("2 ポートの許可: %v", got)
	}
}

// 実機 (コントロール): landlock-exec を通さずに動かすと、許可外のポートにも繋がり、MPTCP 以外の制限は無い。制限が、landlock-exec によるものだと確かめる。
func TestLandlockProbeWithoutLandlockExecIsUnrestricted(t *testing.T) {
	okPort, _ := listenLocal(t)
	badPort, _ := listenLocal(t)
	exe, _ := os.Executable()
	out, err := exec.Command(exe, helperArg, "landlock-probe", okPort, badPort).Output()
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, w := range []string{"connect-denied => OK", "seccomp => 0", "io_uring_setup => "} {
		if !strings.Contains(s, w) {
			t.Errorf("制限なしの出力に %q が無い:\n%s", w, s)
		}
	}
	if strings.Contains(s, "io_uring_setup => operation not permitted") {
		t.Errorf("制限なしで io_uring が EPERM: seccomp が掛かっている\n%s", s)
	}
}

// 実機: Landlock が使えない (ABI の検査が失敗する) ときの fail closed は、差し込み口の単体テストで確かめる (上)。ここでは、実プロセスの exec が
// 制限つきで行われること (exec 先の引数・環境変数がそのまま届くこと) を確かめる。
func TestLandlockExecPassesArgsAndEnv(t *testing.T) {
	requireLandlock(t)
	exe, _ := os.Executable()
	cmd := exec.Command(exe, helperArg, "goronation", "landlock-exec", "--allow-connect", "1", "--", exe, helperArg, "landlock-echo", "a b", "--x=1")
	cmd.Env = append(os.Environ(), "LANDLOCK_TEST_TOKEN=secret-value")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := string(out); got != "args=[a b --x=1] env=secret-value\n" {
		t.Errorf("exec 先の出力 = %q", got)
	}
}

func init() {
	helpers["landlock-echo"] = func(args []string) int {
		fmt.Printf("args=%v env=%s\n", args, os.Getenv("LANDLOCK_TEST_TOKEN"))
		return 0
	}
}

// 実際の配線: 本物の手順の関数 (差し込み口の既定) が、それぞれの役目の関数であること。テストの環境が no_new_privs を引き継いでいても、
// 配線の取り違え・抜け (no_new_privs の手順が空になる等) を見つける。
func TestRealLockdownWiring(t *testing.T) {
	l := realLockdown()
	for name, tc := range map[string]struct {
		fn   any
		want string
	}{
		"abi":        {l.abi, "landlockABI"},
		"noNewPrivs": {l.noNewPrivs, "setNoNewPrivs"},
		"landlock":   {l.landlock, "landlockRestrictConnect"},
		"seccomp":    {l.seccomp, "installSeccomp"},
		"exec":       {l.exec, "syscall.Exec"},
	} {
		got := runtimeFuncName(tc.fn)
		if !strings.HasSuffix(got, tc.want) {
			t.Errorf("%s = %s, want …%s", name, got, tc.want)
		}
	}
	if l.arch() != runtime.GOARCH {
		t.Errorf("arch = %s", l.arch())
	}
}

func runtimeFuncName(f any) string {
	return runtime.FuncForPC(reflect.ValueOf(f).Pointer()).Name()
}
