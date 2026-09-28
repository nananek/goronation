// Package github は、檻が GitHub の PR を作る要求 (POST /github-api/repos/<o>/<r>/pulls) を、許可した repo・head だけに絞り、上流に送る JSON を作り直す検査で、ネットワークに触れない純関数の集まり。
//
// 檻の中のエージェントは信頼できない。要求の JSON を厳格に読み、検査した値から、上流 (api.github.com) へ送る JSON を組み立てる (受けた本文は送らない)。PR は、常に draft で作る。
// 保証するのは、PR の作成先と head が許可した範囲で、項目・大きさ・文字が規則の内側で、檻に返す応答が number と html_url だけであること。
// undraft・PR の更新・マージ・issue など、PR の作成以外の操作は、この package に無く、檻から届かない。
//
// # 使い方
//
//	repo, err := github.ParsePullRoute(method, target) // PR を作る repo
//	pull, err := pp.ParsePull(repo, body)              // pp は PullPolicy。base を省いた要求は、pull.WithBase で既定の branch を入れる
//	out, err := pull.JSON()                            // 上流に送る本文 (draft は常に true)
//
// # 規則
//
//   - route: POST /github-api/repos/<owner>/<name>/pulls だけ。query・%・..・// は断る。上流の path は、検査した repo から作る。
//   - fields: title・body・head・base・draft だけ (完全一致)。重複・大文字違い・未知の項目・null・末尾の続きは断る。未知の項目は、値を読む前に断る。
//   - rebuild: 上流へは、検査した値から作り直した JSON を送る。項目の並びは固定で、受けた本文は送らない。
//   - draft: 上流に送る draft は、常に true。檻が draft: false を送っても、上書きする。
//   - head: Push の名前空間 (refs/heads/goronation/<セッション>/ の下) のブランチだけ。fork の元への PR は、owner:branch (owner は Push の owner) の形だけ。
//   - text: title は 256 バイト・body は 64 KiB・要求は 512 KiB まで。制御文字 (body の改行とタブを除く)・行区切り・書式制御文字 (Cf の全て: 双方向の制御・ゼロ幅・BOM・タグ文字)・見えない文字 (Default_Ignorable) は断る。異体字セレクタは、基底の文字に付くときだけ通す。
//   - result: 檻に返すのは number と html_url だけ。html_url は、検査した number と repo から作り直す。
//   - quota: PR の作成の試行は、Quota の回数まで。上流が断った試行も数える。
//
// # 限界
//
//   - title・body の中身 (リンク・メンション・Markdown) は見ない。見た目の紛らわしい文字 (同形異字) も、検査しない。Cf を全て断るので、絵文字の ZWJ での連結は書けない。文字の表は Go の unicode (Unicode 15.0.0) の版に依り、それより新しい見えない文字は通る。
//   - 上流が draft: false と答えた (draft が効かなかった) ときは、Result.Draft で知らせるだけで、作られた PR は取り消さない。
//   - base は、branch 名として正しいことだけを見る (どの branch でもよい)。PR は提案で、base を書き換えない。
package github
