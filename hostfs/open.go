package hostfs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

var (
	// ErrNotRegular は、名前が、通常のファイルではない (symlink・FIFO・ソケット・デバイス・ディレクトリ、または途中が symlink)。
	ErrNotRegular = errors.New("通常のファイルではない")
	// ErrTooLarge は、ファイルが、上限より大きい。
	ErrTooLarge = errors.New("大きさの上限を超えた")
	// ErrChanged は、開く間に、名前が別のものに差し替えられた (symlink を辿った可能性を含む)。
	ErrChanged = errors.New("開く間に、別のものに差し替えられた")
)

// hook は、テストが、開く処理の途中に、ファイルの差し替えを仕込む場所。本番は nil。stage は、下の定数。
type hook func(stage string)

const (
	stageBeforeOpen = "before-open" // 種別の確認の後、open の前
	stageAfterOpen  = "after-open"  // open の後、fd と名前の確認の前
	stageBeforeRead = "before-read" // 確認の後、読む前 (読む間に、ファイルが伸びる場面)
)

func (h hook) at(stage string) {
	if h != nil {
		h(stage)
	}
}

// Open は、root の中の通常のファイル name を、読み取りで開く。
//
// 檻が書いたファイルは敵対入力なので、名前が指すものを、次の順で確かめる。root の外へ出る名前は、os.Root が拒否する。
// 名前の途中と最後が symlink なら、辿らずに拒否する (Lstat)。FIFO・ソケット・デバイス・ディレクトリは、開かずに拒否する。
// 開いた後は、fd (fstat) と名前 (Lstat) が同じ通常のファイルであることと、大きさが maxSize 以下であることを確かめる。
func Open(root *os.Root, name string, maxSize int64) (*os.File, error) {
	return open(root, name, maxSize, nil)
}

// ReadFile は、Open したファイルの中身を、maxSize バイトまで読んで返す。宣言の大きさより後から伸びていても、超えた分は読まず、
// ErrTooLarge にする。
func ReadFile(root *os.Root, name string, maxSize int64) ([]byte, error) {
	return readFile(root, name, maxSize, nil)
}

func readFile(root *os.Root, name string, maxSize int64, h hook) ([]byte, error) {
	f, err := open(root, name, maxSize, h)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h.at(stageBeforeRead)
	data, err := io.ReadAll(io.LimitReader(f, maxSize+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if int64(len(data)) > maxSize {
		return nil, fmt.Errorf("%s: %w (読む間に伸びた。上限 %d バイト)", name, ErrTooLarge, maxSize)
	}
	return data, nil
}

func open(root *os.Root, name string, maxSize int64, h hook) (*os.File, error) {
	if maxSize < 0 {
		return nil, fmt.Errorf("hostfs: 上限 %d が負", maxSize)
	}
	if !filepath.IsLocal(name) {
		return nil, fmt.Errorf("hostfs: %q は、root の中の相対 path ではない", name)
	}
	if err := checkParents(root, name); err != nil {
		return nil, err
	}
	li, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !li.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: %w (%s)", name, ErrNotRegular, kindOf(li.Mode()))
	}
	h.at(stageBeforeOpen)
	f, err := root.OpenFile(name, readFlags, 0)
	if err != nil {
		return nil, err
	}
	h.at(stageAfterOpen)
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	li, err = root.Lstat(name)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("%s: 開いた後に確かめられない: %w", name, err)
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("%s: %w (開いたものは %s)", name, ErrNotRegular, kindOf(fi.Mode()))
	}
	if !os.SameFile(fi, li) { // fd が通常のファイルなので、名前が同じファイルなら、名前も通常のファイル
		f.Close()
		return nil, fmt.Errorf("%s: %w", name, ErrChanged)
	}
	if fi.Size() > maxSize {
		f.Close()
		return nil, fmt.Errorf("%s: %w (%d バイト。上限 %d バイト)", name, ErrTooLarge, fi.Size(), maxSize)
	}
	return f, nil
}

// checkParents は、name の途中のディレクトリが、すべて本物のディレクトリ (symlink でない) ことを確かめる。
// os.Root は、root の中を指す symlink を辿るので、途中が symlink でも、root の外へは出ないが、別の場所を指せる。
func checkParents(root *os.Root, name string) error {
	parts := strings.Split(name, "/")
	for i := 1; i < len(parts); i++ {
		dir := strings.Join(parts[:i], "/")
		fi, err := root.Lstat(dir)
		if err != nil {
			return err
		}
		if !fi.Mode().IsDir() {
			return fmt.Errorf("%s: %w (途中の %s が %s)", name, ErrNotRegular, dir, kindOf(fi.Mode()))
		}
	}
	return nil
}

// kindOf は、ファイルの種類を、エラー文に出す名前にする。
func kindOf(m fs.FileMode) string {
	switch {
	case m.IsRegular():
		return "通常のファイル"
	case m.IsDir():
		return "ディレクトリ"
	case m&fs.ModeSymlink != 0:
		return "symlink"
	case m&fs.ModeNamedPipe != 0:
		return "FIFO"
	case m&fs.ModeSocket != 0:
		return "ソケット"
	case m&fs.ModeDevice != 0:
		return "デバイス"
	}
	return "不明な種類"
}
