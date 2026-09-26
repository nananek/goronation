package main

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
)

const (
	adrDir    = "docs/adr"
	adrReadme = adrDir + "/README.md"
	adrBegin  = "<!-- docgen:adr-index begin -->"
	adrEnd    = "<!-- docgen:adr-index end -->"
)

var (
	// adrFile は、ADR のファイル名 (NNNN-<英小文字と数字とハイフンのスラッグ>.md)。
	adrFile = regexp.MustCompile(`^([0-9]{4})-[a-z0-9]+(-[a-z0-9]+)*\.md$`)
	// adrTitle は、ADR の 1 行目 (# NNNN. タイトル)。
	adrTitle = regexp.MustCompile(`^# ([0-9]{4})\. (.+)$`)
)

// adr は、索引の 1 行になる ADR。
type adr struct {
	Num, File, Title, Status string
}

// loadADRs は、docs/adr/NNNN-*.md から、索引の行を作る。0000 はテンプレートで、中身を読まない。
// 読めない ADR (1 行目が「# NNNN. タイトル」でない・番号がファイル名と違う・「- 状態:」が無い) と、
// 番号の重複は、error にする。ADR の名前ではない .md (README.md など) は、ここでは無視する (検査は lint)。
func loadADRs(s *sources) ([]adr, error) {
	paths := make([]string, 0, len(s.Markdown))
	for p := range s.Markdown {
		if path.Dir(p) == adrDir && adrFile.MatchString(path.Base(p)) {
			paths = append(paths, p)
		}
	}
	slices.Sort(paths)
	var out []adr
	seen := map[string]string{}
	for _, p := range paths {
		name := path.Base(p)
		num := name[:4]
		if prev, dup := seen[num]; dup {
			return nil, fmt.Errorf("ADR の番号 %s が重複している (%s と %s)", num, prev, name)
		}
		seen[num] = name
		if num == "0000" {
			out = append(out, adr{Num: num, File: name, Title: "テンプレート", Status: "-"})
			continue
		}
		title, status, err := parseADR(num, string(s.Markdown[p]))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		out = append(out, adr{Num: num, File: name, Title: title, Status: status})
	}
	return out, nil
}

// parseADR は、ADR の本文から、タイトルと状態を取り出す。
func parseADR(num, text string) (title, status string, err error) {
	lines := strings.Split(text, "\n")
	m := adrTitle.FindStringSubmatch(lines[0])
	if m == nil {
		return "", "", fmt.Errorf("1 行目が「# NNNN. タイトル」の形ではない")
	}
	if m[1] != num {
		return "", "", fmt.Errorf("1 行目の番号 %s が、ファイル名の番号 %s と違う", m[1], num)
	}
	title = strings.TrimSpace(m[2])
	for _, ln := range lines[1:] {
		if strings.HasPrefix(ln, "## ") { // 状態は、最初の節の前のメタ情報に書く
			break
		}
		if rest, ok := strings.CutPrefix(ln, "- 状態:"); ok {
			if status = strings.TrimSpace(rest); status == "" {
				return "", "", fmt.Errorf("「- 状態:」が空")
			}
			return title, status, nil
		}
	}
	return "", "", fmt.Errorf("最初の節の前に「- 状態:」の行が無い")
}

// renderADRIndex は、索引の表 (生成区間の中身。改行で終わる) を作る。
func renderADRIndex(adrs []adr) (string, error) {
	var b strings.Builder
	b.WriteString("| 番号 | タイトル | 状態 |\n|---|---|---|\n")
	for _, a := range adrs {
		for _, part := range []string{a.Num, a.File} {
			if err := checkPath(part); err != nil {
				return "", err
			}
		}
		u, err := normalize(kindURL, a.File)
		if err != nil {
			return "", err
		}
		title, err := normalize(kindCell, a.Title)
		if err != nil {
			return "", fmt.Errorf("%s の題: %w", a.File, err)
		}
		status, err := normalize(kindCell, a.Status)
		if err != nil {
			return "", fmt.Errorf("%s の状態: %w", a.File, err)
		}
		b.WriteString("| [" + a.Num + "](" + u + ") | " + strings.TrimSpace(title) + " | " + strings.TrimSpace(status) + " |\n")
	}
	return b.String(), nil
}

// spliceADRIndex は、README の、生成区間 (adrBegin と adrEnd の行の間) だけを table に置き換えた全体を返す。
// 区間の外は、1 バイトも変えない。marker が無い・重複している・順序が逆なら、error にする。
func spliceADRIndex(readme, table string) (string, error) {
	lines := strings.SplitAfter(readme, "\n")
	begin, end, nBegin, nEnd := -1, -1, 0, 0
	for i, ln := range lines {
		switch strings.TrimSuffix(ln, "\n") {
		case adrBegin:
			begin = i
			nBegin++
		case adrEnd:
			end = i
			nEnd++
		}
	}
	switch {
	case nBegin != 1 || nEnd != 1:
		return "", fmt.Errorf("索引の生成区間の marker (%s と %s の行) が、1 つずつ無い (begin %d 個・end %d 個)。索引の表を、この 2 行で囲む", adrBegin, adrEnd, nBegin, nEnd)
	case begin >= end:
		return "", fmt.Errorf("索引の生成区間の marker の順序が逆 (end が begin より前)")
	}
	return strings.Join(lines[:begin+1], "") + table + strings.Join(lines[end:], ""), nil
}
