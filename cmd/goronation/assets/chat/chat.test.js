'use strict';
// chat-core.js・chat.js の試験 (node:test)。cmd/goronation/chat_ui_test.go が起動する。GORO_CHAT_FIXTURES は、本物の変換器
// (goldens → chat.Session) が作った Event の JSON (1 行 1 Event) の置き場。
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');
const core = require('./chat-core.js');
const ui = require('./chat.js');

const cp = (n) => String.fromCodePoint(n);
const FIX = process.env.GORO_CHAT_FIXTURES || '';

function ev(seq, type, data) {
  return {v: 0, id: 'e' + seq, ts: '2026-01-01T00:00:00.000Z', session: '20260101-000000-abcdef', seq: seq, type: type, durable: true, data: data};
}
function fixture(name) {
  const f = path.join(FIX, name + '.jsonl');
  return fs.readFileSync(f, 'utf8').split('\n').filter(Boolean).map((l) => JSON.parse(l));
}
function stateWith(events, gen) {
  const s = core.createState();
  core.applyHello(s, {generation: gen || 'g1', first_seq: 0});
  for (const e of events) core.applyEvent(s, e);
  return s;
}

test('sanitize: 制御文字・双方向制御・行区切り・不可視を、見える印にする。通常の文字は残す', () => {
  const bad = [0x00, 0x07, 0x1b, 0x7f, 0x85, 0x061c, 0x200b, 0x200e, 0x200f, 0x2028, 0x2029, 0x202a, 0x202d, 0x202e, 0x2066, 0x2069, 0x2060, 0xfeff, 0xe0041];
  for (const c of bad) {
    const out = core.sanitize('a' + cp(c) + 'b');
    assert.ok(out.startsWith('a<U+') && out.endsWith('>b'), 'U+' + c.toString(16) + ' → ' + JSON.stringify(out));
    assert.ok(!out.includes(cp(c)));
  }
  assert.strictEqual(core.sanitize('a\nb\tc'), 'a\nb\tc');
  assert.strictEqual(core.sanitize('日本語 ok ' + cp(0x1f600)), '日本語 ok ' + cp(0x1f600));
  assert.ok(core.sanitize('x' + String.fromCharCode(0xd800) + 'y').includes('<U+D800>')); // 対になっていないサロゲート
  assert.ok(core.sanitize(String.fromCharCode(0xdc00)).includes('<U+DC00>'));
});

test('clip: 上限で切り、サロゲートの途中で切らず、元の長さを持つ。巨大な文字列でも速い', () => {
  const s = cp(0x1f600).repeat(50);
  const c = core.clip(s, 11); // 奇数: 絵文字の途中で切らない
  assert.strictEqual(c.cut, true);
  assert.strictEqual(c.total, 100);
  assert.ok(!/[\ud800-\udbff]$/.test(c.text));
  const big = 'a'.repeat(5 * 1024 * 1024);
  const t0 = Date.now();
  const cc = core.clip(big, core.LIMITS.maxText);
  assert.strictEqual(cc.text.length, core.LIMITS.maxText);
  assert.ok(Date.now() - t0 < 500);
  assert.strictEqual(core.clip({a: [1, 2]}, 1000).text, JSON.stringify({a: [1, 2]}, null, 2));
  assert.strictEqual(core.clip(null, 5).text, '');
});

test('golden から作った本物の Event: 表示の項目になる', () => {
  const s = stateWith(fixture('simple-text'));
  const kinds = s.items.map((i) => i.kind);
  assert.deepStrictEqual(kinds.filter((k) => k === 'user').length, 1);
  assert.ok(kinds.includes('session') && kinds.includes('assistant') && kinds.includes('turn_end') && kinds.includes('usage'), kinds.join());
  assert.strictEqual(s.items.find((i) => i.kind === 'user').text, 'hi');
  assert.ok(s.items.find((i) => i.kind === 'assistant').text.length > 0);
});

