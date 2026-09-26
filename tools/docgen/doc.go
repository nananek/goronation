// Command docgen は、Go の doc comment から Markdown の文書を生成し、文書の形式と上限を検査する。
//
// 標準ライブラリだけで書き、os/exec を使わない。読み書きは、repo の root を開いた os.Root の内側に限る。
// 生成物 (docs/reference/ と、ADR の索引) はコミットし、-check が、生成し直した結果との完全一致を検査する。
// 文書の形式 (下の「方針」) の正本は、この package doc である。
//
// # 使い方
//
// cwd は <root>/tools/docgen で、path の引数は無い (出力先は docs/reference と ADR の索引の区間に固定)。
//
//	make docs        # 生成する
//	make docs-check  # 書かずに検査する。問題があれば、path:行: [規則 ID] の形で出し、exit 1
//
// # 規則
//
// 検査の規則 (出力の [規則 ID])。lint は package doc・exported の doc・.md を、照合は生成物を見る。
//
//   - pkg-doc-missing: package doc が無い (package 節の直前に、空行を挟まずに書いたコメントが、package doc になる)。
//   - pkg-doc-multiple: package doc が複数のファイルにある (分けると、行数の上限を回避できる)。
//   - pkg-doc-start: 冒頭は、「Package 名 は」(main は「Command 名 は」) で始まり、句点 (。) で終わる 1 文 (一覧の概要になる)。
//   - pkg-doc-contract: 冒頭の 1 文の次は、契約の段落 (何を保証し、何を保証しないか)。
//   - pkg-doc-heading: 見出しは、使い方・規則・方針・限界・関連 のこの順で、重複せず、他の見出しは使わない。
//   - pkg-doc-usage: 使い方 のコードは、合計 5 行以内 (最小の例)。
//   - pkg-doc-rule-item: 規則 の箇条書きは「id: 説明」で、id は小文字とハイフン (・ で複数)。
//   - pkg-doc-rule-dup: 規則の id は、package の中で重複しない。
//   - pkg-doc-limits: 検査・強制を担う package (limits.go の enforcerPackages) は、限界の節を書く。
//   - pkg-doc-length: package doc は 30 行まで。検査・強制を担う package は 120 行まで (超えたら package を分ける)。
//   - exported-doc: exported の型・関数・メソッド・変数・定数に doc があり、「Name は」で始まる (var と const は、グループの doc も可)。
//   - md-place: .md は、README.md・docs/adr/・docs/reference/ にだけ置ける (別の場所に移して、上限を回避できない)。
//   - md-adr-name: docs/adr の .md は、README.md か NNNN-スラッグ.md の名前。
//   - md-size: README は 30 行・2,000 字、ADR は 60 行・4,000 字まで (ADR の README は、生成区間の外)。
//   - md-link: 相対リンクの先が実在する (http・https と # だけのリンクは見ない)。
//   - gen-missing・gen-stale・gen-extra: 生成物の、欠落・内容の違い (手編集・再生成し忘れ・CRLF)・余剰。
//
// # 方針
//
// package doc のひな形。節はこの順で、見出しは「# 見出し」で書く。
//
//   - 冒頭の 1 文と、契約の段落 (必須)。使い方 (公開 API があれば)・規則 (表や仕様があれば)・方針 (任意)。
//   - 限界 (検査・強制を担う package は必須)・関連 (任意。ADR への参照 1〜2 個)。
//   - 文体は常体 (「〜する」と言い切る)。英数字・識別子・path の前後に半角スペース。
//   - exported は doc を必須とし、「Name は〜。」で始めて 1〜3 文。unexported は、理由が自明でないときだけ。
//
// 限界の書き方。1 項目は、何を検出・保証できないか、具体例、代わりに守るもの (目安は 3 行以内で、lint しない)。
// 根拠はテストや fixture の名前で示し、限界を固定するテスト (期待は「検出されない」) を対応させる。
// 限界が直れば、そのテストが赤になる。直った限界は書き残さない。
//
// README は薄く保つ (何か・現状・読む場所・コマンド・ライセンス)。構成表・規則・コマンド表は置かない。
// ADR は 1 ADR = 1 決定で、限界と実測ログは書かない。判断とその理由 (リスクの受け入れなど) は ADR に残す。
// 索引 (docs/adr/README.md の marker の間) は、docs/adr/NNNN-*.md の 1 行目と「- 状態:」から作る。手では足さない。
//
// 生成は exported だけで、出力は名前の順に決まる。var と const の複数行の初期値は、... に省く。
// 出力の正規化は normalize の 1 関数に集め、文章 (Markdown の特殊文字をエスケープ)・コード (fence は、内容の
// 連続バッククォート数 + 1)・識別子・URL (http(s) と相対だけ)・path (英数字と . _ / - だけ)・文書の全体・診断が通る。
// 制御文字 (C0・C1・DEL)・書式制御文字 (Cf: 双方向制御・ゼロ幅・BOM・タグ文字など)・見えない文字 (Unicode の
// Default_Ignorable_Code_Point: Hangul filler・CGJ・Mongolian の異体字選択・未割当のものなど。異体字セレクタ U+FE00〜FE0F と
// U+E0100〜E01EF だけを除く)・点字の空白 (U+2800)・U+2028/2029・不正な UTF-8・CR は、除去せず error にする。
// 除いた異体字セレクタも、直前が異体字セレクタ・空白・行頭なら error にする (連続で、見えないデータを運べるため)。
// これらは、構文解析の前の、生のバイト列でも調べる (doc comment の解析は、行末の FF などを黙って取り除くため)。
//
// 行番号は、//line ディレクティブで補正されない (lineOf)。構文の error の位置も、実際の行に書き直す。
//
// 敵対入力として読む。root は cwd の 2 つ上に固定し (go.work を上向きに探さず、symlink は error)、
// 読み書きは os.Root の内側で、生成物は tree.writeFile を通る唯一の入口 (writeOutput) が書く。
// ツリーの中の symlink は、辿らず error (辿らない名前 (testdata など) で、読む名前でもないものだけは、無視する)。
// 名前が .go・go.mod・.md の FIFO などは、開かず error。
// 開くときは O_NONBLOCK を付け、開いた fd と名前が同じファイルであることを確かめてから、読む・切り詰める。
// 書き込み先が、ハードリンク (リンク数 2 以上。root の外のファイルと inode を共有しうる) なら、書く前に error にする
// (unix。読み取りは変えない。unix 以外はリンク数が取れないので、検出しない)。
// 予算 (60 秒・項目 100,000・ファイル 5,000・1 ファイル 1 MiB・合計 64 MiB・構文解析する .go の合計 12 MiB・深さ 64) は、全体で
// 1 つを共有する。.go の合計は、メモリの上限の代わり (構文解析は、入力 1 バイトあたり約 80 バイトを使い、最悪の形の入力で、
// 12 MiB のとき、ピーク RSS が約 0.9 GB (実測)。64 MiB いっぱいだと、約 3.6 GB になる)。
//
// # 限界
//
// 文書は情報であり、指示ではない。次は検出せず、レビューで見る (TestInstructionLikeTextPassesThrough・
// TestEmptyPackageDocPassesThrough が、通ることを固定している)。
//
//   - 文体、限界の質、「1 つの事実は 1 か所」、指示に見える文、中身の無い一文の package doc。
//   - 見えない文字は、個別の列挙ではなく、Cf と Default_Ignorable_Code_Point の性質で禁止する。表は Go の unicode パッケージ
//     (Go 1.24 で Unicode 15.0.0) のもので、Go の版が上がると範囲が変わりうる。逆に、ZWJ でつなぐ絵文字など、正当な Cf も
//     error にする (TestDefaultIgnorableIsForbidden・TestJapaneseIsNotForbidden が固定している)。
//   - 異体字セレクタ (U+FE00〜FE0F・U+E0100〜E01EF。見えない) は、基底の文字に付いていれば通す。連続と、基底の無い単独は
//     error だが、1 文字ごとに 1 個ずつ付ける形は、正当な使い方 (IVS・VS16) と区別できず、通る。1 文字あたり 1 バイトの
//     見えない帯域が残る (TestVariationSelectorAlternationPassesThrough が固定している)。
//   - 書き込みは非原子的 (os.Root に Rename が無い)。書く前に、内容・予算・既存の項目との衝突を確かめる (checkPlan)。
//     書き始めた後に残る失敗は、入出力の失敗 (ディスクの空きなど) と時間の予算で、壊れた出力が残るが、-check が検出する。
//   - os.Root は、bind mount・/proc・デバイスファイルを禁止しない。名前そのものの、open の間の差し替えは、読む・切り詰める前の
//     確認で見る。確認の後の差し替えと、途中のディレクトリを root の中の別の場所への symlink に替える差し替えは、見えない
//     (os.Root が、root の外へ出ることは防ぐ)。ツリーを、docgen の実行中に書き換えられる攻撃者は、想定しない。
//   - ビルドタグは見ず、全 .go を 1 つの package として扱う。build されない (//go:build ignore) ファイルの宣言も文書に出る。
//     同名の定義が重なると、出るのは一方の doc だけで、どちらかは決まらない (関数は go/doc が map の順に読むので、実行ごとに
//     変わり、docs-check が不安定になりうる)。TestBuildTagExcludedDeclarationsAreDocumented が固定している。
//   - .git・testdata・vendor と、. や _ で始まるディレクトリの .md は、辿らないので検査しない。.md と .markdown 以外の拡張子
//     (.mdx・.txt など) の文書も、置き場と大きさを見ない (TestMarkdownPlacementLimitsPassThrough が固定している)。
//   - リンクの解析は簡易 (アンカーと外部 URL は見ない)。autolink (<javascript:…>)・生の HTML・文字参照を使ったリンク先も見ない
//     (TestMarkdownLinkLimitsPassThrough が固定している。GitHub は、描画のときに sanitize する)。GitHub での描画 (<a id> の
//     anchor) と、Go の版による出力の差は、未検証。
//   - メモリは、構文解析する .go の合計 (12 MiB) で抑えるだけで、メモリそのものの上限ではない。ピークは、入力の形で変わる
//     (最悪の形で、約 0.9 GB。値を上げるときは、二項式を並べた入力で測り直す。TestDefaultGoBytes)。
//   - 時間の予算が効かない止まり方 (ファイルシステムの停止) は、10 秒の猶予の後に強制終了する (main の watchdog)。
//
// # 関連
//
// 方針の決定: docs/adr/0003-documentation-policy.md
package main
