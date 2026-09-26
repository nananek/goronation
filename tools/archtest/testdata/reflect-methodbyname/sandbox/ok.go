package sandbox

import "reflect"

// sandbox/** は execAllowed の対照 (許可される場所)。
func DynUnsafePointerOK(v reflect.Value) reflect.Value {
	meta := reflect.ValueOf(v)
	m := meta.MethodByName("UnsafePointer")
	return m.Call(nil)[0]
}