test('golden: tool.call と tool.update が、call_id で 1 枠になる', () => {
  const s = stateWith(fixture('tool-call'));
  const tools = s.items.filter((i) => i.kind === 'tool');
  assert.ok(tools.length >= 1);
  const t = tools[0];
  assert.ok(t.name.length > 0);
  assert.strictEqual(t.status, 'completed');
  assert.ok(t.output !== null || t.error !== null);
  assert.strictEqual(new Set(tools.map((x) => x.callId)).size, tools.length);
});

test('golden: 権限要求は permission の項目になり、全部の golden で、例外なく処理できる', () => {
  for (const f of fs.readdirSync(FIX).filter((n) => n.endsWith('.jsonl'))) {
    const s = stateWith(fixture(path.basename(f, '.jsonl')));
    assert.ok(s.items.length > 0, f);
  }
  const s = stateWith(fixture('permission-interactive-allow'));
  const p = s.items.filter((i) => i.kind === 'permission');
  assert.ok(p.length >= 1);
  assert.ok(p[0].requestId.length > 0 && p[0].toolName.length > 0);
});

test('未知の type・agent.frame・不正な形は、落ちずに無視する', () => {
  const s = core.createState();
  core.applyHello(s, {generation: 'g', first_seq: 0});
  const junk = [null, 5, 'x', [], {}, {type: 5, seq: 1}, {type: 'x', seq: -1}, {type: 'x', seq: 1.5}, ev(1, 'agent.frame', {}), ev(2, 'future.type', {a: 1}),
    ev(3, 'message.text', null), ev(4, 'message.text', 'str'), ev(5, 'tool.call', []), ev(6, 'usage', {input_tokens: 'x'}), ev(7, 'error', {status: 'a', message: {x: 1}})];
  for (const j of junk) core.applyEvent(s, j);
  assert.ok(s.ignored >= 6);
  assert.ok(Array.isArray(s.items));
});

test('敵対的な文字列は、項目の text に、そのまま (印つきで) 残り、構造を作らない', () => {
  const evil = '<script>alert(1)</script><img src=x onerror=alert(2)>' + cp(0x202e) + 'txet' + cp(0x2066) + '\u0000';
  const s = stateWith([ev(0, 'message.text', {text: evil}), ev(1, 'turn.started', {text: evil}), ev(2, 'permission.requested', {request_id: 'r', tool_name: evil, title: evil, input: {command: evil}})]);
  for (const it of s.items) {
    const all = JSON.stringify(it);
    assert.ok(!all.includes(cp(0x202e)) && !all.includes(cp(0x2066)), '双方向制御が残った');
  }
  assert.ok(s.items[0].text.includes('<script>')); // 文字としては残る (textContent で描く。HTML としては解釈されない)
});

test('繋ぎ直し: 同じ世代の Snapshot の重複は捨て、世代が変われば作り直す', () => {
  const evs = [];
  for (let i = 0; i < 10; i++) evs.push(ev(i, 'message.text', {text: 'm' + i}));
  const s = stateWith(evs, 'g1');
  assert.strictEqual(s.items.length, 10);
  core.applyHello(s, {generation: 'g1', first_seq: 0}); // 同じ世代で繋ぎ直し
  for (const e of evs) core.applyEvent(s, e);
  assert.strictEqual(s.items.length, 10);
  core.applyEvent(s, ev(10, 'message.text', {text: 'new'}));
  assert.strictEqual(s.items.length, 11);
  const r = core.applyHello(s, {generation: 'g2', first_seq: 0}); // serve が再起動した: seq が 0 から振り直される
  assert.strictEqual(r.reset, true);
  assert.strictEqual(s.items.length, 0);
  core.applyEvent(s, ev(0, 'message.text', {text: 'after restart'}));
  assert.strictEqual(s.items.length, 1);
  const d = core.drain(s);
  assert.strictEqual(d.reset, true);
  assert.strictEqual(d.upserts.length, 1);
});

