package chat

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"unicode/utf8"

	v0 "github.com/nananek/goronation/spec/v0"
)

// 会話の状態機械の上限。
const (
	// MaxMessageBytes は、Send の text の上限 (バイト。web の maxRequestBody と同じ)。
	MaxMessageBytes = 64 << 10
	// MaxPendingRequests は、同時に人間の応答を待つ権限要求の数の上限。超えた要求は、claude に拒否を返し、by=policy で決着させる。
	// (agent/claude の未決の上限 64 より小さい。1 件は最大 DefaultMaxEvent なので、固定する Event のメモリ量が、この数で決まる。)
	MaxPendingRequests = 8
	// maxSettled は、応答済みの request_id を覚える数 (同じ ID への 2 回目の応答を、404 でなく 409 にするため)。古い方から忘れる。
	maxSettled = 4096
)

// 型付きのエラー。web は errors.Is で HTTP の status に対応づける。
var (
	ErrBusy            = errors.New("chat: 会話が idle でない (ターンの途中)")                 // Send: 409
	ErrClosed          = errors.New("chat: 会話は終了した (または終了の途中)")                    // Send: 409
	ErrEmptyText       = errors.New("chat: text が空")                               // Send: 400
	ErrTextTooLong     = errors.New("chat: text が上限を超えた")                          // Send: 413
	ErrBadText         = errors.New("chat: text が不正な UTF-8")                       // Send: 400
	ErrBadOutcome      = errors.New("chat: outcome は allow_once か reject_once だけ") // Resolve: 400
	ErrUnknownRequest  = errors.New("chat: 未決の request_id ではない (未知・失効)")           // Resolve: 404
	ErrNoGeneration    = errors.New("chat: 世代が無い")                                 // ResolveIn: 400
	ErrStaleGeneration = errors.New("chat: 別の起動 (世代) の画面からの応答")                    // ResolveIn: 409
	ErrAlreadyResolved = errors.New("chat: その request_id は、もう決着している")              // Resolve: 409
	ErrWriteFailed     = errors.New("chat: エージェントの入力に書けなかった (会話を終了した)")            // Send・Resolve: 500
	// ErrBadAnswer は、form の回答が、保持した要求に対して不正 (キー・型・選択肢・必須・長さ。FormResolve.Validate)。何も書かない。ResolveFormIn: 400。
	ErrBadAnswer = errors.New("chat: form の回答が、保持した要求に合わない")
	// ErrContentChanged は、応答の content_hash が、保持した要求の値と違う (利用者が見た内容と、保持した内容が違う。ADR 0042)。何も書かない。409。
	ErrContentChanged = errors.New("chat: 内容が、利用者が見たものと違う (content_hash が合わない)")
	// ErrContentHashRequired は、content_hash が必須 (ConversationConfig.RequireContentHash) なのに、応答に無い。400。
	ErrContentHashRequired = errors.New("chat: content_hash が要る")
)

// State は、会話の状態。
type State int

const (
	// StateIdle は、次の Send を待つ。
	StateIdle State = iota
	// StateTurn は、ターンの途中 (エージェントが動いている)。
	StateTurn
	// StateAwaitingPermission は、ターンの途中で、人間の応答を待つ権限要求または form がある (名前は、UI・API の互換のため、変えない)。
	StateAwaitingPermission
	// StateClosed は、終了した (または Stop / 書き込みの失敗で、もう指示を受けない)。
	StateClosed
)

// String は、状態の名前 ("idle"・"turn"・"awaiting_permission"・"closed")。
func (s State) String() string {
	return [...]string{"idle", "turn", "awaiting_permission", "closed"}[s]
}

// ConversationConfig は、Conversation の部品。
type ConversationConfig struct {
	Feed *Feed
	Hub  *Hub
	// Write は、エージェントの入力に、1 行 (改行を含む) を書く。Conversation の Mutex を持ったまま呼ばれるので、
	// 長く待たない (呼び手が、有界のキューに積むだけにする。詰まったら error を返す)。Conversation を呼び返してはいけない。
	// error は、ErrWriteFailed として扱い、会話を終了する。
	Write func(line []byte) error
	// RequireContentHash が true なら、応答 (permission.resolve・form.resolve) に content_hash が無いものを拒否する (ADR 0042 決定 3)。false (既定) は、
	// 導入の間の互換: 無ければ照合しない (あれば、必ず保持した値と照合する)。必須にする時期は、UI が content_hash を写すようになったあとの、別の判断。
	RequireContentHash bool
	// OnStop は、Stop (または書き込みの失敗) の最初の 1 回だけ、Mutex の外で呼ぶ (エージェントの入力を閉じ、檻を止める)。nil でもよい。
	OnStop func()
}

