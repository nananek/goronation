package claude

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	v0 "github.com/nananek/goronation/spec/v0"
)

func canUseToolFrame(id, input string) []byte {
	return []byte(fmt.Sprintf(`{"type":"control_request","request_id":%q,"request":{"subtype":"can_use_tool","tool_name":"Bash","tool_use_id":"tu","input":%s}}`, id, input))
}

func resolveCommand(t *testing.T, id, outcome string) v0.Command {
	t.Helper()
	d, err := json.Marshal(map[string]string{"request_id": id, "outcome": outcome})
	if err != nil {
		t.Fatal(err)
	}
	return v0.Command{Type: v0.CommandPermissionResolve, Data: d}
}

// 決着した request_id の再要求は、新しい未決の要求にならない (ID は一意・応答は 1 回限り。ADR 0010)。
// 再送された allow_once が、人間の見ていない別の input を許可してはならない。
func TestSettledRequestIDCannotBeRequestedAgain(t *testing.T) {
	s := Adapter{}.NewStream()
	if _, err := s.DecodeFrame(canUseToolFrame("R1", `{"command":"ls"}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.EncodeCommand(resolveCommand(t, "R1", v0.AllowOnce)); err != nil {
		t.Fatal(err)
	}
	envs, err := s.DecodeFrame(canUseToolFrame("R1", `{"command":"rm -rf /work"}`))
	if err != nil {
		t.Fatal(err)
	}
	if envs[0].Type == v0.TypePermissionRequested {
		t.Errorf("決着済みの request_id の再要求が、新しい permission.requested になった")
	}
	if line, _, err := s.EncodeCommand(resolveCommand(t, "R1", v0.AllowOnce)); err == nil {
		t.Errorf("決着済みの request_id への再送の allow_once が、別の input を許可した: %s", line)
	}
}

// 覚える request_id の数には上限がある。超えた要求は permission.requested にならない (誤って許可もしない)。
func TestRequestIDMemoryIsBounded(t *testing.T) {
	s := Adapter{}.NewStream()
	for i := 0; i < maxSeenRequests; i++ {
		if _, err := s.DecodeFrame(canUseToolFrame(fmt.Sprintf("f%d", i), `{}`)); err != nil {
			t.Fatal(err)
		}
	}
	envs, err := s.DecodeFrame(canUseToolFrame("over", `{}`))
	if err != nil {
		t.Fatal(err)
	}
	if envs[0].Type == v0.TypePermissionRequested {
		t.Errorf("上限を超えた要求が permission.requested になった")
	}
	if _, _, err := s.EncodeCommand(resolveCommand(t, "over", v0.AllowOnce)); err == nil {
		t.Errorf("上限を超えた要求に、許可を返せた")
	}
}

// 未決の要求が多いときに、result (と control_cancel_request) の処理が、要求の数の 2 乗の時間にならない
// (agent が出す frame の数で、呼び手の goroutine を長時間止められない)。
func TestSettlingManyPendingRequestsIsNotQuadratic(t *testing.T) {
	measure := func(n int) time.Duration {
		s := Adapter{}.NewStream()
		for i := 0; i < n; i++ {
			if _, err := s.DecodeFrame(canUseToolFrame(fmt.Sprintf("f%d", i), `{}`)); err != nil {
				t.Fatal(err)
			}
		}
		start := time.Now()
		if _, err := s.DecodeFrame([]byte(`{"type":"result"}`)); err != nil {
			t.Fatal(err)
		}
		return time.Since(start)
	}
	small, large := measure(maxSeenRequests/4), measure(maxSeenRequests)
	t.Logf("result の処理: %d 件 %v・%d 件 %v", maxSeenRequests/4, small, maxSeenRequests, large)
	if large > 9*small+50*time.Millisecond { // 線形なら約 4 倍、2 乗なら約 16 倍
		t.Errorf("未決 %d 件の result の処理が %d 件の %.1f 倍かかった (2 乗の増え方)", maxSeenRequests, maxSeenRequests/4, float64(large)/float64(small))
	}
}
