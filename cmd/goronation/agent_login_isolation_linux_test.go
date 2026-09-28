package main

import (
	"os"
	"testing"
)

// --login の作業ディレクトリ (/work) と run dir は、エージェントごとに別: 片方のエージェントの --login の檻が /work に置いたもの
// (opencode.json・.opencode/・.claude/settings.json など、もう片方のエージェントが起動時に読んで実行する設定) が、もう片方の
// --login の檻 (そのエージェントの HOME の認証情報を持つ) の /work に見えてはいけない。共有すると、エージェント A の檻が、
// エージェント B の檻の中で自分のコードを走らせられ、B の HOME の認証情報と B の許可宛先に届く (HOME を分けた意味がなくなる)。
func TestRunLoginWorkNotSharedAcrossAgents(t *testing.T) {
	f := newRunFixture(t)

	// claude の --login の檻が、/work にファイルを置く (偽の claude の commit は、repo が無く失敗するが、ファイルは残る)。
	f.goro(t, "run", "--login", "--", "commit", "opencode.json", "PLANTED-BY-CLAUDE-LOGIN", "msg")
	if b, err := os.ReadFile(f.agentPath("claude", "login-work", "opencode.json")); err != nil || string(b) != "PLANTED-BY-CLAUDE-LOGIN" {
		t.Fatalf("前提: claude の --login の檻が、/work (<state>/agents/claude/login-work) に書けていない: %q, %v", b, err)
	}

	// opencode の --login の檻の /work は、空のはず。
	r := f.goro(t, "run", "--agent", "opencode", "--login").mustOK(t)
	kv, _ := parseOut(r.stdout)
	if kv["work"] != "" {
		t.Errorf("opencode の --login の檻の /work に、claude の --login の檻が置いたものが見える: work=%q\n%s", kv["work"], r)
	}
}
