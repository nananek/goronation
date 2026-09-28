//go:build linux

package main

import (
	"slices"
	"testing"

	"github.com/nananek/goronation/sandbox/bwrap"
)

func TestCageSpecBinds(t *testing.T) {
	cfg := cageConfig{
		Host:         bwrap.Host{Home: "/home/user"},
		AgentExe:     "/opt/real/opencode",
		AgentBinName: "opencode",
		GoroExe:      "/opt/real/goronation",
		CACertPEM:    "/tmp/ca.pem",
		RunDir:       "/tmp/run",
		Home:         "/tmp/home",
		Work:         "/tmp/work",
	}
	spec := cageSpec(cfg)

	want := []bwrap.Bind{
		{Src: "/usr", Dst: "/usr", RW: false},
		{Src: "/opt/real/opencode", Dst: "/opt/agent/opencode", RW: false},
		{Src: "/opt/real/goronation", Dst: jailGoro, RW: false},
		{Src: "/tmp/ca.pem", Dst: jailCACert, RW: false},
		{Src: "/tmp/run", Dst: jailRun, RW: false},
		{Src: "/tmp/home", Dst: jailHome, RW: true},
		{Src: "/tmp/work", Dst: jailWork, RW: true},
	}
	if !slices.Equal(spec.Binds, want) {
		t.Fatalf("Binds = %+v, want %+v", spec.Binds, want)
	}
}

func TestCageSpecInHomeDetection(t *testing.T) {
	cfg := cageConfig{
		Host:         bwrap.Host{Home: "/home/user"},
		AgentExe:     "/home/user/.local/bin/opencode", // ホストの HOME の下
		AgentBinName: "opencode",
		GoroExe:      "/opt/real/goronation",
		CACertPEM:    "/tmp/ca.pem",
		RunDir:       "/tmp/run",
		Home:         "/tmp/home",
		Work:         "/tmp/work",
	}
	spec := cageSpec(cfg)
	for _, b := range spec.Binds {
		if b.Src == cfg.AgentExe && !b.InHome {
			t.Fatalf("HOME の下の Src (%s) なのに InHome が false", b.Src)
		}
	}
}

func TestCageSpecCmdAndChdir(t *testing.T) {
	cfg := cageConfig{
		Host: bwrap.Host{Home: "/home/user"}, AgentExe: "/opt/real/opencode", AgentBinName: "opencode",
		GoroExe: "/opt/real/goronation", CACertPEM: "/tmp/ca.pem", RunDir: "/tmp/run",
		Home: "/tmp/home", Work: "/tmp/work", Args: []string{"run", "--format", "json", "hi"},
	}
	spec := cageSpec(cfg)

	want := []string{
		jailGoro, "init", "--listen", jailProxyAddr, "--upstream", jailRun + "/" + proxySockName,
		"--no-forward-tty", "--", "/opt/agent/opencode", "run", "--format", "json", "hi",
	}
	if !slices.Equal(spec.Cmd, want) {
		t.Fatalf("Cmd = %+v, want %+v", spec.Cmd, want)
	}
	if spec.Chdir != jailWork {
		t.Fatalf("Chdir = %q, want %q", spec.Chdir, jailWork)
	}
	if spec.NewSession {
		t.Fatalf("NewSession = true, want false (端末に直結しないハーネスなので)")
	}
}

func TestCageEnvIncludesCommonAndExtra(t *testing.T) {
	env := cageEnv([]bwrap.EnvVar{{Key: "FOO", Value: "bar"}})
	byKey := map[string]string{}
	for _, e := range env {
		byKey[e.Key] = e.Value
	}
	for _, k := range []string{"HOME", "PATH", "TERM", "LANG", "NODE_EXTRA_CA_CERTS"} {
		if _, ok := byKey[k]; !ok {
			t.Errorf("cageEnv に %s が無い: %+v", k, env)
		}
	}
	if byKey["HOME"] != jailHome {
		t.Errorf("HOME = %q, want %q", byKey["HOME"], jailHome)
	}
	if byKey["NODE_EXTRA_CA_CERTS"] != jailCACert {
		t.Errorf("NODE_EXTRA_CA_CERTS = %q, want %q", byKey["NODE_EXTRA_CA_CERTS"], jailCACert)
	}
	if byKey["FOO"] != "bar" {
		t.Errorf("extra の FOO が渡っていない: %+v", env)
	}
}
