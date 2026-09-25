package main

import "reflect"

func f(v reflect.Value) {
	_ = reflect.NewAt
	v.UnsafePointer()
}

func main() {}
