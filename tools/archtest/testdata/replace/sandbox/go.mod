module github.com/nananek/goronation/sandbox

go 1.24.0

replace (
	example.com/a => ./_a // コメント
	example.com/b v1.0.0 => ../b
)
