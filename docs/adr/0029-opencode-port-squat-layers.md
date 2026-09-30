# 0029. opencode のポートの横取りは、起動の証明・接続後の生存確認・1 起動 1 トークンで抑える (M2)

- 状態: 採用
- 日付: 2026-09-30
- 関連: [Issue #1](https://github.com/nananek/goronation/issues/1)、ADR 0019・0020・0028 (「PR③ で再検討」をこの ADR で閉じる)、[PR #70](https://github.com/nananek/goronation/pull/70) (再現)・[PR #71](https://github.com/nananek/goronation/pull/71) (spike の詳細)

## 状況

init は `127.0.0.1:<ポート>` の opencode に、トークンつきで TCP 接続する (ADR 0019)。ポートの持ち主が opencode でなければ、トークンと要求が渡り、応答を偽造される。PR #70 のレビューで再現した: (1) opencode の死の直後に、同 uid のプロセスが同じポートを bind する (緩和前 25 回中 6 回・緩和後も約 10.5%)、(2) opencode が生きたまま、別プロセスが先に bind する。ADR 0028 は、横取りが効かない形 (UDS・bind 済み fd の継承) を PR③ で再検討する、とした。

## spike の結果 (opencode 2.0.20)

- **UDS・fd の継承は採れない。** フラグは `--hostname --port --cors --service --stdio` だけ。設定の `server` は `port hostname mdns mdnsDomain cors` だけ。環境変数 `OPENCODE_*` に fd・socket path の口は無い。`--hostname` に path を渡すと起動に失敗する。改造は、cgo 禁止の不変条件で採らない。
- **ポートが使われていると、opencode は起動に失敗して終わる。** stdout の `{"url":"http://127.0.0.1:P"}` の行は、opencode 自身が P を bind できた証明になる。

## 決定

構造で消せないので、「起きる条件を消す」と「起きても実害を限る」を重ねる。

1. **L0 起動の証明。** init が選んだ P で `serve --port P` を起動し、子の stdout の最初の 1 行 (上限・30 秒の期限つき) が `http://127.0.0.1:P` と**完全に一致する**まで、要求を受けない。不一致・期限切れ・子の先の終了は fail closed。その行のあと、`SO_REUSEPORT` つきで P を bind して成功したら (持ち主が `SO_REUSEPORT` を付けている)、起動を拒否する。
2. **L1 接続後の生存確認。** 1 要求ごとに、接続 → 子の pidfd の生存確認 → 先頭 (トークン) の書き込み、の順にする。死んだ opencode の代わりの bind は、子の死の後にしか成立しないので弾ける。**残る窓** (終了の途中) は、PR③c で PR #70 と同じ手法で測り、実測の率を書く (0 にならなければ、そう書く)。
3. **L2 1 起動 1 トークン。** 子が死んだら、init は受け付けを止め、control を閉じて終わる。opencode を再起動せず、トークンを再利用しない。
4. **L3 実害の限定。** 窓を突かれても、トークンは死んだサーバーにしか通らない使い捨てで、偽の応答は、その 1 要求の接続に限られる。
5. **L4 (tool の `kill` からの opencode の防御) は採らない。** opencode は tool の後始末に `kill(0)`・`kill(-pgid)` を使うので、塞ぐと壊れうる。互換を実測してから、別の ADR で決める。

## 帰結

- 窓は消えない。受け入れの基準は、PR③c の実測を見て決める (先に目標値を決めない)。
- 子が死ぬとセッションは終わり、再開は新しい起動 (新しいトークン) になる。
- opencode が fd・UDS の口を持てば、より強い対策になる。版が変わったら spike を再実行する。

## 代替案

- UDS・fd の継承・opencode の改造: 口が無い・native のコードが要る。
- トークンの使い回し・再起動して続ける: L0 の証明と使い捨てが崩れる。
- 子の死の後の受け付け停止だけ (PR #70): 子が生きたまま先に bind される経路に効かない。
