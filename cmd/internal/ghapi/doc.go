// Package ghapi は、goronation pr ready (ホスト側) が使う、GitHub GraphQL API への最小の呼び出しを実装する。
//
// 任意の GraphQL クエリは実装しない: PR の状態 (draft・head の checks) を読む 1 つのクエリと、
// ready for review にする 1 つの mutation だけを、固定の文字列として持つ。
//
// # 使い方
//
//	c := &ghapi.Client{Token: tok}
//	alreadyReady, err := c.Ready(ctx, "owner", "repo", 123)
//
// # 規則
//
//   - fixed-queries: query 文字列は、この package の定数だけ。呼び手は、query を組み立てられない。
//   - checks-required: Ready は、head commit の StatusCheckRollup.state が SUCCESS (または、rollup が
//     無い = checks が 1 つも設定されていない) のときだけ、mutation を呼ぶ。それ以外は ErrChecksNotGreen。
//   - idempotent: PR がすでに draft でなければ、何もせず alreadyReady=true で成功にする。
//   - no-token-in-errors: error の文言に、Token の値を含めない (net/http が Authorization ヘッダに使うだけ)。
//
// # 限界
//
//   - GraphQL の応答の data は、この package が読む形にだけ decode する (未知の項目は無視。厳密な検査はしない:
//     応答は GitHub 自身のもので、檻からの敵対入力ではない)。
package ghapi
