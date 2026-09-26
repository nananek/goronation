package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"time"
)

// errBudget は、予算 (limits) を超えたことを表す。
var errBudget = errors.New("予算を超えた")

// entryKind は、ディレクトリの項目の種類。symlink は辿らず、通常のファイルではないものは開かない。
type entryKind int

const (
	entRegular entryKind = iota
	entDir
	entSymlink
	entOther // FIFO・デバイス・ソケットなど
)

func (k entryKind) String() string {
	switch k {
	case entRegular:
		return "通常のファイル"
	case entDir:
		return "ディレクトリ"
	case entSymlink:
		return "symlink"
	}
	return "通常のファイルではないもの (FIFO・デバイス・ソケットなど)"
}

func kindOf(m fs.FileMode) entryKind {
	switch {
	case m&fs.ModeSymlink != 0:
		return entSymlink
	case m.IsDir():
		return entDir
	case m.IsRegular():
		return entRegular
	}
	return entOther
}

// dirEntry は、ディレクトリの項目 1 つ。種類は、開かずに Lstat で求めたもの。
type dirEntry struct {
	Name string
	Kind entryKind
}

// tree は、repo の root を開いた os.Root と、実行全体で共有する予算 (limits) の勘定。
// docgen の読み書きは、すべてこの型の methods を通る (tree を通らない読み書きは、source_test.go の自己検査が見つける)。
// os.Root は、root の外を指す名前 (".." や、外向きの symlink) を拒否する。symlink のツリー内への向きは、
// os.Root は辿るので、種類は Lstat で確かめ、symlink は辿らずに error にする。
type tree struct {
	root     *os.Root
	lim      limits
	now      func() time.Time
	deadline time.Time
	entries  int
	files    int
	bytes    int64

	// hook は、テストが、open の前後に、ファイルの差し替えを仕込む場所。本番は nil。
	hook func(stage, name string)
}

// hook の stage。
const (
	stageBeforeOpen = "before-open"
	stageAfterOpen  = "after-open" // open の後、fd と名前の確認の前
)

func (t *tree) stage(stage, name string) {
	if t.hook != nil {
		t.hook(stage, name)
	}
}

// verifySame は、開いた f が、名前 name そのものが指す want の種類のもので、同じファイルであることを確かめて、
// f の FileInfo を返す。os.Root は、root の中を指す symlink を辿って open するので、名前が symlink に
// 差し替えられても、open は成功する。fd (fstat) と、名前 (Lstat。symlink は辿らない) が同じファイルなら、
// 開いたものは、名前の実体そのもので、symlink を辿った先や、開く間に差し替えられたものではない。
func (t *tree) verifySame(f *os.File, name string, want entryKind) (os.FileInfo, error) {
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	li, err := t.root.Lstat(name)
	if err != nil {
		return nil, fmt.Errorf("%s: 開いた後に確かめられない: %w", name, err)
	}
	if got := kindOf(fi.Mode()); got != want {
		return nil, fmt.Errorf("%s: 開いたものが %s ではない (%s)", name, want, got)
	}
	if kindOf(li.Mode()) != want || !os.SameFile(fi, li) {
		return nil, fmt.Errorf("%s: 開く間に、別のものに差し替えられた (symlink を辿った可能性を含む)", name)
	}
	return fi, nil
}

// newTree は、root と予算から tree を作る。時間の予算は、このときから数える。
func newTree(root *os.Root, lim limits) *tree {
	t := &tree{root: root, lim: lim, now: time.Now}
	t.deadline = t.now().Add(lim.Timeout)
	return t
}

// tick は、項目 1 つを訪問した勘定を足し、項目の総数と時間の予算を確かめる。
func (t *tree) tick() error {
	t.entries++
	if t.entries > t.lim.MaxEntries {
		return fmt.Errorf("%w: 訪問した項目が %d を超えた", errBudget, t.lim.MaxEntries)
	}
	if t.now().After(t.deadline) {
		return fmt.Errorf("%w: 全体の時間が %s を超えた", errBudget, t.lim.Timeout)
	}
	return nil
}