test('hello.first_seq > 0 で、古い分は省略。end で終了', () => {
  const s = core.createState();
  core.applyHello(s, {generation: 'g', first_seq: 40});
  assert.strictEqual(s.omitted, true);
  core.applyEnd(s, {exit: 3});
  assert.deepStrictEqual(s.ended, {exit: 3});
});

test('対応する要求の無い permission.resolved は、無視する。要求は決着で状態が変わる', () => {
  const s = stateWith([ev(0, 'permission.resolved', {request_id: 'gone', by: 'agent', outcome: 'cancelled'})]);
  assert.strictEqual(s.items.length, 0);
  core.applyEvent(s, ev(1, 'permission.requested', {request_id: 'r1', tool_name: 'Write', input: {a: 1}}));
  assert.strictEqual(s.items[0].state, 'pending');
  core.applyEvent(s, ev(2, 'permission.resolved', {request_id: 'r1', by: 'human', outcome: 'allow_once'}));
  assert.strictEqual(s.items[0].state, 'allow_once');
  assert.strictEqual(s.items[0].by, 'human');
  core.applyEvent(s, ev(3, 'permission.requested', {request_id: 'r1', tool_name: 'Other'})); // 同じ ID の再要求は、上書きしない
  assert.strictEqual(s.items[0].toolName, 'Write');
});

test('tool.update の呼び出しが省略されていても、落ちない', () => {
  const s = stateWith([ev(0, 'tool.update', {call_id: 'x', status: 'completed', output: 'ok'})]);
  assert.strictEqual(s.items.length, 1);
  assert.strictEqual(s.items[0].name, '(履歴から省略)');
});

test('数十万イベント: 項目は上限で止まり、速く、未決の権限要求は捨てない', () => {
  const s = core.createState();
  core.applyHello(s, {generation: 'g', first_seq: 0});
  core.applyEvent(s, ev(0, 'permission.requested', {request_id: 'keep', tool_name: 'Bash', input: {command: 'x'}}));
  const t0 = Date.now();
  const N = 300000;
  for (let i = 1; i <= N; i++) core.applyEvent(s, ev(i, i % 3 === 0 ? 'tool.call' : 'message.text', {text: 'x'.repeat(50), call_id: 'c' + i, name: 'T', input: {i: i}}));
  const ms = Date.now() - t0;
  assert.ok(s.items.length <= core.LIMITS.maxItems + 260, 'items=' + s.items.length);
  assert.ok(s.byCall.size <= core.LIMITS.maxItems + 260, 'byCall=' + s.byCall.size);
  assert.ok(s.items.some((i) => i.kind === 'permission' && i.requestId === 'keep'));
  assert.strictEqual(s.trimmed, true);
  assert.ok(ms < 15000, ms + 'ms');
  const d = core.drain(s);
  assert.ok(d.upserts.length <= core.LIMITS.maxItems + 260);
});

test('巨大テキスト: 上限で切り、切ったことが分かる', () => {
  const s = stateWith([ev(0, 'message.text', {text: 'あ'.repeat(3 * 1024 * 1024)}), ev(1, 'permission.requested', {request_id: 'r', tool_name: 't', input: {c: 'y'.repeat(1 << 20)}})]);
  assert.strictEqual(s.items[0].cut, true);
  assert.ok(s.items[0].text.length <= core.LIMITS.maxText);
  assert.strictEqual(s.items[1].input.cut, true); // 全部は表示できない (PR⑦b は、Approve を無効にする)
});

// ---- chat.js: 偽の DOM・EventSource ----

