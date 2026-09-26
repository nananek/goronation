package core

import (
	"os"
	"reflect"
)

func target() {}

// reflect.Value.Pointer は、関数の実行中コードのアドレスを uintptr で返す。
// これと /proc/self/mem への書き込み (os.OpenFile + WriteAt) を組み合わせると、
// 実行可能メモリを作らず (Mmap・Mprotect 不要)、unsafe も reflect.NewAt も
// UnsafePointer も debug/gosym も使わずに、既存のコードページを機械語で書き換えて、
// target の呼び出しで実行できる。archtest は、この経路を検出しない
// (doc.go の限界に追記する候補)。この fixture は、その事実を pin する。
func control() {
	addr := reflect.ValueOf(target).Pointer()
	f, err := os.OpenFile("/proc/self/mem", os.O_RDWR, 0)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteAt([]byte{0x90}, int64(addr))
	target()
}
