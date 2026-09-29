package claude

import (
	"encoding/json"
	"fmt"
	"testing"

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

// 未決の数の上限を超えた要求は、承認できないが、黙って捨てず error で知らせる。決着すれば回復する。
func TestPendingLimitIsReportedAndRecovers(t *testing.T) {
	s := Adapter{}.NewStream()
	for i := 0; i < maxPendingRequests; i++ {
		if _, err := s.DecodeFrame(canUseToolFrame(fmt.Sprintf("f%d", i), `{}`)); err != nil {
			t.Fatal(err)
		}
	}
	envs, err := s.DecodeFrame(canUseToolFrame("over", `{}`))
	if err != nil {
		t.Fatal(err)
	}
	if envs[0].Type != v0.TypeError {
		t.Errorf("上限を超えた要求が error で知らされなかった: %s", envs[0].Type)
	}
	if _, _, err := s.EncodeCommand(resolveCommand(t, "over", v0.AllowOnce)); err == nil {
		t.Errorf("上限を超えた要求に、許可を返せた")
	}
	if _, _, err := s.EncodeCommand(resolveCommand(t, "f0", v0.RejectOnce)); err != nil {
		t.Fatal(err)
	}
	envs, err = s.DecodeFrame(canUseToolFrame("again", `{}`))
	if err != nil || envs[0].Type != v0.TypePermissionRequested {
		t.Errorf("決着させた後に、新しい要求が通らない: %v %v", envs, err)
	}
}

// result は、未決を届いた順に、by=agent・cancelled で閉じる (未決の数は maxPendingRequests で抑えられる)。
func TestResultClosesPendingInArrivalOrder(t *testing.T) {
	s := Adapter{}.NewStream()
	for i := 0; i < maxPendingRequests; i++ {
		if _, err := s.DecodeFrame(canUseToolFrame(fmt.Sprintf("f%d", i), `{}`)); err != nil {
			t.Fatal(err)
		}
	}
	envs, err := s.DecodeFrame([]byte(`{"type":"result"}`))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxPendingRequests; i++ {
		var d struct {
			RequestID string `json:"request_id"`
		}
		if envs[i].Type != v0.TypePermissionResolved || json.Unmarshal(envs[i].Data, &d) != nil || d.RequestID != fmt.Sprintf("f%d", i) {
			t.Fatalf("%d 番目が届いた順の cancelled でない: %s %s", i, envs[i].Type, envs[i].Data)
		}
	}
}
