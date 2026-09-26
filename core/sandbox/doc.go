// Package sandbox は、サンドボックスのバックエンド (bwrap・seatbelt) が満たす契約 (port) で、檻の起動仕様 (Spec) を「意図」で表し、実装が満たす interface を定める。
//
// 契約が表すのは、檻に何が見えるか (読める・書ける path の集合)・何が渡るか (許可リストの環境変数)・どこへ届くか (egress の UDS だけ) で、
// バックエンドの機構 (bwrap の mount・symlink・/proc) は表さない。契約の文言を満たすことは、実装ごとに sandbox/conformance が確かめる。
// 檻の中のコード自身の振る舞いは、保証しない。この package は標準ライブラリだけを使い、プロセスを起動しない。
//
// # 使い方
//
//	c, err := backend.Start(ctx, sandbox.Spec{Exec: "/usr/bin/git", System: true})
//	err = c.Wait() // 異常終了は *sandbox.ExitError
//
// # 規則
//
//   - default-closed: Spec に無いものは、檻に入らない (path・環境変数・継承した fd)。
//   - die-with-parent: 檻を起動した呼び手が死んだら、檻も死ぬ。
//   - egress-only: ネットワークは無く、Egress の UDS にだけ届く。
//   - no-secrets: 資格情報らしい環境変数名と、ホストの機密 path は、ErrRejected で断る。
//   - path-remap: GuestPath (檻の中の path) を HostPath と変えられるのは、Capabilities.PathRemap のバックエンドだけ。
//   - exit-code: 異常終了は *ExitError で、シグナルで死んだら、終了コードは 128 + 番号。
//   - terminal: Terminal の注入を塞げると確かめられなければ、起動せず、ErrTerminalUnsafe を返す。
//
// # 限界
//
//   - この package は型と文言だけで、強制しない。強制は実装 (sandbox/*) と、検証の共通部品 (sandbox/contract) が行う。
//   - Capabilities が false の項目は、満たさない。適合テストは、「満たさない」ことも確かめる (改善しても悪化しても赤くなる)。
//   - 契約の範囲は、path・環境変数・fd・ネットワーク・プロセス・端末。OS 固有の経路 (macOS の Mach IPC・pasteboard・LaunchServices・Keychain など) は範囲外で、バックエンドが別に塞ぐ。
package sandbox
