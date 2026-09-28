# 0005. 端末ビューの WebSocket 終端に限り、依存ゼロの方針の例外を認める

- 状態: 採用
- 日付: 2026-09-28
- 関連: [Issue #1](https://github.com/nananek/goronation/issues/1) (端末ビュー・WebSocket)、[ADR 0004](0004-webauthn-dependency-exception.md) (同じ枠組みの先例)、`cmd/internal/termrelay` (この決定の実装)

## 状況

- Issue #1 の標準形式節は、「Web は WebSocket で xterm.js につなぐ」と明示している (この PR での再検討の対象外)。
- WebSocket サーバー側の実装は、(1) ハンドシェイク (`Sec-WebSocket-Accept` の計算。`crypto/sha1`+`encoding/base64` だけで書ける小さい範囲)と、(2) フレーミング (マスク処理・断片化・制御フレーム・長さのエンコーディング) の 2 段からなる。(2) は、RFC 6455 準拠を素朴に書くと、認証済み利用者に端末の完全な対話的操作を渡す、この PR で最も重く見るべき経路に、新しい自前パーサーを持ち込むことになる。
- 自前実装 (マスク処理の対応だけ・非断片化限定・拡張なし、に絞る案) と、信頼できる既存ライブラリを使う案のどちらにするかは、依存ゼロ方針と新しい攻撃面という 2 つの軸に関わる判断のため、ユーザーに確認した。**ユーザーの指示: 信頼できる既存ライブラリを使う (依存を増やす)。**

## 決定

**端末ビューの WebSocket 終端 (ハンドシェイク・フレーミング) に限り**、信頼できる既存の Go ライブラリ (`github.com/coder/websocket`) を使ってよい、という、依存ゼロの方針の例外を認める (ADR 0004 と同じ枠組み)。

- 例外の範囲は、WebSocket のメッセージ送受信そのものだけ。pty master の生成・bwrap への受け渡し (`cageConfig.PTY`・`cageSpec`)・セッション管理・resize の意味づけなどは、これまでどおり標準ライブラリだけで、`cmd/goronation` に書く。
- 対象は、既存の `cmd` module の下の新しいパッケージ `cmd/internal/termrelay` にする (ADR 0004 の `cmd/internal/webauthn` と同じ理由: `cmd/goronation` は単一バイナリで静的にリンクするので、module を分けても `cmd` の `go.sum` が依存の checksum を持つことは避けられない。守れるのは「他の module が依存ゼロのままであること」で、`cmd` の中に閉じれば足りる)。
- `tools/archtest` に `websocket-only-dep` 規則を追加した。`github.com/coder/websocket` を import してよいのは `cmd/internal/termrelay/**` だけ。

### ライブラリの選定 (確定)

2026-09-28 時点で調査した候補 (`go get`/`go mod` で依存の footprint を実測):

| 候補 | 直接依存 | ライセンス | 特徴 |
|---|---|---|---|
| A. `github.com/coder/websocket` (旧 `nhooyr.io/websocket`) | 0 (`go.sum` に本体だけ) | ISC | 最小限の API (`Read`/`Write` が `context.Context` を取る)。Autobahn Test Suite に準拠。Coder 社が自社製品で使い、継続的に保守。 |
| B. `github.com/gorilla/websocket` | `go.mod` に `golang.org/x/net` があるが実際の import は無く (`go get` で `go.sum` に載らないことを実測)、実質 0 | BSD-2 | 広く使われ実績が厚いが、API が古い形式 (`Upgrader.Upgrade` → `*Conn` の `ReadMessage`/`WriteMessage`。`context.Context` 非対応)。2022 年に一時 archive された経緯がある (現在は gorilla 組織の下で保守継続)。 |

**候補 A に決めた**: 依存の footprint はどちらも実質ゼロで並ぶが、A は Autobahn Test Suite (WebSocket 実装の標準的な準拠テストスイート) に通っていることを明示しており、この PR で最も厳しく見る経路 (認証済み利用者への端末の完全な対話的操作) に置く実装として、プロトコル準拠の裏付けを重視した。API も `r.Context()` ベースの既存コード (`iwebauthn` 各関数) と自然に噛み合う。

## 帰結

- `cmd` module の `go.sum` に、`fxamacker/cbor/v2`・`x448/float16` に続いて `github.com/coder/websocket` が載る。他の module は引き続き依存ゼロ。
- `tools/archtest` の `websocket-only-dep` が、依存の広がりを CI (`make check`) で機械的に検査する。
- WebSocket の認証は、この依存の範囲外: `cmd/internal/termrelay` はハンドシェイク・フレーミングだけを担い、接続を受理する前の認証 (`requireSession`) は `cmd/goronation/serve.go` 側の既存のミドルウェアをそのまま使う (新しい認証ロジックを増やさない)。

## 代替案

検討して却下した案:

- **自前で RFC 6455 のハンドシェイク・フレーミングを書く**: 断片化なし・拡張なしに絞れば小さく書けるが、認証済み利用者に端末の完全な操作を渡す経路に、新しい自前のバイナリパーサーを増やすことになる。ユーザーの判断で、既存ライブラリを使う側を選んだ。
- **WebSocket をやめ、SSE (出力) + POST (入力) に置き換える**: Issue #1 が「WebSocket で xterm.js につなぐ」と明示済みで、この PR の再検討の対象外と判断した。
- **`golang.org/x/net/websocket`**: Go チーム自身が非推奨とし、最新の WebSocket の機能 (適切な close ハンドシェイク等) に追随していない。候補にしなかった。