// Conversation は、1 回のエージェントの起動に紐づく、会話の状態機械 (書く側。純粋なロジック: プロセス・ネットワークに触れない)。
//
//	idle → (Send) → turn → (turn.completed) → idle。turn 中に、未決の権限要求があれば awaiting_permission。
//
// 出力の側 (OnLine) と、書く側 (Send・Resolve・Stop) を、Mutex で直列にする。並行に呼んでよい。
//
// 権限の承認・form の回答の束縛 (ADR 0009・0040・0042・0047):
//   - 要求 ID に束縛し、1 回だけ有効。クライアントが送れるのは request_id と outcome (allow_once・reject_once) だけ。form は outcome (answered・cancelled) と回答 (保持した form の
//     フィールドに対して検査する) で、種類の違う要求の ID には通らない。応答の content_hash は、保持した値と照合する (RequireContentHash でなければ、無いのは許す)。
//   - claude に返す許可の input は、要求時に Stream が保持した値だけから作る (ここでは input を持たず、触らない)。
//   - この会話 (1 回の起動) の未決の要求だけが有効。起動をまたぐ ID・失効した ID は、404。
//   - 終了・Stop で、未決の要求は全部失効する (by=policy・outcome=cancelled)。タイマーによる失効・自動停止は持たない。
//   - 同時に待つ要求 (権限・form) は MaxPendingRequests まで。超えた要求と、ターンの外に来た要求は、claude に拒否 (form は取り消し) を返して by=policy で決着させる。
//   - 未決の permission.requested・form.requested は Hub に固定し、リングから溢れても、新しい購読者の Snapshot に含める。
//
// 順序外れのフレーム: 未決でない ID の cancel は Stream が agent.frame にする。ターンの外の result (turn.completed) は、
// そのまま配るが、状態は変えない。未決を残した turn.completed・EOF (Close) は、未決を全部失効させる。
type Conversation struct {
	cfg ConversationConfig

	generation string // この起動 (会話) を区別する、ランダムな値 (L12)

	mu       sync.Mutex
	state    State // Idle・Turn・Closed (AwaitingPermission は、Turn と、未決の有無から導く)
	pending  map[string]pendingItem
	settled  map[[sha256.Size]byte]struct{}
	settledQ [][sha256.Size]byte
	// orphans は、会話が先に決着させたが、Stream がまだ未決として持つ request_id (Stop・書き込みの失敗・終了で失効させた分と、
	// 入力を閉じた後に自動拒否した分)。Stream の後追いの permission.resolved (by=agent) は、これに当たれば配らず、外す
	// (決着は要求ごとにちょうど 1 つ。spec/v0)。Stream の未決の上限 (64) が、この表の上限になる。
	// 長い request_id (最大 1 行) でメモリが増えないよう、ID は SHA-256 で持つ (settled と同じ)。
	orphans  map[[sha256.Size]byte]struct{}
	stopped  bool
	lastErr  string // 直前の error イベントの data (同じ error の連続を、まとめる)
	stopOnce sync.Once
}

// pendingItem は、人間の応答を待つ要求 (権限か form)。ev は配った Event (Seq で Hub の固定を外す)。hash は、Feed が付けた content_hash (応答の照合用)。
// form が nil でなければ form (回答の検査用に、保持した要求)。
type pendingItem struct {
	ev   Event
	hash string
	form *v0.FormRequested
}

