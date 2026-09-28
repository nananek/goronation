package main

import (
	"errors"
	"fmt"
	"io"
	"syscall"
	"unsafe"
)

// termState は、端末 (fd) の termios。檻は端末に直結するので、檻の中のプロセスは、端末の設定 (raw・-echo・-isig など) を
// 変えられる。goronation run は、檻の起動の前に保存し、檻が終わった後に戻す (壊れた端末を、利用者の手元に残さない)。
type termState struct {
	fd int
	t  syscall.Termios
}

// ioctl は、fd に ioctl の req を、arg を指す構造体で発行する。
func ioctl(fd int, req uintptr, arg unsafe.Pointer) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(arg)); errno != 0 {
		return errno
	}
	return nil
}

// saveTermios は、fd の termios を保存する。fd が端末でなければ (ENOTTY)、nil と nil を返す (戻すものが無い)。
func saveTermios(fd int) (*termState, error) {
	s := &termState{fd: fd}
	if err := ioctl(fd, syscall.TCGETS, unsafe.Pointer(&s.t)); err != nil {
		if errors.Is(err, syscall.ENOTTY) {
			return nil, nil
		}
		return nil, err
	}
	return s, nil
}

// restore は、保存した termios を、すぐに戻す。s が nil (端末でなかった) なら何もしない。
func (s *termState) restore() error {
	if s == nil {
		return nil
	}
	return ioctl(s.fd, syscall.TCSETS, unsafe.Pointer(&s.t))
}

// tcsetsf は、termios を設定する ioctl の要求番号 (TCSETSF = TCSETS + TCSAFLUSH の効果)。標準ライブラリの syscall には
// 定数が無い (linux/amd64 の値。TCSETS (0x5402) の 2 つ後ろ)。
const tcsetsf = 0x5404

// restoreFlush は、保存した termios を、端末の入力キューの未読の入力 (貼り付けの続きなど) を捨てて戻す (TCSAFLUSH と同じ効果)。
// パスワードの入力 (getpass(3)・sudo) と同じ扱い: ECHO を戻した直後に、キューに残っていた入力が、利用者に見えないまま
// 次のコマンドとして実行される (pastejacking) のを防ぐ。s が nil なら何もしない。
func (s *termState) restoreFlush() error {
	if s == nil {
		return nil
	}
	return ioctl(s.fd, tcsetsf, unsafe.Pointer(&s.t))
}

// restoreTermios は、s を戻し、失敗したら警告を出す。端末が切れた (hangup。EIO) ときは、戻す先が無いので黙る。
func restoreTermios(s *termState, stderr io.Writer) {
	if err := s.restore(); err != nil && !errors.Is(err, syscall.EIO) {
		fmt.Fprintf(stderr, "goronation run: 端末の設定を戻せない (stty sane で戻す): %v\n", err)
	}
}

// rawOf は、tm を raw モード (stty raw -echo -isig 相当) にしたもの: local echo・行編集 (canonical)・特殊文字
// からのシグナル生成 (Ctrl-C など)・拡張処理を切り、入力の CR/NL 変換とソフトウェアのフロー制御を切り、出力の
// 後処理も切る。バイト列を変えずにそのまま通すための設定 (pty へ中継する側の、ホストの実端末に使う。中継先の
// pty の slave 側の termios は、エージェント自身が、いつもどおり自分で決める)。
func rawOf(tm syscall.Termios) syscall.Termios {
	tm.Lflag &^= syscall.ECHO | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	tm.Iflag &^= syscall.ICRNL | syscall.IXON | syscall.INLCR | syscall.ISTRIP
	tm.Oflag &^= syscall.OPOST
	tm.Cc[syscall.VMIN], tm.Cc[syscall.VTIME] = 1, 0
	return tm
}

// setRaw は、s (saveTermios が保存した元の設定) の端末を、rawOf の設定にする。s が nil (端末でない) なら何もしない
// (呼び手は、s が nil なら raw モードを使う中継もしない)。
func (s *termState) setRaw() error {
	if s == nil {
		return nil
	}
	raw := rawOf(s.t)
	return ioctl(s.fd, syscall.TCSETS, unsafe.Pointer(&raw))
}
