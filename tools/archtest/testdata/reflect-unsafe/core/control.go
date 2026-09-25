package core

import "reflect"

type u struct{ Unsafe int }

func h(v reflect.Value, x u) {
	_ = v.Kind()
	_ = v.Pointer()
	_ = reflect.TypeOf(x)
	_ = x.Unsafe
	_ = v.SetInt
}