// holdRequest は、permission.requested・form.requested の Event から、保持する値を読む。Feed が検査した形なので、読めなければ false (保持しない)。
func holdRequest(e Event) (pendingItem, bool) {
	switch e.Type {
	case v0.TypePermissionRequested:
		var v struct {
			Data v0.PermissionRequested `json:"data"`
		}
		if json.Unmarshal(e.JSON, &v) != nil {
			return pendingItem{}, false
		}
		return pendingItem{ev: e, hash: v.Data.ContentHash}, true
	case v0.TypeFormRequested:
		var v struct {
			Data v0.FormRequested `json:"data"`
		}
		if json.Unmarshal(e.JSON, &v) != nil {
			return pendingItem{}, false
		}
		return pendingItem{ev: e, hash: v.Data.ContentHash, form: &v.Data}, true
	}
	return pendingItem{}, false
}

// NewConversation は、idle の会話を作る。
func NewConversation(cfg ConversationConfig) *Conversation {
	var g [16]byte
	rand.Read(g[:])
	return &Conversation{cfg: cfg, generation: hex.EncodeToString(g[:]), pending: map[string]pendingItem{}, settled: map[[sha256.Size]byte]struct{}{}, orphans: map[[sha256.Size]byte]struct{}{}}
}

// State は、今の状態。
func (c *Conversation) State() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stateLocked()
}

func (c *Conversation) stateLocked() State {
	if c.state == StateTurn && len(c.pending) > 0 {
		return StateAwaitingPermission
	}
	return c.state
}

// Pending は、人間の応答を待つ権限要求の request_id の数。
func (c *Conversation) Pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pending)
}

// Send は、ユーザーの指示を、新しいターンとして送る。idle のときだけ受ける (queue・steer・中断は M2)。
func (c *Conversation) Send(text string) error {
	if text == "" {
		return ErrEmptyText
	}
	if len(text) > MaxMessageBytes {
		return ErrTextTooLong
	}
	if !utf8.ValidString(text) {
		return ErrBadText
	}
	data, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.state == StateClosed || c.stopped:
		return ErrClosed
	case c.state != StateIdle:
		return ErrBusy
	}
	raw, events, err := c.cfg.Feed.Encode(v0.Command{V: v0.Version, Type: v0.CommandPrompt, Data: data})
	if err != nil {
		return err
	}
	if err := c.cfg.Write(raw); err != nil {
		c.failLocked(err) // Stream は、書けたものとして状態を進めた (initialize 済みなど)。やり直さず、会話を終える
		return ErrWriteFailed
	}
	c.state = StateTurn
	c.cfg.Hub.Update(events, nil, nil)
	return nil
}

// Generation は、この起動 (会話) を区別する、ランダムな値。permission.requested を見せた画面が、応答に添える (ResolveIn) ことで、
// 起動をまたぐ承認の誤適用 (起動 1 の古い画面の allow が、起動 2 の同じ request_id の別の input に通る。L12) を断てる。
func (c *Conversation) Generation() string { return c.generation }

// ResolveIn は、未決の権限要求に、人間の応答 (allow_once・reject_once) を返す (content_hash なし。ResolvePermissionIn の、hash を持たない形)。generation がこの会話のものと一致するときだけ通す
// (空なら ErrNoGeneration、違えば ErrStaleGeneration。どちらも未決の表には触れない)。世代なしの経路は、型で無い (resolve は非公開。L2)。
func (c *Conversation) ResolveIn(generation, requestID, outcome string) error {
	return c.ResolvePermissionIn(generation, v0.PermissionResolve{RequestID: requestID, Outcome: outcome})
}

// ResolvePermissionIn は、ResolveIn に、content_hash の照合 (ADR 0042) を足したもの: r.ContentHash が、保持した要求の値と違えば ErrContentChanged、
// 無くて RequireContentHash なら ErrContentHashRequired。どちらも、何も書かず、未決のまま。form の request_id には通らない (ErrUnknownRequest)。
func (c *Conversation) ResolvePermissionIn(generation string, r v0.PermissionResolve) error {
	if generation == "" {
		return ErrNoGeneration
	}
	if generation != c.generation {
		return ErrStaleGeneration
	}
	return c.resolve(r.RequestID, r.Outcome, r.ContentHash)
}

