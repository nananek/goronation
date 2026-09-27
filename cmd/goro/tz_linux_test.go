//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidTZValue(t *testing.T) {
	for _, tc := range []struct {
		in string
		ok bool
	}{
		{"Asia/Tokyo", true},
		{"UTC", true},
		{"Etc/GMT+9", true},
		{"America/Argentina/Buenos_Aires", true},
		{"  Asia/Tokyo\n", true}, // 前後の空白 (改行含む) は落とす
		{"", false},
		{"   ", false},
		{"/etc/passwd", false},           // 先頭が空要素 ("" と "etc" と "passwd")
		{"../../../etc/passwd", false},   // ".." を含む (path traversal)
		{"Asia/../../etc/passwd", false}, // 途中に ".."
		{"Asia/Tokyo/", false},           // 末尾が空要素
		{"Asia//Tokyo", false},           // 空要素を挟む
		{"Asia/Tokyo\x00", false},        // NUL
		{"Asia/Tokyo; rm -rf /", false},  // シェルのメタ文字
		{"$(id)", false},
		{"Asia Tokyo", false},            // 空白を含む要素
		{strings.Repeat("a", 65), false}, // 65 バイト (上限超え)
		{strings.Repeat("a", 64), true},  // 64 バイトはちょうど許す
		{"Asia/Tokyo.evil", false},       // "." を含む
	} {
		v, ok := validTZValue(tc.in)
		if ok != tc.ok {
			t.Errorf("validTZValue(%q) ok = %v, want %v (v=%q)", tc.in, ok, tc.ok, v)
			continue
		}
		if ok && v != strings.TrimSpace(tc.in) {
			t.Errorf("validTZValue(%q) = %q, want %q", tc.in, v, strings.TrimSpace(tc.in))
		}
	}
}

// realZoneinfoOrSkip は、zoneinfoDir 配下に実在する、テストに使える zone 名を 1 つ返す (無ければ skip:
// zoneinfo が無い実行環境では、tzFromEtcTimezone・tzFromLocaltime の実在性検査を確かめられないため)。
// symlink (tzdata の別名。"UTC" が "Etc/UTC" を指す環境がある) は、最後まで解決した正規名にする: そうしないと、
// tzFromLocaltime (EvalSymlinks で最後まで辿る) の返り値と、ここで選んだ名前が食い違う。
func realZoneinfoOrSkip(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"UTC", "Etc/UTC"} {
		real, err := filepath.EvalSymlinks(zoneinfoDir + name)
		if err != nil {
			continue
		}
		if fi, err := os.Stat(real); err != nil || fi.IsDir() {
			continue
		}
		if rel, ok := strings.CutPrefix(real, zoneinfoDir); ok {
			return rel
		}
	}
	t.Skip("この環境に /usr/share/zoneinfo/UTC も Etc/UTC も無い (zoneinfo が入っていない)")
	return ""
}

func TestTzFromLocaltime(t *testing.T) {
	zone := realZoneinfoOrSkip(t)
	dir := t.TempDir()

	// 正常: 実在する zoneinfo を指す symlink。
	good := filepath.Join(dir, "good")
	if err := os.Symlink(zoneinfoDir+zone, good); err != nil {
		t.Fatal(err)
	}
	if v, ok := tzFromLocaltime(good); !ok || v != zone {
		t.Errorf("tzFromLocaltime(実在する symlink) = %q, %v, want %q, true", v, ok, zone)
	}

	// symlink でない (通常のファイル)。
	plain := filepath.Join(dir, "plain")
	if err := os.WriteFile(plain, []byte(zone), 0o644); err != nil {
		t.Fatal(err)
	}
	if v, ok := tzFromLocaltime(plain); ok {
		t.Errorf("tzFromLocaltime(symlinkでない通常ファイル) = %q, true, want false", v)
	}

	// 先が無い (dangling) symlink。
	dangling := filepath.Join(dir, "dangling")
	if err := os.Symlink(filepath.Join(dir, "no-such-file"), dangling); err != nil {
		t.Fatal(err)
	}
	if v, ok := tzFromLocaltime(dangling); ok {
		t.Errorf("tzFromLocaltime(先が無いsymlink) = %q, true, want false", v)
	}

	// zoneinfoDir の外を指す symlink。
	outside := filepath.Join(dir, "outside")
	if err := os.Symlink("/etc/hostname", outside); err != nil {
		t.Fatal(err)
	}
	if v, ok := tzFromLocaltime(outside); ok {
		t.Errorf("tzFromLocaltime(zoneinfoDirの外) = %q, true, want false", v)
	}

	// 存在しない path 自体。
	if v, ok := tzFromLocaltime(filepath.Join(dir, "no-such-path")); ok {
		t.Errorf("tzFromLocaltime(存在しないpath) = %q, true, want false", v)
	}
}

