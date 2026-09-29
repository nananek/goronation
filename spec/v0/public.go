package v0

import "encoding/json"

// UIEnvelope は、Envelope から Raw を除いた、UI・API に返す形。
type UIEnvelope struct {
	V       int             `json:"v"`
	ID      string          `json:"id"`
	TS      string          `json:"ts"`
	Session string          `json:"session"`
	Seq     uint64          `json:"seq"`
	Type    string          `json:"type"`
	Durable bool            `json:"durable"`
	Data    json.RawMessage `json:"data"`
}

// Public は、e を、UI・API に返す形にする。Raw は落ちる。
//
// 限界: Go の型システムは、Envelope を UI・API にそのまま返すこと自体を禁じられない。
// Envelope は公開の struct で、誰でも json.Marshal できる。この関数と UIEnvelope は、
// 「Raw を落とす正しい道」を 1 つ用意して、canary test (public_test.go) で押さえるだけで、誤用を防ぐ強制ではない。
// 強制は、UI・API の層が Envelope を import しないことを、tools/archtest の規則で押さえる形になる (M2 の SSE API の前に足す)。
func (e Envelope) Public() UIEnvelope {
	return UIEnvelope{V: e.V, ID: e.ID, TS: e.TS, Session: e.Session, Seq: e.Seq, Type: e.Type, Durable: e.Durable, Data: e.Data}
}
