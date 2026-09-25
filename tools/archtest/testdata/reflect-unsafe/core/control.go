package core

import "reflect"

type u struct {
	Unsafe int
	Method string // net/http の Request.Method のような、正当な名前
}

func h(v reflect.Value, x u) {
	_ = v.Kind()
	_ = v.Pointer()
	_ = reflect.TypeOf(x)
	_ = x.Unsafe
	_ = x.Method
	_ = v.Method(0) // 添字での動的な呼び出しは、検出しない (doc.go の限界)
	_ = v.SetInt
}
