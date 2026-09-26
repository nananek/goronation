//go:build unix

package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// 攻撃者視点レビュー (2d63f6d) の finding 3: 生成物 (docs/reference/core.md など) が、root の外のファイルへのハードリンク
// だと、verifySame (fd と名前が同じ inode) を通り、切り詰めて書くことで、root の外のファイルを書き換えていた。
// 書き込み先のファイルのリンク数が 1 より大きければ、書かずに error にする。読み取り (入力) は、変えない。

// victimFile は、root の外に、被害者のファイルを作る。
func victimFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "victim.txt")
	if err := os.WriteFile(p, []byte("VICTIM\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func hardLink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(target, link); err != nil {
		t.Skipf("ハードリンクを作れない: %v", err)
	}
}

func mustRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestHardLinkedOutputIsRejected は、書き込み先 (生成物・ADR の README) が、root の外のファイルへのハードリンクなら、
// 何も書かずに error になり (書く前の checkPlan が止める。ほかの生成物も、書き換わらない)、外のファイルが無変更なことを確認する。
func TestHardLinkedOutputIsRejected(t *testing.T) {
	for _, target := range []string{"docs/reference/core.md", "docs/reference/README.md", "docs/adr/README.md"} {
		t.Run(target, func(t *testing.T) {
			victim := victimFile(t)
			tr, dir := writeRepo(t, nil)
			res, err := tr.analyze()
			if err != nil {
				t.Fatal(err)
			}
			if target == "docs/adr/README.md" {
				// README は既存のファイルなので、victim に置き換える (ハードリンクにする)。中身は、索引が古い状態。
				if err := os.Remove(filepath.Join(dir, filepath.FromSlash(target))); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(victim, []byte("# ADR\n\n"+adrBegin+"\n"+adrEnd+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			hardLink(t, victim, filepath.Join(dir, filepath.FromSlash(target)))
			victimBefore, before := mustRead(t, victim), readTree(t, dir)

			_, _, err = tr.writeOutputs(res)
			if err == nil || !strings.Contains(err.Error(), "ハードリンク") {
				t.Fatalf("ハードリンクの error にすべき: %v", err)
			}
			if got := mustRead(t, victim); got != victimBefore {
				t.Errorf("root の外のファイルが書き換わった: %q", got)
			}
			if after := readTree(t, dir); !reflect.DeepEqual(before, after) {
				t.Errorf("error なのに、ファイルを書き換えた・消した (before %v, after %v)", sortedKeys(before), sortedKeys(after))
			}
		})
	}
}

// TestWriteFileRejectsHardLink は、書く入口 (writeFile) 自身も、ハードリンクを書かないことを確認する
// (checkPlan を通らずに呼ばれても、切り詰めない)。
func TestWriteFileRejectsHardLink(t *testing.T) {
	victim := victimFile(t)
	tr, dir := newTestTree(t, map[string]string{"docs/reference/x.md": "old\n"})
	if err := os.Remove(filepath.Join(dir, "docs", "reference", "x.md")); err != nil {
		t.Fatal(err)
	}
	hardLink(t, victim, filepath.Join(dir, "docs", "reference", "x.md"))
	err := tr.writeFile("docs/reference/x.md", []byte("生成物\n"))
	if err == nil || !strings.Contains(err.Error(), "ハードリンク") {
		t.Fatalf("ハードリンクの error にすべき: %v", err)
	}
	if got := mustRead(t, victim); got != "VICTIM\n" {
		t.Errorf("root の外のファイルが書き換わった (切り詰めた): %q", got)
	}
}

// TestHardLinkedInputIsRead は、読み取り (入力) は、変えないことを確認する: ハードリンクの .go は、通常のファイルとして読む。
func TestHardLinkedInputIsRead(t *testing.T) {
	victim := victimFile(t)
	tr, dir := newTestTree(t, nil)
	hardLink(t, victim, filepath.Join(dir, "core", "extra.go"))
	got, err := tr.readFile("core/extra.go")
	if err != nil || string(got) != "VICTIM\n" {
		t.Errorf("readFile = %q, %v: ハードリンクの入力も、通常のファイルとして読むべき", got, err)
	}
}

// TestUnsharedFileIsWritten は、リンク数 1 の既存のファイルは、これまでどおり書き換わることを確認する (偽陽性の歯止め)。
func TestUnsharedFileIsWritten(t *testing.T) {
	tr, dir := writeRepo(t, map[string]string{"docs/reference/core.md": "古い\n"})
	res, err := tr.analyze()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := tr.writeOutputs(res); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, filepath.Join(dir, "docs", "reference", "core.md")); got != res.Expected["docs/reference/core.md"] {
		t.Errorf("生成物が、書き換わらなかった: %q", got)
	}
}

// TestCheckNotHardLinked は、リンク数の判定の境界 (1 は通し、2 は error) を確認する。
func TestCheckNotHardLinked(t *testing.T) {
	victim := victimFile(t)
	fi, err := os.Lstat(victim)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkNotHardLinked("victim.txt", fi); err != nil {
		t.Errorf("リンク数 1: %v", err)
	}
	hardLink(t, victim, victim+".link")
	fi, err = os.Lstat(victim)
	if err != nil {
		t.Fatal(err)
	}
	if n, ok := linkCount(fi); !ok || n != 2 {
		t.Fatalf("linkCount = %d, %v, want 2, true", n, ok)
	}
	if err := checkNotHardLinked("victim.txt", fi); err == nil {
		t.Error("リンク数 2 は、error にすべき")
	}
}
