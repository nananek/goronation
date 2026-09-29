package archtest

import (
	"strings"
	"testing"
)

// UI・API の層 (cmd/goronation) が、spec/v0 を import せずに、Raw を含む Envelope を受け取れないこと。
// core/agent の Stream の DecodeFrame は、[]v0.Envelope を返す。型は推論で決まるので、spec/v0 の import が無くても、
// Envelope.Raw に触れる。core/agent と agent の実装の import も、cmd/internal/chat に限る (agent-only-in-chat)。
func TestEnvelopeCannotBeObtainedWithoutImportingV0(t *testing.T) {
	violations, _, err := Check("testdata/v0-envelope-bypass", DefaultRules)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range violations {
		if strings.HasPrefix(v.Path, "cmd/goronation/") {
			return
		}
	}
	t.Errorf("cmd/goronation/main.go は、spec/v0 を import せずに Envelope.Raw を返しているが、違反として検出されなかった: %v", violations)
}
