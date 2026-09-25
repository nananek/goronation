//go:build unix

package archtest

import (
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// nonRegularKinds は、通常のファイルではないものの種類。git は commit できないが、作業ツリーには置ける。
var nonRegularKinds = []string{"fifo", "socket", "device"}

// checkNoHang は、Check を別の goroutine で回し、FIFO などを開いて止まっていたら、テストを失敗にする。
// Check は、通常のファイルではないものを、開かずに種別だけで判定しなければならない
// (go tool は開いて読むが、writer が無いと止まる)。
func checkNoHang(t *testing.T, root string) ([]Violation, error) {
	t.Helper()
	type result struct {
		vs  []Violation
		err error
	}
	done := make(chan result, 1)
	go func() {
		vs, _, err := Check(root, DefaultRules)
		done <- result{vs, err}
	}()
	select {
	case r := <-done:
		return r.vs, r.err
	case <-time.After(10 * time.Second):
		t.Fatal("Check が止まった (FIFO などを開いた疑い)")
		return nil, nil
	}
}

// mkKind は、path に、kind の種類のファイルを作る。作れなければ skip する。
func mkKind(t *testing.T, kind, path string) {
	t.Helper()
	switch kind {
	case "fifo":
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Skipf("FIFO を作れない: %v", err)
		}
	case "socket":
		l, err := net.Listen("unix", path)
		if err != nil {
			t.Skipf("Unix ソケットを作れない: %v", err)
		}
		t.Cleanup(func() { l.Close() }) // Close は、ソケットのファイルを消す。Check が終わるまで閉じない
	case "device":
		if err := syscall.Mknod(path, syscall.S_IFCHR|0o600, 1<<8|3); err != nil {
			t.Skipf("デバイスのファイルを作れない (root が要る): %v", err)
		}
	default:
		t.Fatalf("知らない種類: %s", kind)
	}
}

// place は、root/dir/name に、kind の種類のものを置く。via が "direct" なら name そのもの、"symlink" なら
// name から実体への symlink、"chain" なら name → hop → 実体の symlink の連鎖。
func place(t *testing.T, root, dir, name, kind, via string) {
	t.Helper()
	d := filepath.Join(root, dir)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	link := func(target, path string) {
		t.Helper()
		if err := os.Symlink(target, filepath.Join(d, path)); err != nil {
			t.Skipf("symlink を作れない: %v", err)
		}
	}
	switch via {
	case "direct":
		mkKind(t, kind, filepath.Join(d, name))
	case "symlink":
		mkKind(t, kind, filepath.Join(d, "obj"))
		link("obj", name)
	case "chain":
		mkKind(t, kind, filepath.Join(d, "obj"))
		link("obj", "hop")
		link("hop", name)
	}
}

// TestNonRegularSourceKinds は、.go / go.mod / go.work の名前で、FIFO・Unix ソケット・デバイスのファイルが
// (直接でも、symlink や symlink の連鎖の先でも) あると、開かずに error にすることを確認する。
// TestNonRegularSource (FIFO の直接と symlink) を、種類と経路に広げたもの。
func TestNonRegularSourceKinds(t *testing.T) {
	for _, kind := range nonRegularKinds {
		for _, via := range []string{"direct", "symlink", "chain"} {
			for _, name := range []string{"evil.go", "go.mod", "go.work"} {
				t.Run(kind+"/"+via+"/"+name, func(t *testing.T) {
					root := t.TempDir()
					writeTree(t, root, map[string]string{"core/doc.go": "package core\n"})
					place(t, root, "core", name, kind, via)
					_, err := checkNoHang(t, root)
					if err == nil {
						t.Fatalf("error を返すべき (go tool は開いて build に使う)")
					}
					if !strings.Contains(err.Error(), "core/"+name) {
						t.Errorf("error に対象のファイルが含まれない: %v", err)
					}
				})
			}
		}
	}
}

// TestNonRegularUnrelatedNames は、build に使われない名前 (.go・go.mod・go.work 以外) の、通常のファイルではない
// ものは、これまでどおり黙って通す (error にも違反にもしない) ことを確認する。Go が build に使う .go 以外のソースの
// 拡張子 (.s など) は、種類によらず、名前で違反にする (non-go-source)。
func TestNonRegularUnrelatedNames(t *testing.T) {
	for _, kind := range nonRegularKinds {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			writeTree(t, root, map[string]string{"core/doc.go": "package core\n"})
			for _, name := range []string{"notes.txt", "Makefile", "go.mod.bak", "x.gox"} {
				place(t, root, "core", name, kind, "direct")
			}
			place(t, root, "core", "link.txt", kind, "symlink")
			vs, err := checkNoHang(t, root)
			if err != nil {
				t.Fatalf("error を返してはいけない: %v", err)
			}
			if len(vs) != 0 {
				t.Errorf("違反を出してはいけない: %v", keys(vs))
			}

			place(t, root, "core", "x.s", kind, "direct")
			vs, err = checkNoHang(t, root)
			if err != nil {
				t.Fatal(err)
			}
			if want := []string{"core/x.s:1: non-go-source"}; !slices.Equal(keys(vs), want) {
				t.Errorf("violations=%v (want %v)", keys(vs), want)
			}
		})
	}
}

// TestNonRegularVendorModules は、vendor/modules.txt が FIFO・Unix ソケット・デバイスのファイルでも (直接でも、
// symlink の先でも)、開かずに vendor-mode の違反にすることを確認する。go は、FIFO の modules.txt でも開いて読み、
// vendor の中の外部 module を build する (実測: writer を走らせると、vendor モードになり、os/exec に依存した)。
// modules.txt がディレクトリなら、go は読めずに失敗するので、違反にしない。
func TestNonRegularVendorModules(t *testing.T) {
	for _, kind := range nonRegularKinds {
		for _, via := range []string{"direct", "symlink", "chain"} {
			t.Run(kind+"/"+via, func(t *testing.T) {
				root := t.TempDir()
				writeTree(t, root, map[string]string{"core/doc.go": "package core\n"})
				place(t, root, "vendor", "modules.txt", kind, via)
				vs, err := checkNoHang(t, root)
				if err != nil {
					t.Fatal(err)
				}
				if want := []string{"vendor/modules.txt:1: vendor-mode"}; !slices.Equal(keys(vs), want) {
					t.Errorf("violations=%v (want %v)", keys(vs), want)
				}
			})
		}
	}

	t.Run("ディレクトリは違反にしない", func(t *testing.T) {
		root := t.TempDir()
		writeTree(t, root, map[string]string{"core/doc.go": "package core\n"})
		if err := os.MkdirAll(filepath.Join(root, "vendor", "modules.txt"), 0o755); err != nil {
			t.Fatal(err)
		}
		vs, err := checkNoHang(t, root)
		if err != nil {
			t.Fatal(err)
		}
		if len(vs) != 0 {
			t.Errorf("違反を出してはいけない: %v", keys(vs))
		}
	})
}