class FakeEl {
  constructor(tag, doc) {
    this.tagName = tag;
    this.doc = doc;
    this.children = [];
    this.className = '';
    this._text = '';
    this.listeners = {};
    this.scrollTop = 0;
    this.scrollHeight = 0;
    this.clientHeight = 0;
  }
  set textContent(v) {
    this.children = [];
    this._text = String(v);
  }
  get textContent() {
    return this._text + this.children.map((c) => c.textContent).join('');
  }
  set innerHTML(v) { throw new Error('innerHTML を使った'); }
  get innerHTML() { throw new Error('innerHTML を使った'); }
  set outerHTML(v) { throw new Error('outerHTML を使った'); }
  appendChild(c) { this.children.push(c); return c; }
  removeChild(c) {
    const i = this.children.indexOf(c);
    if (i < 0) throw new Error('子ではない');
    this.children.splice(i, 1);
    return c;
  }
  addEventListener(n, f) { (this.listeners[n] = this.listeners[n] || []).push(f); }
}
class FakeDoc {
  constructor() {
    this.created = [];
    this.byId = {};
    for (const id of ['log', 'status', 'notices']) this.byId[id] = new FakeEl('div', this);
  }
  createElement(tag) { this.created.push(tag); return new FakeEl(tag, this); }
  getElementById(id) { return this.byId[id]; }
}
class FakeES {
  constructor(url) { this.url = url; this.closed = false; this.l = {}; FakeES.all.push(this); }
  addEventListener(n, f) { (this.l[n] = this.l[n] || []).push(f); }
  close() { this.closed = true; }
  fire(n, data) { for (const f of this.l[n] || []) f({data: typeof data === 'string' ? data : JSON.stringify(data)}); }
}
FakeES.all = [];

function harness() {
  FakeES.all = [];
  const doc = new FakeDoc();
  const timers = [];
  const env = {document: doc, EventSource: FakeES, setTimeout: (f, ms) => { timers.push({f, ms}); return timers.length; }, scroller: null};
  const app = ui.create(env);
  app.start('/s/20260101-000000-abcdef/events');
  return {doc, timers, app, es: () => FakeES.all[FakeES.all.length - 1],
    runTimers() { const t = timers.splice(0); for (const x of t) x.f(); }};
}

test('UI: hello・イベント・end。描画は textContent と createElement だけ。end で再接続しない', () => {
  const h = harness();
  h.es().fire('hello', {first_seq: 0, generation: 'g1'});
  const evil = '<script>alert(1)</script><img src=x onerror=alert(2)>';
  h.es().fire('message', ev(0, 'turn.started', {text: evil}));
  h.es().fire('message', ev(1, 'message.text', {text: evil}));
  h.es().fire('message', ev(2, 'tool.call', {call_id: 'c', name: evil, kind: 'execute', input: {command: evil}, status: 'in_progress'}));
  h.es().fire('message', ev(3, 'tool.update', {call_id: 'c', status: 'completed', output: evil}));
  h.es().fire('message', ev(4, 'permission.requested', {request_id: 'r', tool_name: 'Bash', title: evil, input: {command: evil}}));
  h.runTimers();
  const text = h.doc.byId.log.textContent;
  assert.ok(text.includes(evil), '敵対的な文字列が、文字として表示されていない');
  for (const bad of ['script', 'img', 'iframe', 'a', 'style', 'link', 'object', 'embed', 'svg']) assert.ok(!h.doc.created.includes(bad), bad + ' の要素を作った');
  assert.strictEqual(h.doc.byId.status.textContent, '接続済み');
  const last = h.es();
  last.fire('end', {exit: 0});
  h.runTimers();
  assert.strictEqual(last.closed, true, 'end で、接続を閉じていない (EventSource が、自動で再接続する)');
  assert.strictEqual(h.doc.byId.status.textContent, '終了');
  assert.ok(h.doc.byId.notices.textContent.includes('終了'));
  const n = FakeES.all.length;
  h.es().fire('error', {});
  h.runTimers();
  assert.strictEqual(FakeES.all.length, n, 'end の後に、再接続した');
});

