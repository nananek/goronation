package chat

import (
	"fmt"
	"testing"

	v0 "github.com/nananek/goronation/spec/v0"
)

func resolutionCounts(t *testing.T, e *convEnv) (requested, resolved map[string]int) {
	t.Helper()
	requested, resolved = map[string]int{}, map[string]int{}
	for _, ev := range e.events(t) {
		id, ok := requestID(ev)
		if !ok {
			continue
		}
		switch ev.Type {
		case v0.TypePermissionRequested:
			requested[id]++
		case v0.TypePermissionResolved:
			resolved[id]++
		}
	}
	return requested, resolved
}

// 決着はちょうど 1 つ: 会話が Stop で先に決着させた要求に、エージェントが、要求と撤回の組を大量に出したあとで、
// 最後の result を出しても、2 つ目の permission.resolved は配られない (決着済みの ID の記憶が、押し出されない)。
func TestSecondResolutionCannotBeForcedByEvictingSettledIDs(t *testing.T) {
	e := newConv(t, HubConfig{})
	_ = e.c.Send("hi")
	_ = e.c.OnLine(reqFrame("P"))
	e.c.Stop()
	for i := 0; i < maxSettled+100; i++ {
		id := fmt.Sprintf("X%d", i)
		_ = e.c.OnLine(reqFrame(id))
		_ = e.c.OnLine([]byte(fmt.Sprintf(`{"type":"control_cancel_request","request_id":%q}`, id)))
	}
	_ = e.c.OnLine([]byte(`{"type":"result","is_error":false}`))
	if _, resolved := resolutionCounts(t, e); resolved["P"] != 1 {
		t.Errorf("P の permission.resolved が %d 件 (ちょうど 1 件のはず)", resolved["P"])
	}
}

// 決着はちょうど 1 つ: 自動拒否の書き込みが失敗しても、画面に出した要求は、決着 (失効) が 1 つ付く。
func TestAutoRejectWriteFailureStillResolvesTheRequest(t *testing.T) {
	e := newConv(t, HubConfig{})
	_ = e.c.Send("hi")
	for i := 0; i < MaxPendingRequests; i++ {
		_ = e.c.OnLine(reqFrame(fmt.Sprintf("a%d", i)))
	}
	e.mu.Lock()
	e.failW = true
	e.mu.Unlock()
	_ = e.c.OnLine(reqFrame("over"))
	requested, resolved := resolutionCounts(t, e)
	if requested["over"] != 1 || resolved["over"] != 1 {
		t.Errorf("over: requested=%d resolved=%d (画面に出した要求に、決着が 1 つ付くはず)", requested["over"], resolved["over"])
	}
}
