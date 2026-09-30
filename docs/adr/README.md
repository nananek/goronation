# ADR (Architecture Decision Records)

設計判断と spike の結果を、あとから根拠ごと辿れるように残す場所です。ドキュメントは日本語のみで書きます。

## 索引

| 番号 | タイトル | 状態 |
|---|---|---|
| [0000](0000-template.md) | テンプレート | - |
| [0001](0001-repository-layout.md) | リポジトリ構成 (multi-module workspace と import 制約テスト) | 採用 |
| [0002](0002-branch-workflow.md) | ブランチ運用 (develop を base とし、main は保守者が反映する) | 採用 |
| [0004](0004-webauthn-dependency-exception.md) | WebAuthn の署名検証・CBOR デコードに限り、依存ゼロの方針の例外を認める | 採用 |
| [0005](0005-websocket-dependency-exception.md) | 端末ビューの WebSocket 終端に限り、依存ゼロの方針の例外を認める | 採用 |
| [0006](0006-goro-to-goronation-rename.md) | `goro` バイナリ名を `goronation` に統一する | 採用 |
| [0007](0007-xterm-csp-nonce-fork.md) | xterm.js を CSP nonce 対応のためフォークし、upstream の新版を nvchecker で追跡する | 採用 |
| [0008](0008-spec-v0-envelope.md) | 標準形式 v0: エージェントのフレームを、goronation の封筒に写す | 採用 |
| [0009](0009-chat-delivery.md) | 構造化チャットの配信方式 (M1.5): SSE・メモリのリングバッファ・serve --chat | 採用 |
| [0010](0010-claude-interactive-permissions.md) | claude の対話的な権限要求を、標準形式の permission.requested・permission.resolve に写す | 採用 |
| [0011](0011-chat-conversation-state.md) | 会話の状態機械と、未決の権限要求の保持 (M1.5) | 採用 |
| [0012](0012-chat-launch-and-stdio-isolation.md) | chat の起動: 標準入出力の隔離・洪水・世代 (M1.5) | 採用 |
| [0013](0013-chat-serve-api.md) | serve --chat の UDS API と、承認の世代 (M1.5) | 採用 |
| [0014](0014-chat-web-relay.md) | web の chat の中継: SSE・書き込み・関門・接続の上限 (M1.5) | 採用 |
| [0015](0015-chat-ui-display.md) | チャット画面の表示 (読む側): textContent だけ・危険な文字の印・再接続・項目の上限 (M1.5) | 採用 |
| [0016](0016-chat-ui-write.md) | チャット画面の書く側: 送信・終了・権限の承認 (M1.5) | 採用 |
| [0017](0017-chat-start-and-permission-mode.md) | チャットの開始の入口・E2E・権限モード (M1.5) | 採用 |
| [0018](0018-opencode-serve-adapter.md) | opencode アダプタ (serve 方式) の設計: 認証・中継・Landlock・Stream の形 (M2) | 採用 |

## 書き方

1. [`0000-template.md`](0000-template.md) を `NNNN-<短い英語のスラッグ>.md` としてコピーする。番号は索引の最大値 + 1 で、欠番を作らない。
2. 状態・状況・決定・帰結・代替案を埋める。**却下した代替案とその理由**を必ず書く。
3. spike の ADR は、テンプレートの「spike の結果」の節も埋める。Issue #1 の「進め方」のとおり、**再現手順・fixtures の場所・go / no-go・成立しない場合の代替**を残して終わる。
4. この README の索引に 1 行足す (同じ PR で行う)。
5. 決定を変えるときは、既存の ADR を書き換えず、新しい ADR を書いて、古い ADR の状態を「置き換えられた (→ NNNN)」に変える。決定に例外や手順を足すだけの追記は、同じ ADR に書き、日付の行に「(YYYY-MM-DD に追記)」と添えてよい。

## 状態

| 状態 | 意味 |
|---|---|
| 提案 | 議論中 |
| 採用 | 決定済みで、実装がこれに従う |
| 却下 | 採用しないと決めた (理由を残すため ADR は消さない) |
| 置き換えられた (→ NNNN) | 別の ADR に取って代わられた |

## 関連

- リポジトリの概要: [`../../README.md`](../../README.md)
- ロードマップと不変条件・設計ルール: [Issue #1](https://github.com/nananek/goronation/issues/1)
