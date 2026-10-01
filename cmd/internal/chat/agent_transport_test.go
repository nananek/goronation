package chat

import "testing"

// claude は、標準入出力に行を流す (stdio)。
func TestClaudeUsesStdioTransport(t *testing.T) {
	l, err := Agent("claude")
	if err != nil {
		t.Fatal(err)
	}
	if l.Transport() != TransportStdio {
		t.Fatalf("Transport = %v, want stdio", l.Transport())
	}
}
