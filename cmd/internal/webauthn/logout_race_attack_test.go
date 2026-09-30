package webauthn_test

import (
	"sync"
	"testing"

	"github.com/nananek/goronation/cmd/internal/webauthn"
	"github.com/nananek/goronation/cmd/internal/webauthn/webauthntest"
)

// RemoveCredential は Store.mu の下で「読んで・消して・保存」する。Logout が mu を取らないと、並行したとき、
// 古い状態 (削除前) を書き戻し、削除した passkey が復活する。
func TestRemoveCredentialIsNotUndoneByConcurrentLogout(t *testing.T) {
	uv := webauthntest.Options{UV: true}
	for i := 0; i < 40; i++ {
		w := newWorld(t)
		_, c2 := w.addPasskey(w.a, "r0")
		az := w.opAuth(w.a, webauthn.Binding{Op: webauthn.OpRemoveCredential, Target: c2.CredentialID, RequestID: "rm"}, uv)
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		go func() { defer wg.Done(); <-start; webauthn.RemoveCredential(ctx, w.st, c2.CredentialID, az) }()
		go func() { defer wg.Done(); <-start; webauthn.Logout(ctx, w.st) }()
		close(start)
		wg.Wait()
		if list, _ := webauthn.ListCredentials(ctx, w.st); len(list) != 1 {
			t.Fatalf("試行 %d: 削除した passkey が復活した (一覧 %d 件)", i, len(list))
		}
	}
}
