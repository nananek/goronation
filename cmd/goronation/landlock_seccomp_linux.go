//go:build linux

package main

import (
	"fmt"
	"syscall"
)

// seccomp の BPF (ADR 0020 決定 3)。既定は ALLOW。次を EPERM にする (Landlock の TCP の connect の制限が止めない経路の予防):
//   - arch が x86_64 以外・x32 の syscall (syscall 番号の解釈が変わるので、フィルターを迂回される)
//   - socket の protocol が IPPROTO_MPTCP (262)
//   - sendto・sendmsg・sendmmsg の flags に MSG_FASTOPEN
//   - io_uring_setup・io_uring_enter・io_uring_register (seccomp を通らずに socket の作成・送信ができる)
const (
	auditArchX86_64 = 0xc000003e
	x32SyscallBit   = 0x40000000

	sysSocket      = 41
	sysSendto      = 44
	sysSendmsg     = 46
	sysSendmmsg    = 307
	sysIoUringBase = 425 // io_uring_setup=425・enter=426・register=427

	ipprotoMPTCP = 262
	msgFastopen  = 0x20000000

	seccompRetAllow = 0x7fff0000
	seccompRetErrno = 0x00050000 // | errno
	errnoEPERM      = 1

	// seccomp_data の offset: nr (0)・arch (4)・args[i] の下位 32 bit (16 + 8*i)。
	offNr   = 0
	offArch = 4

	bpfLdAbs = 0x20 // BPF_LD | BPF_W | BPF_ABS
	bpfJeq   = 0x15 // BPF_JMP | BPF_JEQ | BPF_K
	bpfJge   = 0x35 // BPF_JMP | BPF_JGE | BPF_K
	bpfJset  = 0x45 // BPF_JMP | BPF_JSET | BPF_K
	bpfRet   = 0x06 // BPF_RET | BPF_K
)

func offArg(i int) uint32 { return uint32(16 + 8*i) }

// bpfStep は、label つきの BPF の命令 1 つ。jt・jf は飛び先の label (空なら次の命令)。
type bpfStep struct {
	label  string
	code   uint16
	k      uint32
	jt, jf string
}

// seccompSteps は、フィルターの命令の並び。
func seccompSteps() []bpfStep {
	ld := func(off uint32) bpfStep { return bpfStep{code: bpfLdAbs, k: off} }
	return []bpfStep{
		ld(offArch),
		{code: bpfJeq, k: auditArchX86_64, jf: "deny"},
		ld(offNr),
		{code: bpfJge, k: x32SyscallBit, jt: "deny"},
		{code: bpfJeq, k: sysSocket, jt: "socket"},
		{code: bpfJeq, k: sysSendto, jt: "sendto"},
		{code: bpfJeq, k: sysSendmsg, jt: "sendmsg"},
		{code: bpfJeq, k: sysSendmmsg, jt: "sendmmsg"},
		{code: bpfJeq, k: sysIoUringBase, jt: "deny"},
		{code: bpfJeq, k: sysIoUringBase + 1, jt: "deny"},
		{code: bpfJeq, k: sysIoUringBase + 2, jt: "deny"},
		{code: bpfRet, k: seccompRetAllow},

		{label: "socket", code: bpfLdAbs, k: offArg(2)}, // protocol
		{code: bpfJeq, k: ipprotoMPTCP, jt: "deny", jf: "allow"},
		{label: "sendto", code: bpfLdAbs, k: offArg(3)}, // flags
		{code: bpfJset, k: msgFastopen, jt: "deny", jf: "allow"},
		{label: "sendmsg", code: bpfLdAbs, k: offArg(2)},
		{code: bpfJset, k: msgFastopen, jt: "deny", jf: "allow"},
		{label: "sendmmsg", code: bpfLdAbs, k: offArg(3)},
		{code: bpfJset, k: msgFastopen, jt: "deny", jf: "allow"},

		{label: "deny", code: bpfRet, k: seccompRetErrno | errnoEPERM},
		{label: "allow", code: bpfRet, k: seccompRetAllow},
	}
}

// seccompProgram は、seccompSteps の label を、相対 offset (0〜255) にして、kernel に渡す形にする。
func seccompProgram() ([]syscall.SockFilter, error) {
	steps := seccompSteps()
	at := map[string]int{}
	for i, s := range steps {
		if s.label != "" {
			at[s.label] = i
		}
	}
	out := make([]syscall.SockFilter, len(steps))
	for i, s := range steps {
		off := func(label string) (uint8, error) {
			if label == "" {
				return 0, nil
			}
			to, ok := at[label]
			if !ok || to <= i || to-i-1 > 255 {
				return 0, fmt.Errorf("seccomp の BPF: 命令 %d の飛び先 %q が不正", i, label)
			}
			return uint8(to - i - 1), nil
		}
		jt, err := off(s.jt)
		if err != nil {
			return nil, err
		}
		jf, err := off(s.jf)
		if err != nil {
			return nil, err
		}
		out[i] = syscall.SockFilter{Code: s.code, Jt: jt, Jf: jf, K: s.k}
	}
	return out, nil
}
