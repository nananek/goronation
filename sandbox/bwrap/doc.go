// Package bwrap は、Linux 向けのサンドボックスバックエンド (bubblewrap) で、檻の起動仕様 (Spec) を検証して argv にし、起動する。
//
// 檻は常に --unshare-all (ネットワークを含む)・--die-with-parent・--clearenv で作り、Spec に無いものは、檻に入らない
// (許可リスト方式)。起動器は固定パスの /usr/bin/bwrap だけで、PATH は検索しない。檻の中のコード自身の振る舞いは、保証しない。
//
// # 使い方
//
//	c, err := bwrap.Start(ctx, bwrap.Spec{Host: bwrap.CurrentHost(), Binds: binds, Env: env, Cmd: cmd})
//	err = c.Wait() // Start は、検証に通らない Spec を起動しない
//
// # 規則
//
//   - fixed-flags: --unshare-all・--die-with-parent・--clearenv・--proc・--dev は常に付く。外す手段も、--share-net を出す手段も無い。
//   - bind-src: 機密の Src (ホストの HOME 自体・~/.ssh・~/.claude・/run・/etc・/root・/home・/・SSH_AUTH_SOCK の実体など) は error。
//     HOME の下は、InHome を明示した作業用 dir だけ。システムの path は ro だけ。Start は、symlink を辿った実体にも同じ規則をかける。
//   - bind-dst: Dst は絶対・クリーンで、重複せず、/・/proc・/dev の中は使えない。
//   - env: 檻の環境変数は、Spec.Env に書いたものだけ。資格情報らしい名前 (SSH_AUTH_SOCK・*_TOKEN・AWS_* など) は error。
//   - tty: NewSession が false (既定) で、端末に直結して (標準入出力のどれかが端末か、制御端末を持つ) 起動するとき、TIOCSTI が無効と確かめられなければ、起動しない。
//
// # 限界
//
//   - seccomp・cap-drop・cgroup の制限は未実装 (uid 0 で起動すると、檻の中に capability が残る)。pty 中継も未実装で、
//     端末に直結する檻は制御端末を共有し、TIOCSTI は kernel の設定に頼る。
//   - 検証は path の字面と、Start が解決した実体だけ (解決の後の差し替えは見ない)。値の中身は見ない。檻の / は tmpfs で、書けるが揮発する。
//
// # 関連
//
// docs/adr/0001-repository-layout.md
package bwrap
