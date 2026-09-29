# 0012. chat の起動: 標準入出力の隔離・洪水・世代 (M1.5)

- 状態: 採用
- 日付: 2026-09-29
- 関連: [Issue #1](https://github.com/nananek/goronation/issues/1)、ADR 0009・0010・0011、`cmd/goronation/serve_chat.go`、`cmd/internal/chat`

## 状況

ADR 0010 の B3: 標準出力が pipe の間、檻の中のプロセスは、偽のフレーム (偽の result・cancel・can_use_tool・承認) を書ける。

## 決定

1. **標準入出力は、入力用・出力用の別々の socketpair。** 標準エラー出力だけが pipe (上限 64 KiB で serve の標準エラー出力に残す。UI に出さない)。子が片方の fd を継承しても、逆向きには書けない。
2. **socketpair だけでは塞がらない (実測。ptrace_scope=0・非 root・bwrap)。** `/proc/<pid>/fd/N` の開き直しは ENXIO、connect は失敗するが、同じ uid の檻の中の子は `pidfd_getfd`・`PTRACE_SEIZE`・`/proc/<pid>/mem` で fd を奪う・メモリを書き換えられた。次の 3 つで、すべて断られる (EACCES・EPERM・ENXIO):
   - エージェントは、**読めない複製 (mode 0100) から起動**する (kernel は、読めない実行ファイルの exec を dumpable=0 にする)。実物の claude 2.1.284 は、これで動く。
   - **goronation init は、bwrap の `--as-pid-1` で PID 1 になり**、`--non-dumpable` で自分を dumpable=0 にする (bwrap 自身の PID 1 は dumpable のまま標準入出力を持つため)。
   - 断るのは、kernel の許可検査 (`ptrace_may_access`)。seccomp は要らない。
3. **root では起こさない** (檻に capability が残り、2 は意味を失う)。
4. **配線は `cmd/internal/chat`。** `chat.Agent(name)` が Launch (引数・Stream の作り方。M1.5 は claude だけ)、`chat.Session` が LineReader・Feed・Conversation・Hub・書き込みのキューをつなぐ。`cmd/goronation` は名前・引数・バイト列・Event の JSON だけを扱う。initialize は、アダプタが最初の prompt の前に書く (別に送らない)。
5. **洪水 (L11)**: Write は有界のキュー (256 行・4 MiB。`QueuedWriter`)。入力を読むエージェントは、要求の洪水 (自動拒否の書き込み) でも、会話は終わらない。**読まないエージェントは、キュー満杯で書き込みの失敗になり、会話を終える** (OnStop で檻を止める。受け入れる: 読まないエージェントを残しても、承認は進まない)。
6. **世代 (L12)**: `Conversation.Generation()` (起動ごとのランダム値) と `ResolveIn` (違う世代は `ErrStaleGeneration`)。`/permission` に世代を添える形は PR⑤ (hello に載せる案)。API の形が変わるので、着手前に確認する。
7. **L15**: 未対応の `control_request` は `agent.frame` になり、返答しない。`initialize` の応答に `pending_user_dialog_requests` があり、対話が要る別の要求が出うる。M1.5 が扱うのは `can_use_tool` だけで、ほかは手動の「終了」で止める。実物での出方の採取と、拒否の返答は M2。

## 帰結

- 断れるのは、標準入出力の fd と、エージェントのメモリまで。エージェントの子が、設定ファイル (`/work/.claude/settings*.json`・HOME) を書き換えて承認を迂回する経路は、別の問題 (塞がない)。同じ uid の子は、エージェントを kill できる (DoS)。
- 実物の Bash tool の fd の継承は、hook の子で確かめた (継承なし)。PR⑧ の E2E で、tool の子でも確かめる。
- 複製 (約 240 MB) を、エージェントの状態ディレクトリに持つ (版が変わると作り直し、古い複製は消す)。
- yama (ptrace_scope ≥ 1) の環境では、断る理由が増えるだけで、結果は同じ。CI の runner は scope 1 なので、対照テストは SKIP になり、ハードニングを外した変異は CI では区別できない 。**運用の約束**: bwrap を使う変更 (檻の組み立て・`chat_*_bwrap_linux_test.go`) は、`ptrace_scope=0` の非 root で、手元で `make test-bwrap` を回して確かめる (CI では確かめられない)。
- 檻は `--new-session` で起こす (制御端末を持つ serve から起こしても、`/dev/tty` に書けない・読めない)。エージェントの標準エラー出力は、prefix `[agent] ` (ready 行 `goronation serve: …` に一致させない) を付け、制御文字を `?` にして serve の標準エラー出力に出す。本番の入口を通るテストで固定する。

## Limit

- 版の違う実体を並行に呼ぶと、読めない複製の掃除が、他方の返した path を消しうる。
- `Conversation.Resolve` (世代なし) は公開のまま。PR⑤ は `ResolveIn` だけを使う。
- root では chat が動かない (`errChatRoot`)。手動確認は非 root で行う。
- Bash tool の子の fd の継承は未確認 (PR⑧)。

## 代替案

- seccomp で `pidfd_getfd`・`ptrace` を塞ぐ: `/proc/<pid>/mem` が残り、それだけでは足りない。dumpable=0 は、3 つとも同じ検査で断る。
- エージェントの子を別 uid・別 pid namespace にする: エージェントが子を起こすので、goronation からは決められない。設計の変更が大きい。
- pipe のまま、フレームに認証 (MAC) を付ける: claude の stream-json に、その仕組みが無い。
