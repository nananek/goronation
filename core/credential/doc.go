// Package credential は、資格情報 (トークンなど) の取り出し口 (Source) と、値を誤って出力・記録させない型 (Secret) の契約 (port) を定める。
//
// 資格情報は、名前 (github など) で引く。Source の実装 (ホストのファイル・将来の Vault) は、この package を import する側から見えない。
// Secret は、値を包み、fmt・JSON・テキスト・slog・panic のどの経路でも "[redacted]" を出し、値を返すのは Reveal だけにする。
// 保証しない: Reveal した後の string の扱い (呼び手の責任)・メモリからの消去。この package は標準ライブラリだけを使い、プロセスを起動しない。
//
// # 使い方
//
//	tok, err := src.Token(ctx, "github") // 無ければ errors.Is(err, credential.ErrNotFound)
//	req.Header.Set("Authorization", "Bearer "+tok.Reveal())
//
// # 規則
//
//   - redacted: Secret の String・GoString・Format (全ての動詞)・MarshalJSON・MarshalText・LogValue は、値に依らず "[redacted]" を返す。
//   - reveal-only: 値を得られるのは Reveal だけ。Secret の中身は、フィールドとして出力されても (%v・%+v・%#v)、値でなくアドレスになる。
//   - name-form: 名前は [a-z][a-z0-9-]{0,31}。path・URL の一部にしても安全な形だけで、CheckName が、全ての Source の実装に共通の検査を与える。
//   - not-found: 無い資格情報は ErrNotFound。error の文言に、値を含めない。
//
// # 限界
//
//   - Reveal した string は、Go のメモリから確実には消せない (string は不変で、GC が動かす)。core dump・swap への流出は防げない。
//   - Secret は、値を隠す型で、値を守る仕組みではない: Reveal を呼んだ側が、ログ・error に書けば、漏れる (呼び手の規律とレビューの対象)。
package credential
