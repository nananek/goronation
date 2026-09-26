package main

import (
	"strings"
	"testing"
)

// 攻撃者視点レビュー (2d63f6d) の finding 6 (限界): ビルドタグを見ないので、build されないファイルの宣言が、文書になる。
//
// 実測: //go:build ignore のファイル (build されない) の exported な宣言が、実在する API として文書に出る。
// 同名の定義が重なると、出るのは一方の doc だけで、どちらかは決まらない: go/doc は、関数を map の順に読む
// (reader.readPackage の最後のループ) ので、実行ごとに変わる (実測: 200 回中、名前順で先の doc が 160 回、後の doc が 40 回)。
// 同名の型は、名前順で後のファイルの doc が出た (200 回中 200 回) が、go/doc の実装の詳細なので、固定しない。
// レビューは「名前順で先のファイルが勝つ」としたが、関数については不正確で、それを固定するテストは、不安定 (60 回中 6 回、赤) だった。
// 差分に新しいファイルが見えるので、レビューで見られるが、生成物は「契約の正」なので、限界を実測で固定する。
// このテストは、この限界を固定する (期待は「build されない宣言が出る・同名は一方だけが出る (どちらかは問わない)」)。
// 直したら (build されないファイルを除く・同名を error にするなど) 赤になるので、期待を反転し、doc.go の「限界」も直す。
func TestBuildTagExcludedDeclarationsAreDocumented(t *testing.T) {
	files := map[string]string{
		"core/doc.go": goodDoc("core") +
			"\n// Verify は、署名を必ず検証して、失敗なら error を返す。\nfunc Verify() error { return nil }\n",
		// aaa_ は、名前の順で doc.go より先。build されない (ignore)。
		"core/aaa_fake.go": "//go:build ignore\n\npackage core\n\n" +
			"// Verify は、署名の検証を省略して、常に成功を返す。\nfunc Verify() error { return nil }\n\n" +
			"// BackdoorOff は、実在しない API。\nfunc BackdoorOff() {}\n",
	}
	// どちらの Verify が出るかは、実行ごとに変わりうるので、何回か作って、どの回も「一方だけ」であることを確かめる。
	for range 20 {
		res, err := analyzeFiles(t, files)
		if err != nil {
			t.Fatalf("analyze が error (限界が直った。期待を反転する): %v", err)
		}
		page := res.Expected["docs/reference/core.md"]
		if !strings.Contains(page, "BackdoorOff") {
			t.Fatalf("build されない BackdoorOff が、文書に出なかった (限界が直った。期待を反転し、doc.go の「限界」も直す)")
		}
		fake, real := strings.Contains(page, "省略して、常に成功を返す"), strings.Contains(page, "署名を必ず検証して")
		if fake == real {
			t.Fatalf("同名の Verify は、一方の doc だけが出るはず (fake = %v, real = %v。限界が直った)", fake, real)
		}
	}
}
