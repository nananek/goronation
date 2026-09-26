//go:build !(linux || darwin)

package conformance

import "errors"

// この OS には、probe の unix 用の部品が無い (必要なら足す)。

const osNoCTTY = 0

func preadFd(fd int, buf []byte) (int, error) { return 0, errors.New("未対応") }

func killSelf() {}

func kill0(pid int) error { return errors.New("未対応") }

func isESRCH(err error) bool { return false }

func spawnDetached(marker string) error { return errors.New("未対応") }

func tiocstiOn(fd int) string { return "未対応" }
