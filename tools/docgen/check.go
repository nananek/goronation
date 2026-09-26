package main

import (
	"bytes"
	"fmt"
	"strings"
)

// firstDiffLine は、a と b が最初に違う行 (1 始まり) を返す。同じなら 0。
func firstDiffLine(a, b string) int {
	la, lb := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := 0; i < max(len(la), len(lb)); i++ {
		if i >= len(la) || i >= len(lb) || la[i] != lb[i] {
			return i + 1
		}
	}
	return 0
}

// compare は、期待する生成物と、実際のファイルを、書かずに比べる。
//   - 欠落: 期待するのに、無い。
//   - 内容: 期待と 1 バイトでも違う (手で編集した・doc comment を変えて再生成していない・CRLF に変換した)。
//   - 余剰: 生成物の置き場に、期待しない項目がある。
//
// ADR の README は、生成区間だけを期待の内容にして、全体を比べる (区間の中の手編集と、古さを検出する。
// 区間の外は、期待が現状のままなので、比べても一致する)。
func (t *tree) compare(res *result) ([]finding, error) {
	ref, err := t.listReference()
	if err != nil {
		return nil, err
	}
	isFile := map[string]bool{}
	isDir := map[string]bool{}
	for _, e := range ref {
		if e.Dir {
			isDir[e.Path] = true
		} else {
			isFile[e.Path] = true
		}
	}

	var out []finding
	for _, p := range sortedKeys(res.Expected) {
		want := res.Expected[p]
		var have []byte
		switch {
		case p == adrReadme:
			have = res.Src.Markdown[p]
		case isFile[p]:
			if have, err = t.readFile(p); err != nil {
				return nil, err
			}
		default:
			out = append(out, finding{Path: p, Rule: ruleGenMissing, Msg: "生成物が無い (make docs で生成する)"})
			continue
		}
		if !bytes.Equal(have, []byte(want)) {
			what := "生成物が古いか、手で編集されている"
			if p == adrReadme {
				what = "索引の生成区間が古いか、手で編集されている"
			}
			out = append(out, finding{Path: p, Line: firstDiffLine(string(have), want), Rule: ruleGenStale,
				Msg: fmt.Sprintf("%s (期待する内容と、この行から違う。make docs で再生成する)", what)})
		}
	}

	wantDirs := map[string]bool{}
	for p := range res.Expected {
		for d := parentDir(p); strings.HasPrefix(d, referenceDir+"/"); d = parentDir(d) {
			wantDirs[d] = true
		}
	}
	for _, e := range ref {
		switch {
		case e.Dir && !wantDirs[e.Path]:
			out = append(out, finding{Path: e.Path, Rule: ruleGenExtra, Msg: "生成物の置き場に、生成物ではないディレクトリがある (make docs で消える)"})
		case !e.Dir:
			if _, ok := res.Expected[e.Path]; !ok {
				out = append(out, finding{Path: e.Path, Rule: ruleGenExtra, Msg: "生成物の置き場に、生成物ではないファイルがある (make docs で消える。手で書いた文書は、docs/reference に置けない)"})
			}
		}
	}
	return out, nil
}

// parentDir は、"/" 区切りの path の、親のディレクトリ。
func parentDir(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i]
	}
	return "."
}
