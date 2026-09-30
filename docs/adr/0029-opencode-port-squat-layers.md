# 0029. opencode の待ち受けポートの横取りは、構造では消せないので、起動の証明・接続後の生存確認・1 起動 1 トークンの重ねがけで抑える (M2)

- 状態: 採用
- 日付: 2026-09-30
- 関連: [Issue #1](https://github.com/nananek/goronation/issues/1)、ADR 0018・0019・0020・0028 (「PR③ で再検討」を、この ADR で閉じる)、[PR #70](https://github.com/nananek/goronation/pull/70) (横取りの再現)

## 状況

init は、`127.0.0.1:<ポート>` の opencode に、トークンつきで TCP 接続する (ADR 0019)。そのポートの持ち主が opencode でなければ、トークンと要求が渡り、応答を偽造される。PR #70 の攻撃者視点レビューで再現した: (1) opencode が死んだ直後に、同じ uid のプロセスが同じポートを bind し直す (25 回中 6 回。緩和後も約 10.5%)、(2) opencode が生きたまま、別プロセスが先に bind する。ADR 0028 は、TCP の待ち受けの横取りが効かない形 (UDS・bind 済み fd の継承) を、PR③ で必ず再検討する、とした。

## spike の結果 (opencode 2.0.20・kernel 6.12・非 root の bwrap の檻)

- **UDS・fd の継承は、採れない。** `serve` のフラグは `--hostname --port --cors --service --stdio` だけ。`--hostname` に path を渡すと起動に失敗する (`baseURI must have a hostname`・`getaddrinfo ENOTFOUND`)。設定の `server` は `port`・`hostname`・`mdns`・`mdnsDomain`・`cors` だけ。環境変数 (`OPENCODE_*` 約 50 個を、バイナリの文字列から全件確認) に、listen の fd・socket path の口は無い (`LISTEN_FDS` も無い)。opencode の改造 (`LD_PRELOAD` の bind フック) は、cgo 禁止・単一の Go バイナリの不変条件で採らない。
- **ポートが使われていると、opencode は起動に失敗して終わる** (別ポートへの退避なし)。よって、stdout の `{"url":"http://127.0.0.1:P"}` の行 = opencode 自身が P を bind できた、という証明になる。
- 再現: `npm pack @opencode/cli-linux-x64@2.0.20` → `bin/opencode serve --help`・`strings` で上の名前を数える・P を先に bind して起動する。

## 決定

構造で消せないので、「起きる条件を消す」と「起きても実害を限る」を重ねる。

1. **L0 起動の証明。** init は `serve --port P` (P は init が選ぶ) で起動し、子の stdout の最初の 1 行 (上限・期限 30 秒つき) が、init が選んだ `http://127.0.0.1:P` と**完全に一致する**まで、要求を受けない (受けた fd は閉じる)。一致しない・期限切れ・子が先に終わる、は fail closed (init が非 0 で終わる)。行が出たあと、init が `127.0.0.1:P` に `SO_REUSEPORT` つきで bind を試み、成功したら (持ち主が `SO_REUSEPORT` を付けている) 起動を拒否する。
2. **L1 接続後の生存確認。** 1 要求ごとに、接続 → 子の pidfd の生存確認 → そのあとで初めて先頭 (トークン) を書く。死んだ opencode の代わりの bind は、子が死んだあとにしか成立しないので、弾ける。**残る窓** (opencode の終了の途中) は、PR③c で PR #70 のレビューと同じ手法で測り、率を ADR に実測で書く。0 にならなければ、0 でないと書く。
3. **L2 1 起動 1 トークン。** 子が死んだら、init は受け付けを止め、control を閉じて終わる。opencode を再起動しない。トークンは、この 1 回の起動にだけ使う (再利用しない。ADR 0028 の「再利用なら Blocking」に当たらない)。
4. **L3 実害の限定。** 窓を突かれても、トークンは死んだサーバーにしか通らない使い捨てで、偽の応答・SSE はその 1 要求の接続に限られる。
5. **L4 (tool の `kill` からの opencode の防御) は、この ADR では採用しない。** opencode は tool の後始末に `kill(0)`・`kill(-pgid)` を使うので、塞ぐと互換が壊れうる。互換を実測してから、別の ADR で決める。

## 帰結

- 窓は消えない。L1 の率が実測の受け入れ基準を満たすかは、PR③c で決める (先に目標値を決めない)。
- opencode の upstream が `--unix`・fd の受け取りを持てば、L0〜L2 より強い対策になる。版が変わったら、spike を再実行する。
- 子が死ぬとセッションが終わる。再開は、新しい起動 (新しいトークン) になる。

## 代替案

- UDS・fd の継承: 上のとおり、opencode に口が無い。
- `LD_PRELOAD` で bind を書き換える・opencode を改造する: native のコードが要る。
- トークンを使い回す・再起動して続ける: L0 の証明と、トークンの使い捨てが崩れる。
- 死んだあとの受け付け停止だけ (PR #70 の緩和): 子が生きたまま先に bind される経路に効かない。
