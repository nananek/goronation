package eventlog

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// ファイル名。DB・WAL・共有メモリは、セッションのディレクトリの直下。掃除用のロックは、そのディレクトリ自身 (acquireLock)。
const dbName = "events.db"

// dbFiles は、DB と、SQLite が隣に作るファイル。
var dbFiles = []string{dbName, dbName + "-wal", dbName + "-shm", dbName + "-journal"}

var errLocked = errors.New("eventlog: ロックを他が持っている")

// acquireLock は、root (セッションのディレクトリ) 自身の fd に、排他の flock を、待たずに掛ける。他が持っていれば errLocked。
// ファイルでなくディレクトリの inode に掛けるので、lock ファイルの rename・削除・差し替えで、ロックを外せない (vault の lockDir と同じ)。
// 返す File を閉じると解ける (kernel は、プロセスが死んでも解く)。
func acquireLock(root *os.Root) (*os.File, error) {
	f, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	if err := tryLock(f); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// removeDBFiles は、DB と隣のファイルを消す (無いものは無視)。
func removeDBFiles(root *os.Root) error {
	var first error
	for _, n := range dbFiles {
		if err := root.Remove(n); err != nil && !errors.Is(err, fs.ErrNotExist) && first == nil {
			first = err
		}
	}
	return first
}

// checkDir は、セッションのディレクトリが、自分のもので、他の人が書けないことを確かめる (ファイルの入れ替えを防ぐ)。
func checkDir(root *os.Root) error {
	fi, err := root.Stat(".")
	if err != nil {
		return err
	}
	if !fi.IsDir() || !ownedByMe(fi) || fi.Mode().Perm()&0o022 != 0 {
		return errors.New("eventlog: ディレクトリが、自分のものでない・他の人に書ける")
	}
	return nil
}

// checkFile は、通常のファイル・自分の uid・0600 (group・other の権限なし) であることを確かめる。
func checkFile(fi os.FileInfo) error {
	if !fi.Mode().IsRegular() || !ownedByMe(fi) || fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("eventlog: %s が、通常のファイル・自分の uid・0600 でない (%v)", fi.Name(), fi.Mode())
	}
	return nil
}
