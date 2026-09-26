package core

import "reflect"

func f(v reflect.Value) {
	v.UnsafePointer()
	v.UnsafeAddr()
	v.SetPointer(nil)
	_ = reflect.Value.UnsafePointer
}
