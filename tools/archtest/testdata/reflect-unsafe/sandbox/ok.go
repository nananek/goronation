package sandbox

import "reflect"

func f(v reflect.Value) {
	_ = reflect.NewAt
	v.UnsafePointer()
	v.UnsafeAddr()
	v.SetPointer(nil)
}
