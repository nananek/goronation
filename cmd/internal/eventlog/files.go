package eventlog

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// ファイル名。DB・WAL・共有メモリ・掃除用のロックは、すべて、セッションのディレクトリの直下。
const (
	dbName   = "events.db"
	lockName = "events.lock"
)

// dbFiles は、DB と、SQLite が隣に作るファイル。
var dbFiles = []string{dbName, dbName + "-wal", dbName + "-shm", dbName + "-journal"}

var errLocked = errors.New("eventlog: ロックを他が持っている")

// acquireLock は、root (セッションのディレクトリ) の events.lock を、作って (0600・symlink を辿らない)、排他で掴む。
// 他が持っていれば errLocked。掴んだあと、path がまだ同じファイルを指すことを確かめる (持ち主が、終了時にロックのファイルを
// 消す: 消えたファイルの flock を掴んで「取れた」と思い込まない)。
func acquireLock(root *os.Root) (*os.File, error) {
	for range 4 {
		f, err := root.OpenFile(lockName, os.O_RDWR|os.O_CREATE|oNoFollow, 0o600)
		if err != nil {
			return nil, err
		}
		fi, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, err
		}
		if !fi.Mode().IsRegular() || !ownedByMe(fi) {
			f.Close()
			return nil, fmt.Errorf("eventlog: %s が通常のファイルでない・自分のものでない", lockName)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			if err := f.Chmod(0o600); err != nil {
				f.Close()
				return nil, err
			}
		}
		if err := tryLock(f); err != nil {
			f.Close()
			return nil, err
		}
		cur, err := root.Lstat(lockName)
		if err == nil && os.SameFile(fi, cur) {
			return f, nil
		}
		f.Close() // 消された・入れ替えられた: やり直す
	}
	return nil, errors.New("eventlog: ロックのファイルが、取るたびに入れ替わる")
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

// removeLockFile は、(ロックを持ったまま) events.lock を消す。
func removeLockFile(root *os.Root) error {
	if err := root.Remove(lockName); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
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
