//go:build linux

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// zoneinfoDir は、timezone データベースの置き場。ホストにあるものが、そのまま檻にも見える
// (cage.go の "/usr" bind が、/usr/share/zoneinfo ごと ro で運ぶ。新しい bind は要らない)。
const zoneinfoDir = "/usr/share/zoneinfo/"

// tzSegmentRE は、TZ を "/" で割った 1 要素に許す文字 (英数字・_・+・-)。IANA の zone 名の要素は、実際には
// これだけで書かれる (例: "Asia/Tokyo"・"Etc/GMT+9"・"America/Indiana/Indianapolis")。"." を許さないのは、
// zoneinfoDir と結合して存在を確かめる (tzFromEtcTimezone) ときに、".." で外に出る (path traversal) 余地を、
// 文字の集合そのもので断つため (実在の zone 名は、どのみち "." を使わない)。
var tzSegmentRE = regexp.MustCompile(`^[A-Za-z0-9_+-]+$`)

// validTZValue は、raw (前後の空白を落としたもの) が、TZ として檻に渡してよい形かを確かめる。
// 空・64 バイト超・"/" で割った要素のどれかが tzSegmentRE に合わない、のいずれかなら ok=false
// (壊れた値は、渡さずに諦める。呼び手はエラーにしない)。
func validTZValue(raw string) (v string, ok bool) {
	v = strings.TrimSpace(raw)
	if v == "" || len(v) > 64 {
		return "", false
	}
	for _, seg := range strings.Split(v, "/") {
		if seg == "" || !tzSegmentRE.MatchString(seg) {
			return "", false
		}
	}
	return v, true
}

// tzFromLocaltime は、localtimePath (通常 /etc/localtime) が指す実体 (symlink を辿った先) が zoneinfoDir の
// 下なら、その相対 path ("Asia/Tokyo" など) を返す。symlink でない・その下を指さない・先が無い・形が壊れている、
// のいずれでも ok=false (EvalSymlinks が、先が無い symlink もここで弾く)。path を引数にするのは、テストが
// ホストの実物の /etc/localtime を書き換えずに確かめるため。
func tzFromLocaltime(localtimePath string) (string, bool) {
	real, err := filepath.EvalSymlinks(localtimePath)
	if err != nil {
		return "", false
	}
	rel, ok := strings.CutPrefix(real, zoneinfoDir)
	if !ok {
		return "", false
	}
	return validTZValue(rel)
}

// tzFromEtcTimezone は、etcTimezonePath (通常 /etc/timezone。Debian 系、1 行に zone 名だけ) の中身を読み、
// 形を確かめたうえで、zoneinfoDir の下に実在するファイルかも確かめて返す (中身は、host が書いたものでも、
// 丸ごとは信用しない)。path を引数にする理由は tzFromLocaltime と同じ。
func tzFromEtcTimezone(etcTimezonePath string) (string, bool) {
	b, err := os.ReadFile(etcTimezonePath)
	if err != nil {
		return "", false
	}
	v, ok := validTZValue(string(b))
	if !ok {
		return "", false
	}
	if fi, err := os.Stat(zoneinfoDir + v); err != nil || fi.IsDir() {
		return "", false
	}
	return v, true
}

// hostTZ は、ホストのタイムゾーンを、檻の TZ 環境変数に渡してよい値にする。
func hostTZ() string {
	return tzFrom(os.Getenv("TZ"), "/etc/localtime", "/etc/timezone")
}

// tzFrom は hostTZ の本体。優先順は (1) 環境変数 TZ の値 envTZ、(2) localtimePath の symlink 先、
// (3) etcTimezonePath (Debian 系) の中身。どれも無い・形が壊れている・(見つかったはずの) zoneinfo が
// 実在しない、のいずれであっても、空文字を返すだけで諦める: 呼び手 (cageEnv) は、空文字なら TZ を檻に
// 渡さない。渡さなければ、これまでどおり UTC のまま動く (ホストの timezone 設定が壊れていても、
// goro run 自体は止まらない)。localtimePath・etcTimezonePath を引数にするのは、hostTZ から実物の path を
// 渡す一方、テストは差し替えた path で、この関数を直に確かめられるようにするため。
func tzFrom(envTZ, localtimePath, etcTimezonePath string) string {
	if v, ok := validTZValue(envTZ); ok {
		return v
	}
	if v, ok := tzFromLocaltime(localtimePath); ok {
		return v
	}
	if v, ok := tzFromEtcTimezone(etcTimezonePath); ok {
		return v
	}
	return ""
}
