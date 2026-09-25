// Package core は goronation の domain 型と ports を置く。
//
// backend / adapter の実装 (sandbox/bwrap など) は import しない。
// この規則は go.mod では守れないため、tools/archtest が強制する。
package core
