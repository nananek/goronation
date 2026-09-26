package main

import (
	"bytes"
	"fmt"
	"path"
	"slices"
	"strings"
)

// sortedKeys は、map のキーを、名前の順に返す (走査・出力の順序を、map の並びに依らせない)。
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// refIndexPath は、生成物の一覧 (package の索引) の path。
const refIndexPath = referenceDir + "/README.md"

// result は、1 回の解析の結果。
type result struct {
	Inv      *inventory
	Src      *sources
	Docs     []*pkgDoc
	Expected map[string]string // 期待する生成物 (repo 相対 path → 中身): referenceDir の下と、ADR の README
	Findings []finding         // lint の結果 (生成物との照合は、compare が別に返す)
}

// analyze は、repo を読み (予算は tree が共有する)、生成物の期待する内容を作る。
// 何かを書く前に、すべてを作り終える (途中で error なら、何も書かない)。
func (t *tree) analyze() (*result, error) {
	inv, err := t.inventory()
	if err != nil {
		return nil, err
	}
	return t.analyzeInventory(inv)
}

// analyzeInventory は、analyze の、inventory を得た後の部分 (テストが、列挙の順序を変えて呼べるように分ける)。
func (t *tree) analyzeInventory(inv *inventory) (*result, error) {
	src, err := t.readSources(inv)
	if err != nil {
		return nil, err
	}
	res := &result{Inv: inv, Src: src, Expected: map[string]string{}}

	seen := map[string]string{} // 小文字にした出力の path → 元の path (大文字小文字を区別しない FS でも衝突しない)
	claim := func(p string) error {
		key := strings.ToLower(p)
		if prev, dup := seen[key]; dup {
			return fmt.Errorf("生成物の path が衝突する: %s と %s (大文字小文字を区別しないファイルシステムでも、別のファイルにする)", prev, p)
		}
		seen[key] = p
		return nil
	}
	if err := claim(refIndexPath); err != nil {
		return nil, err
	}
	for _, p := range src.Pkgs {
		if err := t.checkTime(); err != nil {
			return nil, err
		}
		d, err := buildDoc(src, p)
		if err != nil {
			return nil, err
		}
		if err := claim(d.outPath()); err != nil {
			return nil, err
		}
		body, err := newRenderer(d).render()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p.Dir, err)
		}
		res.Docs = append(res.Docs, d)
		res.Expected[d.outPath()] = body
	}
	if len(res.Docs) == 0 {
		return nil, fmt.Errorf("文書を作る package が 1 つも無い (空振りで緑にしない)")
	}
	if res.Expected[refIndexPath], err = renderRefIndex(res.Docs); err != nil {
		return nil, err
	}

	adrs, err := loadADRs(src)
	if err != nil {
		return nil, err
	}
	table, err := renderADRIndex(adrs)
	if err != nil {
		return nil, err
	}
	readme, ok := src.Markdown[adrReadme]
	if !ok {
		return nil, fmt.Errorf("%s が無い (ADR の索引の生成区間を置く場所)", adrReadme)
	}
	if res.Expected[adrReadme], err = spliceADRIndex(string(readme), table); err != nil {
		return nil, fmt.Errorf("%s: %w", adrReadme, err)
	}
	if res.Findings, err = t.lint(res); err != nil {
		return nil, err
	}
	return res, nil
}

// writeOutput は、生成物 1 つを書く唯一の入口。書く内容の全体を normalize (kindDocument) に通してから、
// tree に書く (どの部品を通ったかによらず、制御文字が、ファイルに出ない)。
func writeOutput(t *tree, name, content string) error {
	if _, err := normalize(kindDocument, content); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return t.writeFile(name, []byte(content))
}

// writeOutputs は、期待する生成物のうち、内容が違うものだけを書き、生成物の置き場の余剰なものを消す。
// 書いたファイルの数と、変更が無かったファイルの数を返す。
func (t *tree) writeOutputs(res *result) (written, unchanged int, err error) {
	ref, err := t.listReference()
	if err != nil {
		return 0, 0, err
	}
	isFile := map[string]bool{}
	for _, e := range ref {
		if !e.Dir {
			isFile[e.Path] = true
		}
	}

	paths := make([]string, 0, len(res.Expected))
	for p := range res.Expected {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	for _, p := range paths {
		want := res.Expected[p]
		var have []byte
		exists := false
		switch {
		case p == adrReadme:
			have, exists = res.Src.Markdown[p], true
		case isFile[p]:
			if have, err = t.readFile(p); err != nil {
				return written, unchanged, err
			}
			exists = true
		}
		if exists && bytes.Equal(have, []byte(want)) {
			unchanged++
			continue
		}
		if err := writeOutput(t, p, want); err != nil {
			return written, unchanged, err
		}
		written++
	}

	// 余剰の削除。子を先に消すため、逆順 (ref は、親が子より前) に辿る。
	wantDirs := map[string]bool{}
	for p := range res.Expected {
		for d := path.Dir(p); strings.HasPrefix(d, referenceDir+"/"); d = path.Dir(d) {
			wantDirs[d] = true
		}
	}
	for i := len(ref) - 1; i >= 0; i-- {
		e := ref[i]
		keep := wantDirs[e.Path]
		if !e.Dir {
			_, keep = res.Expected[e.Path]
		}
		if keep {
			continue
		}
		if err := t.remove(e.Path); err != nil {
			return written, unchanged, err
		}
	}
	return written, unchanged, nil
}
