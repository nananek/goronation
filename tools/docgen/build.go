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
	if err := checkOutputTree(res.Expected); err != nil {
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

// checkOutputTree は、期待する生成物の path が、ファイルとディレクトリで衝突しない (ある生成物が、別の生成物の
// 親のディレクトリにならない) ことを確かめる。例えば、ディレクトリ core.md/sub の package は、core.md を
// ディレクトリにするが、ディレクトリ core の package は、core.md をファイルにする。書く途中でしか分からないと、
// 一部だけが書かれた出力が残る。大文字小文字は、claim と同じく、区別しない。
func checkOutputTree(expected map[string]string) error {
	lower := map[string]string{}
	for p := range expected {
		lower[strings.ToLower(p)] = p
	}
	for _, p := range sortedKeys(expected) {
		for d := path.Dir(strings.ToLower(p)); strings.HasPrefix(d, referenceDir+"/"); d = path.Dir(d) {
			if file, ok := lower[d]; ok {
				return fmt.Errorf("生成物の path が衝突する: %s はファイルだが、%s の親のディレクトリでもある (大文字小文字を区別しないファイルシステムでも、別の名前にする)", file, p)
			}
		}
	}
	return nil
}

// writeOutput は、生成物 1 つを書く唯一の入口。書く内容の全体を normalize (kindDocument) に通してから、
// tree に書く (どの部品を通ったかによらず、制御文字が、ファイルに出ない)。
func writeOutput(t *tree, name, content string) error {
	if _, err := normalize(kindDocument, content); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return t.writeFile(name, []byte(content))
}

// extraEntries は、生成物の置き場 (ref) の項目のうち、期待する生成物にも、その親のディレクトリにも当たらないもの
// (make docs が消し、-check が余剰と報告するもの) を、ref の順 (親が子より前) に返す。
func extraEntries(ref []refEntry, expected map[string]string) []refEntry {
	wantDirs := map[string]bool{}
	for p := range expected {
		for d := path.Dir(p); strings.HasPrefix(d, referenceDir+"/"); d = path.Dir(d) {
			wantDirs[d] = true
		}
	}
	var out []refEntry
	for _, e := range ref {
		if e.Dir && wantDirs[e.Path] {
			continue
		}
		if _, ok := expected[e.Path]; !e.Dir && ok {
			continue
		}
		out = append(out, e)
	}
	return out
}

// checkPlan は、書く予定 (writes) と消す予定 (removes) のすべてを、何かを書く前に確かめる。書き始めた後に、
// 予算や、既存の項目との衝突で止まると、一部だけが書かれた出力が残るため。確かめるのは、書く内容の検査、
// 既存の項目との衝突 (書く path がディレクトリ・その親が通常のファイル・書き込み先がハードリンク)、予算 (書く・消すときと同じ勘定を、
// tree のコピーに足して試す)。isFile と isDir は、生成物の置き場の、既存の項目。
func (t *tree) checkPlan(expected map[string]string, writes []string, removes []refEntry, isFile, isDir map[string]bool) error {
	c := *t
	for _, p := range writes {
		if _, err := normalize(kindDocument, expected[p]); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		if isDir[p] {
			return fmt.Errorf("%s: 同じ名前のディレクトリがある (生成物は通常のファイル。消してから、再生成する)", p)
		}
		for d := path.Dir(p); strings.HasPrefix(d, referenceDir+"/"); d = path.Dir(d) {
			if isFile[d] {
				return fmt.Errorf("%s: 親の %s が通常のファイル (ディレクトリにする。消してから、再生成する)", p, d)
			}
		}
		if err := t.checkUnshared(p); err != nil { // 既存のファイルが、ハードリンクなら、書く前に止める
			return err
		}
		if err := c.tick(); err != nil {
			return err
		}
		if err := c.chargeFile(p, int64(len(expected[p]))); err != nil {
			return err
		}
	}
	for range removes {
		if err := c.tick(); err != nil {
			return err
		}
	}
	return nil
}

// writeOutputs は、期待する生成物のうち、内容が違うものだけを書き、生成物の置き場の余剰なものを消す。
// 書いたファイルの数と、変更が無かったファイルの数を返す。
// 何かを書く前に、読む (比べる) ことと、checkPlan を済ませる。書き始めた後に残る失敗は、入出力の失敗
// (ディスクの空きなど) と、時間の予算だけで、そのときは壊れた出力が残るが、-check が検出する。
func (t *tree) writeOutputs(res *result) (written, unchanged int, err error) {
	ref, err := t.listReference()
	if err != nil {
		return 0, 0, err
	}
	isFile, isDir := map[string]bool{}, map[string]bool{}
	for _, e := range ref {
		if e.Dir {
			isDir[e.Path] = true
		} else {
			isFile[e.Path] = true
		}
	}

	var writes []string
	for _, p := range sortedKeys(res.Expected) {
		var have []byte
		exists := false
		switch {
		case p == adrReadme:
			have, exists = res.Src.Markdown[p], true
		case isFile[p]:
			if have, err = t.readFile(p); err != nil {
				return 0, 0, err
			}
			exists = true
		}
		if exists && bytes.Equal(have, []byte(res.Expected[p])) {
			unchanged++
			continue
		}
		writes = append(writes, p)
	}
	// 余剰の削除は、子を先に消すため、逆順 (ref は、親が子より前) に辿る。
	removes := extraEntries(ref, res.Expected)
	slices.Reverse(removes)
	if err := t.checkPlan(res.Expected, writes, removes, isFile, isDir); err != nil {
		return 0, 0, err
	}

	for _, p := range writes {
		if err := writeOutput(t, p, res.Expected[p]); err != nil {
			return written, unchanged, err
		}
		written++
	}
	for _, e := range removes {
		if err := t.remove(e.Path); err != nil {
			return written, unchanged, err
		}
	}
	return written, unchanged, nil
}
