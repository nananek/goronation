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
| [0018](0018-opencode-2x-serve-stdio.md) | opencode は 2.x の serve --stdio で起動する (M2) | 採用 |
| [0019](0019-opencode-request-relay.md) | opencode サーバーへの通信は、socketpair と SCM_RIGHTS の中継で通す (M2) | 採用 |
| [0020](0020-opencode-token-and-landlock.md) | opencode のトークンは使い捨てにし、読めても繋げない 3 重の守りを置く (M2) | 採用 |
| [0021](0021-opencode-permission-ask.md) | opencode の承認は、セッション作成時の permissions で固定する (M2) | 採用 |
| [0022](0022-opencode-stream-shape.md) | opencode の Stream は行のまま、HTTP と SSE は transport 層が写す (M2) | 採用 |
| [0023](0023-chat-durable-event-log.md) | chat の耐久イベントログ: セッションごとの SQLite に、進行中の起動の durable だけを残す (M2) | 採用 |
| [0024](0024-chat-sse-resume.md) | chat の SSE 再接続: `after=seq`・`generation`・Hub の境目 (M2) | 採用 |
| [0025](0025-chat-event-log-degrade.md) | chat の耐久イベントログの失敗は、縮退して知らせる (M2) | 採用 |
| [0026](0026-sqlite-dependency-exception.md) | 耐久イベントログに限り、`modernc.org/sqlite` を使う (依存ゼロの方針の例外) | 採用 |
| [0027](0027-chat-event-log-async-writer.md) | chat の耐久イベントログの書き込みは、Hub の Mutex の外の専用 goroutine で行い、量を有界にする (M2) | 採用 |
| [0028](0028-relay-control-stdin.md) | 要求の中継の control は init の標準入力にし、HTTP の解釈と上限を固定する (M2) | 採用 |
| [0029](0029-opencode-port-squat-layers.md) | opencode のポートの横取りは、起動の証明・接続後の生存確認・1 起動 1 トークンで抑える (M2) | 採用 |
| [0030](0030-relay-init-startup-contract.md) | init は起動の結果を stdout の 1 行で知らせ、要求のヘッダは許可リストで通す (M2) | 採用 |
| [0031](0031-vault-responsibility.md) | Vault は保管と署名だけを持ち、egress は使うたびに Vault から引く (M2) | 採用 |
| [0032](0032-vault-core-ports.md) | Vault の port は core に置き、資格情報の取り出しは既存の `credential.Source` を使う (M2) | 採用 |
| [0033](0033-vault-key-hierarchy.md) | Vault の鍵は、passkey の PRF から作るラップ鍵で、本体鍵を包む (M2) | 採用 |
| [0034](0034-vault-reset.md) | `goronation vault reset` は、Vault の中身を壊すだけで、読まず、記録を残す (M2) | 採用 |
| [0035](0035-vault-unlock-sas.md) | 解錠の儀式は、操作対象と確認コードを両方の画面に出す。経路は S9 の後に決める (M2) | 提案 |
| [0036](0036-vault-import-rules.md) | `vault/` を import してよいのは `cmd/**` だけで、`vault/` は `core/` と標準ライブラリだけに依存する (M2) | 採用 |
| [0037](0037-vault-process-layout.md) | Vault の本体鍵は、専用のデーモンだけが持ち、他のプロセスは UDS で引く (M2) | 採用 |
| [0038](0038-vault-wrap-add-remove.md) | passkey の追加と削除は、既存の passkey の証明と、両方の画面での確認を要る (M2) | 採用 |
| [0039](0039-webauthn-multi-passkey.md) | webauthn は、複数の passkey と、操作に束縛した再認証を持ち、ED は許可リストで受ける (M2) | 採用 |
| [0040](0040-form-vocabulary.md) | 人間への入力の要求は、質問でなく form (フィールドの一覧) として表す (M2) | 採用 |
| [0041](0041-permission-summary-details.md) | 権限の要求は、機械生成の要約と、tool に依らない詳細の 2 階層で見せる (M2) | 採用 |
| [0042](0042-content-hash-binding.md) | 承認と回答は、利用者が見た内容の SHA-256 を写させて束縛する (M2) | 採用 |
| [0043](0043-question-permission.md) | opencode の question の tool は、session の permissions で承認の段を省く (M2) | 採用 |
| [0044](0044-claude-ask-user-question.md) | claude の AskUserQuestion は form にし、回答は updatedInput.answers で返す (M2) | 採用 |
| [0045](0045-subagent-origin.md) | サブエージェントの出力には、帰属 (origin) を付ける (M2) | 採用 |
| [0046](0046-form-resolve-api.md) | form の応答の API (POST /form) と、承認の応答の content_hash (M2) | 採用 |
| [0047](0047-content-hash-in-feed.md) | 内容ハッシュは Feed が付け、Conversation が照合する (M2) | 採用 |

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