func TestTzFromEtcTimezone(t *testing.T) {
	zone := realZoneinfoOrSkip(t)
	dir := t.TempDir()

	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	if v, ok := tzFromEtcTimezone(write("good", zone+"\n")); !ok || v != zone {
		t.Errorf("tzFromEtcTimezone(実在するzone) = %q, %v, want %q, true", v, ok, zone)
	}
	if v, ok := tzFromEtcTimezone(write("bogus", "Nowhere/Fake\n")); ok {
		t.Errorf("tzFromEtcTimezone(実在しないzone) = %q, true, want false (%v)", v, ok)
	}
	if v, ok := tzFromEtcTimezone(write("traversal", "../../../etc/passwd\n")); ok {
		t.Errorf("tzFromEtcTimezone(path traversal) = %q, true, want false", v)
	}
	if v, ok := tzFromEtcTimezone(write("empty", "")); ok {
		t.Errorf("tzFromEtcTimezone(空) = %q, true, want false", v)
	}
	if v, ok := tzFromEtcTimezone(write("control", "Asia/Tokyo\x00evil")); ok {
		t.Errorf("tzFromEtcTimezone(NUL混入) = %q, true, want false", v)
	}
	if v, ok := tzFromEtcTimezone(filepath.Join(dir, "no-such-file")); ok {
		t.Errorf("tzFromEtcTimezone(存在しないファイル) = %q, true, want false", v)
	}
	// zoneinfoDir 自体 (ディレクトリ) を指す値は、実在はするが IsDir なので拒否する。
	if v, ok := tzFromEtcTimezone(write("dirname", "Asia\n")); ok {
		t.Errorf("tzFromEtcTimezone(ディレクトリを指す値) = %q, true, want false", v)
	}
}

func TestTzFrom(t *testing.T) {
	zone := realZoneinfoOrSkip(t)
	dir := t.TempDir()
	localtime := filepath.Join(dir, "localtime")
	if err := os.Symlink(zoneinfoDir+zone, localtime); err != nil {
		t.Fatal(err)
	}
	etcTimezone := filepath.Join(dir, "timezone")
	if err := os.WriteFile(etcTimezone, []byte(zone+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "no-such-file")

	// (1) 環境変数 TZ が最優先 (形が正しければ、localtime・timezone を見ない)。
	if got := tzFrom("Custom/Zone", localtime, etcTimezone); got != "Custom/Zone" {
		t.Errorf("TZ環境変数が最優先されない: got %q", got)
	}
	// 環境変数の値が壊れていれば、無視して次 (localtime) に落ちる。
	if got := tzFrom("../../etc/passwd", localtime, etcTimezone); got != zone {
		t.Errorf("壊れたTZ環境変数のあとlocaltimeに落ちない: got %q, want %q", got, zone)
	}
	// (2) 環境変数が空なら localtime。
	if got := tzFrom("", localtime, etcTimezone); got != zone {
		t.Errorf("localtimeから取れない: got %q, want %q", got, zone)
	}
	// (3) localtime が無ければ /etc/timezone。
	if got := tzFrom("", missing, etcTimezone); got != zone {
		t.Errorf("/etc/timezoneから取れない: got %q, want %q", got, zone)
	}
	// 全部無ければ、空文字 (エラーにしない)。
	if got := tzFrom("", missing, missing); got != "" {
		t.Errorf("全部無いのに空文字を返さない: got %q", got)
	}
	// 全部壊れていても、空文字 (エラーにしない)。
	if got := tzFrom("bad tz!", missing, missing); got != "" {
		t.Errorf("全部壊れているのに空文字を返さない: got %q", got)
	}
}

// TestHostTZDoesNotPanic は、hostTZ (実物の /etc/localtime・/etc/timezone・環境変数 TZ を見る) が、
// この実行環境がどんな timezone 設定でも panic せず、返り値が空か validTZValue を通る形であることを確かめる。
func TestHostTZDoesNotPanic(t *testing.T) {
	if got := hostTZ(); got != "" {
		if v, ok := validTZValue(got); !ok || v != got {
			t.Errorf("hostTZ() = %q は validTZValue を通らない形", got)
		}
	}
}
