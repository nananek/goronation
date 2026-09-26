// Package session は、goro run のホスト側のセッション (private clone の作成と、成果の bundle の取り出し) を扱う。
//
// セッションは <state>/sessions/<id>/{clone,run,export} (0700)。git は、ホストで実行せず、使い捨ての檻 (ネットワーク無し・
// 資格情報無し・環境変数はクリーン) の中で実行する。ホストが読むのは、hostfs で検査した bundle だけで、os/exec を import しない (I1)。
//
// # 使い方
//
//	st, err := session.NewStore(stateDir, bwrap.CurrentHost())
//	s, err := st.Create(ctx, session.CreateOptions{Repo: path})
//	b, err := st.Export(ctx, s.ID) // b.Fetch を、利用者が自分の repo で実行する
//
// # 規則
//
//   - clone-committed: 元の repo は ro で /src に bind し、git clone --no-local --no-hardlinks する。clone されるのは
//     コミット済みの内容だけで、origin は外す。clone は、元の repo と、object を共有しない。
//   - no-host-git: ホストは、clone の中で git を実行しない (.git/config・hooks は、檻が書ける)。export は、clone を ro で bind した
//     使い捨ての檻で bundle を作る。
//   - bundle-checked: bundle は、hostfs で、通常のファイル・上限 (512 MiB)・ヘッダを検査してから、path を返す。
//   - safe-fetch: 取り込みのコマンドには、名前が安全な refs/heads/* だけを書き (名前は檻が決める)、transfer.fsckObjects=true を付ける。
//     タグと submodule は、自動では取り込まない (--no-tags・--no-recurse-submodules)。
//
// # 限界
//
//   - ローカルの path だけ (URL は、egress が要るので後の PR)。git は、ホストの /usr/bin/git を、檻の中で使う。
//   - export/ の中の bundle 以外のファイルの量は、見ない。bundle の中身は、利用者の git fetch (fsck) が最後の関門。
package session