// chargeFile は、ファイル 1 つ (大きさ size) を読む・書く勘定を足し、ファイルの数・大きさ・バイトの合計を確かめる。
func (t *tree) chargeFile(name string, size int64) error {
	t.files++
	switch {
	case t.files > t.lim.MaxFiles:
		return fmt.Errorf("%w: 読み書きするファイルが %d を超えた (%s)", errBudget, t.lim.MaxFiles, name)
	case size > t.lim.MaxFileBytes:
		return fmt.Errorf("%w: %s が 1 ファイルの上限 %d バイトを超えた", errBudget, name, t.lim.MaxFileBytes)
	}
	return t.chargeBytes(name, size)
}

func (t *tree) chargeBytes(name string, n int64) error {
	if t.bytes+n > t.lim.MaxTotalBytes {
		return fmt.Errorf("%w: 読み書きするバイトの合計が %d を超えた (%s)", errBudget, t.lim.MaxTotalBytes, name)
	}
	t.bytes += n
	return nil
}

// readFile は、通常のファイル name の中身を返す。種類を判定できないものがあっても止まらないよう、
// O_NONBLOCK で開き、開いた fd が、名前そのものの通常のファイルであることを確かめてから読む (verifySame)。
// 予算は、読む前に (宣言の大きさで) 確かめ、読み終えて、宣言より多ければ足す。
func (t *tree) readFile(name string) ([]byte, error) {
	if err := t.tick(); err != nil {
		return nil, err
	}
	t.stage(stageBeforeOpen, name)
	f, err := t.root.OpenFile(name, readFlags, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	t.stage(stageAfterOpen, name)
	info, err := t.verifySame(f, name, entRegular)
	if err != nil {
		return nil, err
	}
	if err := t.chargeFile(name, info.Size()); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, t.lim.MaxFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if int64(len(data)) > t.lim.MaxFileBytes {
		return nil, fmt.Errorf("%w: %s が 1 ファイルの上限 %d バイトを超えた", errBudget, name, t.lim.MaxFileBytes)
	}
	if extra := int64(len(data)) - info.Size(); extra > 0 {
		if err := t.chargeBytes(name, extra); err != nil {
			return nil, err
		}
	}
	return data, nil
}

// writeFile は、通常のファイル name に data を書く (親のディレクトリは作る)。書き込みは非原子的で
// (os.Root に Rename が無い)、途中で失敗すると壊れた出力が残る。それは -check が検出する。
// 既存の name が通常のファイルでなければ (symlink を含む) 触らずに error にする。開いた後も、名前そのものの
// ファイルであることを確かめてから、切り詰めて書く (symlink を辿って、別のファイルを上書きしない)。
func (t *tree) writeFile(name string, data []byte) error {
	if err := t.tick(); err != nil {
		return err
	}
	if err := t.chargeFile(name, int64(len(data))); err != nil {
		return err
	}
	if err := t.mkdirAll(path.Dir(name)); err != nil {
		return err
	}
	switch li, err := t.root.Lstat(name); {
	case err == nil && kindOf(li.Mode()) != entRegular:
		return fmt.Errorf("%s: 通常のファイルではない (%s) ので、書かない", name, kindOf(li.Mode()))
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return err
	}
	t.stage(stageBeforeOpen, name)
	f, err := t.root.OpenFile(name, writeFlags, 0o644)
	if err != nil {
		return err
	}
	t.stage(stageAfterOpen, name)
	_, err = t.verifySame(f, name, entRegular)
	if err == nil {
		err = f.Truncate(0)
	}
	if err == nil {
		_, err = f.Write(data)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("%s を書けない: %w", name, err)
	}
	return nil
}

// mkdirAll は、dir とその親を、1 つずつ作る (os.Root に MkdirAll は無い)。途中に、ディレクトリ以外
// (symlink を含む) があれば error にする。
func (t *tree) mkdirAll(dir string) error {
	if dir == "." {
		return nil
	}
	cur := ""
	for _, part := range strings.Split(dir, "/") {
		cur = path.Join(cur, part)
		info, err := t.root.Lstat(cur)
		switch {
		case err == nil:
			if !info.IsDir() {
				return fmt.Errorf("%s: ディレクトリではない (%s)", cur, kindOf(info.Mode()))
			}
		case errors.Is(err, fs.ErrNotExist):
			if err := t.root.Mkdir(cur, 0o755); err != nil {
				return err
			}
		default:
			return err
		}
	}
	return nil
}

// remove は、ファイルか、空のディレクトリ name を消す。
func (t *tree) remove(name string) error {
	if err := t.tick(); err != nil {
		return err
	}
	return t.root.Remove(name)
}

// lstat は、name の種類を、辿らずに返す。存在しなければ fs.ErrNotExist を返す。
// name の親が symlink でないことは、呼び出し側が、辿った経路で保証する (listDir の結果だけを渡す)。
func (t *tree) lstat(name string) (entryKind, error) {
	if err := t.tick(); err != nil {
		return 0, err
	}
	info, err := t.root.Lstat(name)
	if err != nil {
		return 0, err
	}
	return kindOf(info.Mode()), nil
}

// listDir は、ディレクトリ dir の項目を、名前の順に返す。開くのは O_DIRECTORY で、開いた後に、名前そのものの
// ディレクトリであることを確かめる (verifySame)。名前は Readdirnames で、256 個ずつ読み、1 つごとに予算を数える
// (項目が非常に多いディレクトリでも、上限を超えた時点で止まり、メモリを使い切らない)。
// 種類は、名前ごとに、root の中で Lstat して求める (DirEntry の種類は、判定できない環境では、
// root の外の path 文字列で lstat する実装になるため使わない)。
func (t *tree) listDir(dir string) ([]dirEntry, error) {
	if err := t.tick(); err != nil {
		return nil, err
	}
	t.stage(stageBeforeOpen, dir)
	f, err := t.root.OpenFile(dir, dirFlags, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	t.stage(stageAfterOpen, dir)
	if _, err := t.verifySame(f, dir, entDir); err != nil {
		return nil, err
	}
	var names []string
	for {
		batch, err := f.Readdirnames(256)
		for range batch {
			if err := t.tick(); err != nil {
				return nil, err
			}
		}
		names = append(names, batch...)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s を読めない: %w", dir, err)
		}
	}
	slices.Sort(names)
	out := make([]dirEntry, 0, len(names))
	for _, name := range names {
		info, err := t.root.Lstat(path.Join(dir, name))
		if err != nil {
			return nil, err
		}
		out = append(out, dirEntry{Name: name, Kind: kindOf(info.Mode())})
	}
	return out, nil
}

// referenceDir は、生成物を置くディレクトリ (repo 相対)。固定で、引数では変えられない。
const referenceDir = "docs/reference"

// inventory は、root の下を辿って見つけた、docgen が読むファイルの path (repo 相対、"/" 区切り、名前の順)。
type inventory struct {
	Go       []string // .go (_test.go を含む)
	Mod      []string // go.mod
	Markdown []string // .md (referenceDir の下は含めない。listReference が別に数える)
}

// skipDir は、辿らないディレクトリの名前か。tools/archtest と同じ (go tool の ./... が無視するものと、.git)。
func skipDir(name string) bool {
	return name == "testdata" || name == "vendor" ||
		strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}

func isGoFile(name string) bool { return strings.HasSuffix(name, ".go") }

func isMarkdown(name string) bool {
	l := strings.ToLower(name)
	return strings.HasSuffix(l, ".md") || strings.HasSuffix(l, ".markdown")
}

// isInput は、docgen が中身を読む名前 (.go・go.mod・.md) か。
func isInput(name string) bool { return isGoFile(name) || name == "go.mod" || isMarkdown(name) }

// inventory は、root の下を辿り、.go・go.mod・.md の path を集める。
//
// symlink は、名前によらず (辿らずに、開かずに) error にする。名前が .go・go.mod・.md で、通常のファイルではないもの
// (FIFO・デバイス・ソケット) も、開かずに error にする (開くと、writer が無い FIFO で止まる)。それ以外の名前は、
// 種類によらず読まないので、無視する。skipDir の名前のディレクトリと symlink は、辿らない。
// 集める path は、許可する文字だけで書かれていなければ error にする。
func (t *tree) inventory() (*inventory, error) {
	inv := new(inventory)
	if err := t.walk(".", 0, inv); err != nil {
		return nil, err
	}
	return inv, nil
}

// walk は、ディレクトリ dir (root から depth 段) の下を、名前の順に辿る。
func (t *tree) walk(dir string, depth int, inv *inventory) error {
	ents, err := t.listDir(dir)
	if err != nil {
		return err
	}
	for _, e := range ents {
		// 辿らないのは、ディレクトリと、ディレクトリかもしれない symlink だけ (tools/archtest と同じ)。
		// ファイルは、名前が . や _ で始まっても、.go・go.mod・.md なら数える (go tool は、そのような .go を build に
		// 使わないが、数えすぎる側に倒す)。
		// symlink は、名前が読む名前 (.go・go.mod・.md) なら、辿らない名前ではないので、error にする。
		if skipDir(e.Name) && (e.Kind == entDir || e.Kind == entSymlink && !isInput(e.Name)) {
			continue
		}
		p := path.Join(dir, e.Name)
		switch e.Kind {
		case entSymlink:
			return fmt.Errorf("%s: symlink は辿れない (辿らずに無視すると、見えないものを通す抜け道になるため error にする)", p)
		case entDir:
			if p == referenceDir {
				continue
			}
			if depth+1 > t.lim.MaxDepth {
				return fmt.Errorf("%w: %s がディレクトリの深さの上限 %d を超えた", errBudget, p, t.lim.MaxDepth)
			}
			if err := t.walk(p, depth+1, inv); err != nil {
				return err
			}
		default:
			var into *[]string
			switch {
			case isGoFile(e.Name):
				into = &inv.Go
			case e.Name == "go.mod":
				into = &inv.Mod
			case isMarkdown(e.Name):
				into = &inv.Markdown
			default:
				continue
			}
			if e.Kind == entOther {
				return fmt.Errorf("%s: %s は読めない (開くと writer が無いときに止まるため、開かずに error にする)", p, e.Kind)
			}
			if _, err := normalize(kindPath, p); err != nil {
				return err
			}
			*into = append(*into, p)
		}
	}
	return nil
}

// refEntry は、referenceDir の下の項目 1 つ。
type refEntry struct {
	Path string // repo 相対
	Dir  bool
}

// listReference は、referenceDir の下のすべての項目を、名前の順に返す (辿らないものは無い)。
// referenceDir が無ければ空。symlink・通常のファイルではないものは、開かずに error にする。
// referenceDir までの親 (docs) が symlink なら error にする。
func (t *tree) listReference() ([]refEntry, error) {
	cur := ""
	for _, part := range strings.Split(referenceDir, "/") {
		cur = path.Join(cur, part)
		k, err := t.lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if k != entDir {
			return nil, fmt.Errorf("%s: ディレクトリではない (%s)", cur, k)
		}
	}
	var out []refEntry
	var walk func(dir string, depth int) error
	walk = func(dir string, depth int) error {
		ents, err := t.listDir(dir)
		if err != nil {
			return err
		}
		for _, e := range ents {
			p := path.Join(dir, e.Name)
			switch e.Kind {
			case entRegular:
				out = append(out, refEntry{Path: p})
			case entDir:
				if depth+1 > t.lim.MaxDepth {
					return fmt.Errorf("%w: %s がディレクトリの深さの上限 %d を超えた", errBudget, p, t.lim.MaxDepth)
				}
				out = append(out, refEntry{Path: p, Dir: true})
				if err := walk(p, depth+1); err != nil {
					return err
				}
			default:
				return fmt.Errorf("%s: %s は、生成物の置き場に置けない (開かずに error にする)", p, e.Kind)
			}
		}
		return nil
	}
	if err := walk(referenceDir, strings.Count(referenceDir, "/")+1); err != nil {
		return nil, err
	}
	return out, nil
}
