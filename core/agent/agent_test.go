package agent

import (
	"testing"

	v0 "github.com/nananek/goronation/spec/v0"
)

// 型の契約を、コンパイル時に固定する: Adapter・Stream を満たす、最小の実装が書けること。
type nopStream struct{}

func (nopStream) DecodeFrame([]byte) ([]v0.Envelope, error) { return nil, nil }
func (nopStream) EncodeCommand(v0.Command) ([]byte, []v0.Envelope, error) {
	return nil, nil, nil
}

type nopAdapter struct{}

func (nopAdapter) Name() string      { return "nop" }
func (nopAdapter) NewStream() Stream { return nopStream{} }

var _ Adapter = nopAdapter{}

func TestNopAdapter(t *testing.T) {
	var a Adapter = nopAdapter{}
	if a.Name() != "nop" || a.NewStream() == nil {
		t.Fatal("Adapter が、名前と Stream を返さない")
	}
}