// ResolveFormIn は、未決の form に、人間の回答 (answered・cancelled) を返す。世代の検査は ResolveIn と同じ。そのあと、未決の form でなければ ErrUnknownRequest /
// ErrAlreadyResolved、content_hash が合わなければ ErrContentChanged / ErrContentHashRequired、回答が、保持した要求に対して不正 (FormResolve.Validate) なら ErrBadAnswer。
// どれも、何も書かず、未決のまま。通れば、エージェントには {request_id, outcome, answer} だけを渡す (hash は渡さない)。権限の request_id には通らない。
func (c *Conversation) ResolveFormIn(generation, requestID string, r v0.FormResolve) error {
	if generation == "" {
		return ErrNoGeneration
	}
	if generation != c.generation {
		return ErrStaleGeneration
	}
	if r.RequestID != requestID {
		return ErrBadAnswer
	}
	if r.Outcome != v0.FormAnswered && r.Outcome != v0.FormCancelled {
		return ErrBadOutcome
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	item, err := c.heldLocked(requestID, true)
	if err != nil {
		return err
	}
	if err := c.verifyHash(item, r.ContentHash); err != nil {
		return err
	}
	if err := r.Validate(*item.form); err != nil {
		return ErrBadAnswer
	}
	data, err := json.Marshal(v0.FormResolve{RequestID: requestID, Outcome: r.Outcome, Answer: r.Answer}) // content_hash は、アダプタに渡さない
	if err != nil {
		return err
	}
	return c.writeResolveLocked(requestID, item, v0.Command{V: v0.Version, Type: v0.CommandFormResolve, Data: data})
}

// heldLocked は、未決の要求 requestID を返す。未決でなければ ErrUnknownRequest (決着済みなら ErrAlreadyResolved)。form が true なら form だけ、false なら権限だけ
// (種類の違う要求の ID は、未決でないものとして扱う: form に permission.resolve の allow を返すと、回答なしの許可になる)。
func (c *Conversation) heldLocked(requestID string, form bool) (pendingItem, error) {
	item, ok := c.pending[requestID]
	if !ok || (item.form != nil) != form {
		if _, done := c.settled[sha256.Sum256([]byte(requestID))]; !ok && done && c.state != StateClosed && !c.stopped {
			return pendingItem{}, ErrAlreadyResolved
		}
		return pendingItem{}, ErrUnknownRequest
	}
	return item, nil
}

// verifyHash は、応答の content_hash (got) を、保持した値と照合する (v0.VerifyHash)。
func (c *Conversation) verifyHash(item pendingItem, got string) error {
	switch err := v0.VerifyHash(item.hash, got, c.cfg.RequireContentHash); {
	case errors.Is(err, v0.ErrHashRequired):
		return ErrContentHashRequired
	case err != nil:
		return ErrContentChanged
	}
	return nil
}

// resolve は、ResolvePermissionIn の、世代を確かめた後の本体。
// 未決でない ID は ErrUnknownRequest、応答済みの ID は ErrAlreadyResolved (別タブの後追いなど)。並行に呼ばれても、1 つだけが通る。
func (c *Conversation) resolve(requestID, outcome, contentHash string) error {
	if outcome != v0.AllowOnce && outcome != v0.RejectOnce {
		return ErrBadOutcome
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	item, err := c.heldLocked(requestID, false)
	if err != nil {
		return err
	}
	if err := c.verifyHash(item, contentHash); err != nil {
		return err
	}
	data, err := json.Marshal(map[string]string{"request_id": requestID, "outcome": outcome})
	if err != nil {
		return err
	}
	return c.writeResolveLocked(requestID, item, v0.Command{V: v0.Version, Type: v0.CommandPermissionResolve, Data: data})
}

// writeResolveLocked は、検査を通った応答 (cmd) を、エージェントの入力に書き、要求を決着させる。
func (c *Conversation) writeResolveLocked(requestID string, item pendingItem, cmd v0.Command) error {
	raw, events, err := c.cfg.Feed.Encode(cmd)
	if err != nil {
		// Stream が、未決でない (または答えられない) と言った (表と食い違った)。表から外し、失効として扱う。
		c.settleLocked(requestID)
		c.cfg.Hub.Update(nil, nil, []uint64{item.ev.Seq})
		return ErrUnknownRequest
	}
	c.settleLocked(requestID)
	if err := c.cfg.Write(raw); err != nil {
		c.cfg.Hub.Update(nil, nil, []uint64{item.ev.Seq})
		c.emitResolvedLocked(requestID, item.form != nil, "cancelled")
		c.failLocked(err)
		return ErrWriteFailed
	}
	c.cfg.Hub.Update(events, nil, []uint64{item.ev.Seq})
	return nil
}

// Stop は、手動の「終了」。未決の要求を全部失効させ、以後の Send・ResolveIn を断り、OnStop (入力を閉じて檻を止める) を 1 回だけ呼ぶ。
// 何度呼んでもよい。出力は、エージェントが終わるまで OnLine で読み続け、終わったら Close する。
func (c *Conversation) Stop() {
	c.mu.Lock()
	if !c.stopped {
		c.stopped = true
		c.expireAllLocked()
	}
	c.mu.Unlock()
	if c.cfg.OnStop != nil {
		c.stopOnce.Do(c.cfg.OnStop)
	}
}

// Close は、エージェントの終了 (exit code つき) を記録する。未決の要求は全部失効させ、Hub を終了にする。何度呼んでもよい。
func (c *Conversation) Close(exit int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = StateClosed
	c.expireAllLocked()
	c.cfg.Hub.Close(exit)
}

// OnLine は、エージェントの出力の 1 行 (LineReader の返す行) を、変換して配り、状態を進める。
// 変換できない行 (Feed の error) は、何も配らず、その error を返す。
func (c *Conversation) OnLine(line []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	events, err := c.cfg.Feed.Decode(line)
	if err != nil {
		return err
	}
	var publish, pin []Event
	var unpin []uint64
	var rejects []heldID
	completed := false
	for _, e := range events {
		switch e.Type {
		case v0.TypeError:
			// 同じ error の連続 (上限超過の要求の洪水など) は、最初の 1 件だけを配る。
			if key := string(e.JSON[max(bytes.Index(e.JSON, []byte(`"data":`)), 0):]); key == c.lastErr {
				continue
			} else {
				c.lastErr = key
			}
		case v0.TypePermissionRequested, v0.TypeFormRequested:
			id, ok := requestID(e)
			item, held := holdRequest(e)
			if !ok || !held {
				continue // Feed が作る形ではない。捨てる
			}
			if c.state != StateTurn || c.stopped || len(c.pending) >= MaxPendingRequests {
				rejects = append(rejects, heldID{id, item.form != nil}) // 承認できない要求 (ターンの外・上限超過・終了の途中) は、見せたうえで、by=policy で拒否する (form は取り消す)
			} else {
				c.pending[id] = item
				pin = append(pin, e)
			}
		case v0.TypePermissionResolved, v0.TypeFormResolved:
			// claude の撤回・ターンの終了で、Stream が閉じた要求 (by=agent)。表から外す。
			if id, ok := requestID(e); ok {
				if req, held := c.pending[id]; held {
					c.settleLocked(id)
					unpin = append(unpin, req.ev.Seq)
				} else if _, orphan := c.orphans[sha256.Sum256([]byte(id))]; orphan {
					delete(c.orphans, sha256.Sum256([]byte(id)))
					continue // 会話が先に決着させた (Stop・書き込みの失敗・自動拒否)。決着は、要求ごとにちょうど 1 つ (spec/v0)
				}
			}
		case v0.TypeTurnCompleted:
			completed = true
		}
		if e.Type != v0.TypeError {
			c.lastErr = ""
		}
		publish = append(publish, e)
	}
	c.cfg.Hub.Update(publish, pin, unpin)
	if completed && c.state == StateTurn {
		c.state = StateIdle
		c.expireAllLocked() // 未決を残したターンの終わり。Stream が閉じたはずだが、表に残ったものは失効させる
	}
	for _, r := range rejects {
		c.autoRejectLocked(r)
	}
	return nil
}

// heldID は、承認・回答できない要求の ID と、form か。
type heldID struct {
	id   string
	form bool
}

// autoRejectLocked は、承認・回答できない要求を、エージェントに拒否 (権限は reject_once・form は cancelled。書けるなら) して、by=policy で決着させる。
func (c *Conversation) autoRejectLocked(r heldID) {
	id := r.id
	c.settleLocked(id)     // 画面に出た要求への、後追いの応答は、404 でなく 409
	outcome := "cancelled" // 入力を閉じた後は、エージェントに返せない
	if c.state == StateClosed || c.stopped {
		c.orphans[sha256.Sum256([]byte(id))] = struct{}{} // Stream は、まだ未決として持つ
	} else {
		cmd := v0.Command{V: v0.Version, Type: v0.CommandPermissionResolve}
		want := v0.RejectOnce
		if r.form {
			cmd.Type, want = v0.CommandFormResolve, v0.FormCancelled
		}
		data, err := json.Marshal(map[string]string{"request_id": id, "outcome": want})
		if err == nil {
			cmd.Data = data
			raw, err := c.cfg.Feed.EncodeQuiet(cmd)
			if err == nil {
				if werr := c.cfg.Write(raw); werr != nil {
					c.emitResolvedLocked(id, r.form, "cancelled") // 画面に出した要求には、書けなくても、決着を 1 つ付ける
					c.failLocked(werr)
					return
				}
				outcome = want
			}
		}
	}
	c.emitResolvedLocked(id, r.form, outcome)
}

// emitResolvedLocked は、by=policy の permission.resolved (form なら form.resolved) を配る。
func (c *Conversation) emitResolvedLocked(id string, form bool, outcome string) {
	typ := v0.TypePermissionResolved
	if form {
		typ = v0.TypeFormResolved
	}
	e, err := c.cfg.Feed.Emit(typ, true, map[string]string{"by": "policy", "outcome": outcome, "request_id": id})
	if err == nil {
		c.cfg.Hub.Update([]Event{e}, nil, nil)
	}
}

// expireAllLocked は、未決の要求を全部、by=policy・cancelled で失効させる (Seq 順)。
func (c *Conversation) expireAllLocked() {
	if len(c.pending) == 0 {
		return
	}
	ids := make([]string, 0, len(c.pending))
	for id := range c.pending {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return c.pending[ids[i]].ev.Seq < c.pending[ids[j]].ev.Seq })
	var out []Event
	var unpin []uint64
	for _, id := range ids {
		item := c.pending[id]
		unpin = append(unpin, item.ev.Seq)
		c.settleLocked(id)
		c.orphans[sha256.Sum256([]byte(id))] = struct{}{}
		typ := v0.TypePermissionResolved
		if item.form != nil {
			typ = v0.TypeFormResolved
		}
		if e, err := c.cfg.Feed.Emit(typ, true, map[string]string{"by": "policy", "outcome": "cancelled", "request_id": id}); err == nil {
			out = append(out, e)
		}
	}
	c.cfg.Hub.Update(out, nil, unpin)
}

// failLocked は、書き込みの失敗で、会話を終える (Hub は Close しない。エージェントの終了を、呼び手が Close で記録する)。
// 入力が壊れたエージェントを置き去りにしないよう、OnStop も (Mutex を離れた goroutine で、1 回だけ) 呼ぶ。
func (c *Conversation) failLocked(cause error) {
	c.state = StateClosed
	c.stopped = true
	if c.cfg.OnStop != nil {
		go c.stopOnce.Do(c.cfg.OnStop)
	}
	c.expireAllLocked()
	if e, err := c.cfg.Feed.Emit(v0.TypeError, true, map[string]any{"status": nil, "retryable": false, "message": "chat: エージェントの入力に書けなかった: " + cause.Error()}); err == nil {
		c.cfg.Hub.Update([]Event{e}, nil, nil)
	}
}

// settleLocked は、id を、未決の表から外し、応答済みとして覚える。
func (c *Conversation) settleLocked(id string) {
	delete(c.pending, id)
	k := sha256.Sum256([]byte(id))
	if _, ok := c.settled[k]; ok {
		return
	}
	if len(c.settledQ) >= maxSettled {
		delete(c.settled, c.settledQ[0])
		c.settledQ = c.settledQ[1:]
	}
	c.settled[k] = struct{}{}
	c.settledQ = append(c.settledQ, k)
}

// requestID は、permission・form の requested・resolved の Event から、data.request_id を読む。
func requestID(e Event) (string, bool) {
	var v struct {
		Data struct {
			RequestID string `json:"request_id"`
		} `json:"data"`
	}
	if json.Unmarshal(e.JSON, &v) != nil || v.Data.RequestID == "" {
		return "", false
	}
	return v.Data.RequestID, true
}
