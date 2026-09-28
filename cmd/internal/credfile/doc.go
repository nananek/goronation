// Package credfile は、資格情報 (トークン) を、ホストのファイル <state>/credentials/<名前> に保存し、読む credential.Source の実装で、ホストの中でだけ使う。
//
// 檻には、このディレクトリを見せない (goronation run が、檻に bind するのは、clone・run dir・エージェントの HOME だけ)。値は、ここで読んだ Secret として
// 呼び手へ渡り、error・ログには出ない。保証しない: 同じ利用者 (と root) の他のプロセスからの保護・ディスク上の暗号化。標準ライブラリと hostfs だけを使い、プロセスを起動しない。
//
// # 使い方
//
//	st, err := credfile.New(stateDir)
//	err = st.Save("github", credential.New(tok)); s, err := st.Token(ctx, "github")
//
// # 規則
//
//   - private: ディレクトリは利用者だけ (0700)・ファイルは 0600 (group・other・setuid の bit が 1 つでもあれば、読まずに ErrUnsafe)。所有者は、実行した利用者。
//   - regular: ファイルは、通常のファイルだけ。symlink・FIFO・ディレクトリ・ハードリンク (Nlink が 1 でない) は、読まずに ErrUnsafe。ディレクトリも symlink でない。
//   - pinned: 開いた fd を検査し、名前の Lstat と同じファイルでなければ拒否する (開く間の差し替え。hostfs)。ディレクトリは、fd で開いてから検査する。
//   - atomic: 書くときは、同じディレクトリの一時ファイル (O_EXCL・0600) に書いて fsync し、rename で置き換える。途中で失敗しても、元のファイルは変わらない。
//   - no-leak: error の文言に、値を含めない。値は、印字できる ASCII 1 語だけ (空白・制御文字・NUL・改行を含まない・1024 バイト以下)。
//
// # 限界
//
//   - 同じ利用者・root からは、読める。ディスク上は平文で、バックアップ・スナップショットに残る (暗号化は、Vault の仕事)。
//   - 検査から読むまでの間の、ディレクトリ (0700・自分の所有) の差し替えは、自分自身と root にしかできない前提で、rename の直前の検査 (同じディレクトリか) でしか、確かめない。
//   - unix (Linux・macOS) だけ。所有者・Nlink の検査に、stat を使う (Windows は対象外)。
package credfile
