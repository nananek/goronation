package core

type t struct{}

func (t) UnsafePointer() {}

func g(x t) {
	x.UnsafePointer()
}
