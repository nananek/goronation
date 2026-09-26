module github.com/nananek/goronation/core

go 1.24.0

// replace example.com/c => ./x は、コメントなので違反にしない。
require example.com/replace v1.0.0

replace example.com/a => ./_a
