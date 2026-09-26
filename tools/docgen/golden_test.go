package main

import (
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// update は、golden (testdata/golden/<case>/want) を、いまの出力で書き換える。
// 書き換えた後は、差分を目で読んで、意図した変更だけであることを確かめる。
var update = flag.Bool("update", false, "golden を更新する")

// copyTree は、src の下の通常のファイルを、dst の下にコピーする。
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		return os.WriteFile(out, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// readTree は、dir の下のすべてのファイルを、"/" 区切りの相対 path → 中身の map にする。dir が無ければ空。
func readTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// firstDiff は、a と b が最初に違う行 (1 始まり) と、その 2 行を返す。
func firstDiff(a, b string) (int, string, string) {
	la, lb := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := 0; i < max(len(la), len(lb)); i++ {
		var x, y string
		if i < len(la) {
			x = la[i]
		}
		if i < len(lb) {
			y = lb[i]
		}
		if x != y {
			return i + 1, x, y
		}
	}
	return 0, "", ""
}

// TestGolden は、testdata/golden/<case>/repo の生成結果が、<case>/want と完全一致することを確認する。
// 生成物の path の集合と、中身 (1 バイトも違わない) の両方を見る。生成した後の repo (ファイルへ書いた結果) も、
// want と同じで、もう一度生成しても何も書かない (冪等) ことも見る。
func TestGolden(t *testing.T) {
	cases, err := filepath.Glob(filepath.Join("testdata", "golden", "*"))
	if err != nil || len(cases) == 0 {
		t.Fatalf("golden の case が無い (空振りで緑にしない): %v", err)
	}
	for _, c := range cases {
		t.Run(filepath.Base(c), func(t *testing.T) {
			repo := t.TempDir()
			copyTree(t, filepath.Join(c, "repo"), repo)
			tr := newTree(openTestRoot(t, repo), defaultLimits)
			res, err := tr.analyze()
			if err != nil {
				t.Fatal(err)
			}

			want := filepath.Join(c, "want")
			if *update {
				if err := os.RemoveAll(want); err != nil {
					t.Fatal(err)
				}
				for p, content := range res.Expected {
					out := filepath.Join(want, filepath.FromSlash(p))
					if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(out, []byte(content), 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}
			wantFiles := readTree(t, want)
			if len(wantFiles) == 0 {
				t.Fatalf("%s に golden が無い (go test -update で作る)", want)
			}

			if got, w := sortedKeys(res.Expected), sortedKeys(wantFiles); !reflect.DeepEqual(got, w) {
				t.Fatalf("生成物の path の集合が違う:\n  got  %v\n  want %v", got, w)
			}
			for p, w := range wantFiles {
				if got := res.Expected[p]; got != w {
					n, x, y := firstDiff(got, w)
					t.Errorf("%s: %d 行目から違う\n  got : %s\n  want: %s", p, n, x, y)
				}
			}

			// 書いた結果 (生成物の置き場と、ADR の README) も、want と同じ。
			written, unchanged, err := tr.writeOutputs(res)
			if err != nil {
				t.Fatal(err)
			}
			if written == 0 || written+unchanged != len(wantFiles) {
				t.Errorf("written = %d, unchanged = %d, 生成物 %d 個", written, unchanged, len(wantFiles))
			}
			after := readTree(t, repo)
			for p, w := range wantFiles {
				if after[p] != w {
					t.Errorf("書いた後の %s が、golden と違う", p)
				}
			}
			// 生成物の置き場に、want に無いものが無い。
			for p := range after {
				if strings.HasPrefix(p, referenceDir+"/") {
					if _, ok := wantFiles[p]; !ok {
						t.Errorf("生成物の置き場に、余分なファイル %s がある", p)
					}
				}
			}

			// 冪等: 2 回目は、何も書かない。
			tr2 := newTree(openTestRoot(t, repo), defaultLimits)
			res2, err := tr2.analyze()
			if err != nil {
				t.Fatal(err)
			}
			w2, u2, err := tr2.writeOutputs(res2)
			if err != nil || w2 != 0 || u2 != len(wantFiles) {
				t.Errorf("2 回目: written = %d, unchanged = %d, err = %v (何も書かないはず)", w2, u2, err)
			}
			if !reflect.DeepEqual(after, readTree(t, repo)) {
				t.Error("2 回目の生成で、repo が変わった")
			}

			// 出力のどこにも、禁止する文字が無い。
			for p, content := range res.Expected {
				if err := checkRunes(content); err != nil {
					t.Errorf("%s: %v", p, err)
				}
				if strings.ContainsRune(content, 0x1b) {
					t.Errorf("%s に ESC がある", p)
				}
			}
		})
	}
}
