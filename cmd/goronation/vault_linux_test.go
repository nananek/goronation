package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nananek/goronation/vault"
)

// vaultFixture は、<state>/vault に、passkey 1 つのラップを持つ Vault を作って閉じ、state と、Vault の ID を返す。
func vaultFixture(t *testing.T) (state, dir, id string) {
	t.Helper()
	state = t.TempDir()
	dir = filepath.Join(state, "vault")
	v, err := vault.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	salt, err := vault.NewSalt()
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Init("cred-a", salt, bytes.Repeat([]byte{7}, vault.PRFSize)); err != nil {
		t.Fatal(err)
	}
	v.Close()
	id, err = vault.PeekID(dir)
	if err != nil || len(id) != 32 {
		t.Fatalf("PeekID = %q, %v", id, err)
	}
	return state, dir, id
}

// ttyIn は、端末 (pty の slave) を stdin にし、master に line を書いておく (canonical モードなので、読まれるまで溜まる)。
func ttyIn(t *testing.T, line string) *os.File {
	t.Helper()
	master, slave, err := openHostPty()
	if err != nil {
		t.Skipf("openHostPty: %v", err)
	}
	t.Cleanup(func() { master.Close(); slave.Close() })
	if line != "" {
		if _, err := master.WriteString(line); err != nil {
			t.Fatal(err)
		}
	}
	return slave
}

func vaultFileExists(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "vault.json"))
	return err == nil
}

func TestVaultResetOnTerminal(t *testing.T) {
	state, dir, id := vaultFixture(t)
	var out, errb bytes.Buffer
	code := runVault([]string{"reset", "--state-dir", state}, ttyIn(t, id[:8]+"\n"), &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d\nstderr: %s", code, errb.String())
	}
	if vaultFileExists(dir) {
		t.Fatal("vault.json が残っている")
	}
	if !strings.Contains(out.String(), id) || !strings.Contains(errb.String(), id) {
		t.Fatalf("ID を見せる: out=%q err=%q", out.String(), errb.String())
	}
	audit, err := os.ReadFile(filepath.Join(dir, "audit.log"))
	if err != nil || !strings.Contains(string(audit), `"event":"reset"`) || !strings.Contains(string(audit), id) {
		t.Fatalf("監査の記録 = %q, %v", audit, err)
	}
}

func TestVaultResetRefusesWithoutTerminal(t *testing.T) {
	state, dir, _ := vaultFixture(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	w.WriteString("anything\n")
	var out, errb bytes.Buffer
	if code := runVault([]string{"reset", "--state-dir", state}, r, &out, &errb); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "端末") || !vaultFileExists(dir) {
		t.Fatalf("端末でなければ、実行しない (何も消さない): %q", errb.String())
	}
	// /dev/null も、端末でない。
	null, _ := os.Open(os.DevNull)
	defer null.Close()
	if code := runVault([]string{"reset", "--state-dir", state}, null, &out, &errb); code != 1 || !vaultFileExists(dir) {
		t.Fatalf("/dev/null: exit = %d", code)
	}
}

func TestVaultResetWrongConfirmation(t *testing.T) {
	state, dir, id := vaultFixture(t)
	for name, in := range map[string]string{
		"別の文字列":    "no\n",
		"空":        "\n",
		"ID の一部だけ": id[:7] + "\n",
		"長すぎる":     strings.Repeat("a", 200) + "\n",
	} {
		var out, errb bytes.Buffer
		if code := runVault([]string{"reset", "--state-dir", state}, ttyIn(t, in), &out, &errb); code != 1 {
			t.Errorf("%s: exit = %d, want 1", name, code)
		}
		if !vaultFileExists(dir) {
			t.Fatalf("%s: 確認が合わないのに、消えた", name)
		}
		if strings.Contains(out.String(), "消した") {
			t.Errorf("%s: 消したと言った", name)
		}
	}
}

func TestVaultResetRefusesWhileInUse(t *testing.T) {
	state, dir, id := vaultFixture(t)
	v, err := vault.Open(dir) // デーモンの代わり: lock を持つ
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	var out, errb bytes.Buffer
	if code := runVault([]string{"reset", "--state-dir", state}, ttyIn(t, id[:8]+"\n"), &out, &errb); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "デーモン") || !vaultFileExists(dir) {
		t.Fatalf("動いている間は、消さない: %q", errb.String())
	}
}

func TestVaultResetNoVaultAndBrokenVault(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runVault([]string{"reset", "--state-dir", t.TempDir()}, ttyIn(t, ""), &out, &errb); code != 1 || !strings.Contains(errb.String(), "Vault が無い") {
		t.Fatalf("Vault が無い: exit = %d, %q", code, errb.String())
	}
	// 壊れた Vault (ID を読めない) は、"reset" を入力させる。
	state, dir, _ := vaultFixture(t)
	if err := os.WriteFile(filepath.Join(dir, "vault.json"), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errb.Reset()
	if code := runVault([]string{"reset", "--state-dir", state}, ttyIn(t, "reset\n"), &out, &errb); code != 0 {
		t.Fatalf("壊れた Vault: exit = %d, %s", code, errb.String())
	}
	if vaultFileExists(dir) {
		t.Fatal("壊れた vault.json が残っている")
	}
}

func TestVaultUsageErrors(t *testing.T) {
	for name, args := range map[string][]string{
		"no subcommand": nil,
		"unknown":       {"serve"},
		"extra arg":     {"reset", "x"},
		"bad flag":      {"reset", "--nope"},
	} {
		var out, errb bytes.Buffer
		if code := runVault(args, ttyIn(t, ""), &out, &errb); code != exitUsage {
			t.Errorf("%s: exit = %d, want %d (%s)", name, code, exitUsage, errb.String())
		}
	}
	var out, errb bytes.Buffer
	if code := runVault([]string{"-h"}, ttyIn(t, ""), &out, &errb); code != 0 || !strings.Contains(errb.String(), "reset") {
		t.Fatalf("-h: exit = %d, %q", code, errb.String())
	}
}

func TestVaultDispatch(t *testing.T) {
	var out, errb bytes.Buffer
	if code := dispatch([]string{"vault"}, &out, &errb); code != exitUsage {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(errb.String(), "vault") {
		t.Fatalf("stderr = %q", errb.String())
	}
}
