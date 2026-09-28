//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestResolveExeOK(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bin")
	writeExecutable(t, p, "\x7fELF fake binary")

	real, err := resolveExe(p)
	if err != nil {
		t.Fatalf("resolveExe: %v", err)
	}
	if real != p {
		t.Fatalf("real = %q, want %q", real, p)
	}
}

func TestResolveExeNotExecutable(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "notexec")
	if err := os.WriteFile(p, []byte("data"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := resolveExe(p); err == nil {
		t.Fatal("実行権限が無いのに、resolveExe が成功した")
	}
}

func TestResolveExeMissing(t *testing.T) {
	if _, err := resolveExe("/no/such/binary/anywhere"); err == nil {
		t.Fatal("存在しないパスなのに、resolveExe が成功した")
	}
}

func TestIsScriptDetectsShebang(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "script")
	writeExecutable(t, script, "#!/bin/sh\necho hi\n")
	if !isScript(script) {
		t.Fatal("シェバンのあるファイルが isScript で検出されない")
	}

	bin := filepath.Join(dir, "bin")
	writeExecutable(t, bin, "\x7fELF fake binary")
	if isScript(bin) {
		t.Fatal("ELF (っぽい) ファイルが isScript で誤検出された")
	}
}

func TestFindBinPrefersFlagOverPath(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "mybin")
	writeExecutable(t, p, "\x7fELF fake binary")

	real, err := findBin(p, "mybin")
	if err != nil {
		t.Fatalf("findBin: %v", err)
	}
	if real != p {
		t.Fatalf("real = %q, want %q", real, p)
	}
}

func TestFindBinRejectsScript(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "myscript")
	writeExecutable(t, p, "#!/bin/sh\necho hi\n")

	if _, err := findBin(p, "myscript"); err == nil {
		t.Fatal("スクリプトなのに、findBin が成功した")
	}
}

func TestFindBinMissingGivesActionableError(t *testing.T) {
	_, err := findBin("", "definitely-not-a-real-binary-name-xyz")
	if err == nil {
		t.Fatal("存在しない名前なのに、findBin が成功した")
	}
}
