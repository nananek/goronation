//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nananek/goronation/tools/fakeproviders/anthropic"
	"github.com/nananek/goronation/tools/fakeproviders/openai"
)

// scenario は、フレームを採取する 1 つの場面: ユーザーが送るメッセージ (ターン) と、fake サーバーが
// 呼び出し順に返す応答 (Step)。claude・opencode で tool の名前・引数の形が違うので、応答はエージェント別に持つ。
// Steps の JSON のキーは、tools/fakeproviders の Step・ToolUse・ToolCall・Error の Go のフィールド名
// (大文字小文字は問わない。例: "text"・"toolUse"・"toolCalls"・"finishReason"・"error")。
type scenario struct {
	Description string `json:"description"`
	// Turns は、ユーザーが順に送るメッセージ。1 つ以上。
	Turns []string `json:"turns"`
	// Files は、エージェントの作業ディレクトリ (檻の中の /work) に、採取の前に置くファイル
	// (相対 path → 中身)。tool 呼び出しが読む対象などに使う。
	Files map[string]string `json:"files"`
	// Claude・Opencode は、それぞれの fake サーバーが返す Step (1 つ以上。尽きたら最後を繰り返す)。
	Claude   []anthropic.Step `json:"claude"`
	Opencode []openai.Step    `json:"opencode"`
	// ExpectExit は、エージェントの終了コードの期待値 (既定は 0。エラー応答の場面などで、0 以外になる)。
	ExpectExit struct {
		Claude   int `json:"claude"`
		Opencode int `json:"opencode"`
	} `json:"expect_exit"`
}

// loadScenario は、path の JSON を読んで検証する (未知のキーは、綴りの誤りとして error にする)。
func loadScenario(path string) (*scenario, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var s scenario
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(s.Turns) == 0 {
		return nil, fmt.Errorf("%s: turns が空", path)
	}
	for name := range s.Files {
		if !filepath.IsLocal(name) {
			return nil, fmt.Errorf("%s: files のキー %q は、work の下の相対 path でなければならない", path, name)
		}
	}
	return &s, nil
}

// writeFiles は、s.Files を work の下に書く。
func (s *scenario) writeFiles(work string) error {
	for name, content := range s.Files {
		p := filepath.Join(work, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			return err
		}
	}
	return nil
}

var errNoSteps = errors.New("この場面には、このエージェントの応答 (Step) が無い")
