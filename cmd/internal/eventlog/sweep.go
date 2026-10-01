package eventlog

import (
	"errors"
	"io"
	"io/fs"
	"os"
)

// maxSweepDirs は、1 回の掃除で見る、セッションのディレクトリの数の上限 (敵対的に多いディレクトリで、起動を止めない)。
const maxSweepDirs = 10000

// SweepStale は、異常終了 (SIGKILL・OOM・電源断) の残りを、sessionsDir (<state>/groups/default/sessions) の下から掃除する。
// 各セッションのディレクトリの ディレクトリの flock を、待たずに取れれば、持ち主は死んでいるので、ロックを持ったまま DB・-wal・-shm を
// 消す (removed に数える)。取れなければ、生きているので、触らず、DB ファイルの大きさを totalBytes に足す。
// events.* の無いディレクトリ・ディレクトリでないもの・symlink は、触らない。1 つの失敗は、ほかを止めず、error にまとめて返す。
func SweepStale(sessionsDir string) (removed int, totalBytes int64, err error) {
	top, err := os.OpenRoot(sessionsDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, 0, nil
		}
		return 0, 0, err
	}
	defer top.Close()
	d, err := top.Open(".")
	if err != nil {
		return 0, 0, err
	}
	defer d.Close()
	entries, err := d.ReadDir(maxSweepDirs + 1)
	if err != nil && !errors.Is(err, io.EOF) { // n > 0 の ReadDir は、空のディレクトリで io.EOF を返す
		return 0, 0, err
	}
	var errs []error
	if len(entries) > maxSweepDirs {
		errs = append(errs, errors.New("eventlog: セッションのディレクトリが多すぎる (先頭の分だけ掃除した)"))
		entries = entries[:maxSweepDirs]
	}
	for _, e := range entries {
		if !e.IsDir() { // symlink (Type が symlink) も、ここで除く
			continue
		}
		n, size, err := sweepOne(top, e.Name())
		removed += n
		totalBytes += size
		if err != nil {
			errs = append(errs, err)
		}
	}
	return removed, totalBytes, errors.Join(errs...)
}

func sweepOne(top *os.Root, name string) (removed int, size int64, err error) {
	root, err := top.OpenRoot(name)
	if err != nil {
		return 0, 0, nil // 開けない (権限・入れ替わり): 触らない
	}
	defer root.Close()
	has := false
	for _, n := range dbFiles {
		if _, err := root.Lstat(n); err == nil {
			has = true
			break
		}
	}
	if !has {
		return 0, 0, nil
	}
	if err := checkDir(root); err != nil { // 自分のものでない・他の人に書ける: 触らない (入れ替えの余地)
		return 0, 0, nil
	}
	lock, err := acquireLock(root)
	if errors.Is(err, errLocked) {
		return 0, dirSize(root), nil // 生きている
	}
	if err != nil {
		return 0, 0, err
	}
	defer lock.Close()
	return 1, 0, removeDBFiles(root)
}