test('UI: ping (コメント) は届かない。繋ぎ直しで、同じ Event が重複して積まれない', () => {
  const h = harness();
  h.es().fire('hello', {first_seq: 0, generation: 'g1'});
  for (let i = 0; i < 5; i++) h.es().fire('message', ev(i, 'message.text', {text: 'm' + i}));
  h.runTimers();
  assert.strictEqual(h.doc.byId.log.children.length, 5);
  h.es().fire('error', {}); // 素の EOF (上流のフレーミングが破れた)
  h.runTimers();
  const es2 = h.es();
  assert.strictEqual(FakeES.all.length, 2);
  es2.fire('hello', {first_seq: 0, generation: 'g1'});
  for (let i = 0; i < 8; i++) es2.fire('message', ev(i, 'message.text', {text: 'm' + i}));
  h.runTimers();
  assert.strictEqual(h.doc.byId.log.children.length, 8);
});

test('UI: 世代が変わると (serve の再起動)、表示を作り直す', () => {
  const h = harness();
  h.es().fire('hello', {first_seq: 0, generation: 'g1'});
  for (let i = 0; i < 5; i++) h.es().fire('message', ev(i, 'message.text', {text: 'old' + i}));
  h.runTimers();
  h.es().fire('error', {});
  h.runTimers();
  h.es().fire('hello', {first_seq: 0, generation: 'g2'});
  h.es().fire('message', ev(0, 'message.text', {text: 'new'}));
  h.runTimers();
  assert.strictEqual(h.doc.byId.log.children.length, 1);
  assert.ok(h.doc.byId.log.textContent.includes('new') && !h.doc.byId.log.textContent.includes('old'));
});

test('UI: 切断で、間隔を倍々に空けて再接続し (上限 30 秒)、hello で戻り、続けて失敗したら諦めてボタンを出す', () => {
  const h = harness();
  const delays = [];
  for (let i = 0; i < ui.GIVE_UP_AFTER; i++) {
    h.es().fire('error', {});
    const t = h.timers[h.timers.length - 1];
    if (i < ui.GIVE_UP_AFTER - 1) {
      delays.push(t.ms);
      h.runTimers();
    }
  }
  assert.deepStrictEqual(delays.slice(0, 5), [1000, 2000, 4000, 8000, 16000]);
  assert.ok(delays.every((d) => d <= ui.RETRY_MAX_MS));
  assert.ok(delays.includes(30000));
  // 諦めた: 再接続のタイマーは無く、ボタンがある。
  assert.ok(h.doc.byId.notices.textContent.includes('再接続'));
  const n = FakeES.all.length;
  h.runTimers();
  assert.strictEqual(FakeES.all.length, n);
  // ボタンで再開し、hello で失敗の数が戻る。
  const btn = h.doc.byId.notices.children.find((c) => c.tagName === 'button');
  btn.listeners.click[0]();
  assert.strictEqual(FakeES.all.length, n + 1);
  h.es().fire('hello', {first_seq: 0, generation: 'g1'});
  h.es().fire('error', {});
  assert.strictEqual(h.timers[h.timers.length - 1].ms, 1000); // hello で、間隔が戻った
});

test('UI: 古い接続の Event は、無視する', () => {
  const h = harness();
  const old = h.es();
  old.fire('hello', {first_seq: 0, generation: 'g1'});
  old.fire('error', {});
  h.runTimers();
  const cur = h.es();
  assert.notStrictEqual(old, cur);
  old.fire('message', ev(0, 'message.text', {text: 'from old'}));
  h.runTimers();
  assert.strictEqual(h.doc.byId.log.children.length, 0);
});

test('UI: 数万イベントでも、DOM の要素は上限で止まる', () => {
  const h = harness();
  h.es().fire('hello', {first_seq: 0, generation: 'g1'});
  for (let i = 0; i < 20000; i++) h.es().fire('message', ev(i, 'message.text', {text: 'x' + i}));
  h.runTimers();
  assert.ok(h.doc.byId.log.children.length <= core.LIMITS.maxItems + 260, 'nodes=' + h.doc.byId.log.children.length);
  assert.ok(h.doc.byId.notices.textContent.includes('省略'));
});
