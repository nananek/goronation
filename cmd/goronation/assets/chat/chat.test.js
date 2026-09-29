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
const G1 = '0123456789abcdef0123456789abcdef'; // serve の世代 (32 桁の 16 進)
const G2 = 'fedcba9876543210fedcba9876543210';

function ev(seq, type, data) {
  return {v: 0, id: 'e' + seq, ts: '2026-01-01T00:00:00.000Z', session: '20260101-000000-abcdef', seq: seq, type: type, durable: true, data: data};
}
function fixture(name) {
  const f = path.join(FIX, name + '.jsonl');
  return fs.readFileSync(f, 'utf8').split('\n').filter(Boolean).map((l) => JSON.parse(l));
}
function stateWith(events, gen) {
  const s = core.createState();
  core.applyHello(s, {generation: gen || G1, first_seq: 0});
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
  core.applyHello(s, {generation: G1, first_seq: 0});
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
  const s = stateWith(evs, G1);
  assert.strictEqual(s.items.length, 10);
  core.applyHello(s, {generation: G1, first_seq: 0}); // 同じ世代で繋ぎ直し
  for (const e of evs) core.applyEvent(s, e);
  assert.strictEqual(s.items.length, 10);
  core.applyEvent(s, ev(10, 'message.text', {text: 'new'}));
  assert.strictEqual(s.items.length, 11);
  const r = core.applyHello(s, {generation: G2, first_seq: 0}); // serve が再起動した: seq が 0 から振り直される
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
  core.applyHello(s, {generation: G1, first_seq: 40});
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
  core.applyHello(s, {generation: G1, first_seq: 0});
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
    this.disabled = false;
    this.value = '';
    this.open = false;
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
    for (const [id, tag] of [['log', 'main'], ['status', 'span'], ['notices', 'div'], ['dialogs', 'div'], ['dialogs-head', 'div'], ['msg', 'textarea'], ['send', 'button'], ['stop', 'button'], ['write-status', 'span']]) this.byId[id] = new FakeEl(tag, this);
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

const BASE = '/s/20260101-000000-abcdef';

function harness() {
  FakeES.all = [];
  const doc = new FakeDoc();
  const timers = [];
  const calls = []; // fetch の呼び出し: {url, opts, body, respond(status)}
  const env = {
    document: doc, EventSource: FakeES, setTimeout: (f, ms) => { timers.push({f, ms}); return timers.length; }, scroller: null,
    fetch: (url, opts) => new Promise((resolve, reject) => {
      calls.push({url, opts, body: JSON.parse(opts.body), respond: (status) => resolve({status}), fail: () => reject(new Error('net'))});
    }),
    TextEncoder: TextEncoder,
  };
  const app = ui.create(env);
  app.start(BASE + '/events', BASE);
  return {doc, timers, calls, app, es: () => FakeES.all[FakeES.all.length - 1],
    runTimers() { const t = timers.splice(0); for (const x of t) x.f(); },
    // hello を受けて、描画まで進める。
    hello(gen) { this.es().fire('hello', {first_seq: 0, generation: gen || G1}); this.runTimers(); },
    fire(e) { this.es().fire('message', e); },
    click(btn) { for (const f of btn.listeners.click || []) f(); },
    async tick() { await new Promise((r) => setImmediate(r)); }};
}

test('UI: hello・イベント・end。描画は textContent と createElement だけ。end で再接続しない', () => {
  const h = harness();
  h.es().fire('hello', {first_seq: 0, generation: G1});
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
  h.es().fire('hello', {first_seq: 0, generation: G1});
  for (let i = 0; i < 5; i++) h.es().fire('message', ev(i, 'message.text', {text: 'm' + i}));
  h.runTimers();
  assert.strictEqual(h.doc.byId.log.children.length, 5);
  h.es().fire('error', {}); // 素の EOF (上流のフレーミングが破れた)
  h.runTimers();
  const es2 = h.es();
  assert.strictEqual(FakeES.all.length, 2);
  es2.fire('hello', {first_seq: 0, generation: G1});
  for (let i = 0; i < 8; i++) es2.fire('message', ev(i, 'message.text', {text: 'm' + i}));
  h.runTimers();
  assert.strictEqual(h.doc.byId.log.children.length, 8);
});

test('UI: 世代が変わると (serve の再起動)、表示を作り直す', () => {
  const h = harness();
  h.es().fire('hello', {first_seq: 0, generation: G1});
  for (let i = 0; i < 5; i++) h.es().fire('message', ev(i, 'message.text', {text: 'old' + i}));
  h.runTimers();
  h.es().fire('error', {});
  h.runTimers();
  h.es().fire('hello', {first_seq: 0, generation: G2});
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
  h.es().fire('hello', {first_seq: 0, generation: G1});
  h.es().fire('error', {});
  assert.strictEqual(h.timers[h.timers.length - 1].ms, 1000); // hello で、間隔が戻った
});

test('UI: 古い接続の Event は、無視する', () => {
  const h = harness();
  const old = h.es();
  old.fire('hello', {first_seq: 0, generation: G1});
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
  h.es().fire('hello', {first_seq: 0, generation: G1});
  for (let i = 0; i < 20000; i++) h.es().fire('message', ev(i, 'message.text', {text: 'x' + i}));
  h.runTimers();
  assert.ok(h.doc.byId.log.children.length <= core.LIMITS.maxItems + 260, 'nodes=' + h.doc.byId.log.children.length);
  assert.ok(h.doc.byId.notices.textContent.includes('省略'));
});

// 決着した権限要求が、上限 (maxPending) の数に数えられ、その後の、新しい未決の要求が、表示されなくなる (承認できない)。
// エージェント自身が、要求と撤回を繰り返すだけで、後の本物の要求を隠せる。上限は、未決の数にだけ効く。
test('決着した権限要求が 64 件たまっても、新しい未決の要求は表示される', () => {
  const s = core.createState();
  core.applyHello(s, {generation: G1, first_seq: 0});
  let seq = 0;
  for (let i = 0; i < core.LIMITS.maxPending + 6; i++) {
    core.applyEvent(s, ev(seq++, 'permission.requested', {request_id: 'old' + i, tool_name: 'Bash', input: {}}));
    core.applyEvent(s, ev(seq++, 'permission.resolved', {request_id: 'old' + i, outcome: 'cancelled', by: 'agent'}));
  }
  assert.strictEqual(core.applyEvent(s, ev(seq++, 'permission.requested', {request_id: 'new', tool_name: 'Bash', input: {command: 'x'}})), true);
  const it = s.items.find((i) => i.kind === 'permission' && i.requestId === 'new');
  assert.ok(it && it.state === 'pending', '新しい未決の要求が、表示されない');
});

// ---- レビュー (PR⑦a) で見つかった点 ----

test('B1: 上限は、未決の数にだけ効く。未決が上限に達したら、それ以上は出さず、重複した request_id は無視する', () => {
  const s = core.createState();
  core.applyHello(s, {generation: G1, first_seq: 0});
  let seq = 0;
  for (let i = 0; i < core.LIMITS.maxPending; i++) assert.strictEqual(core.applyEvent(s, ev(seq++, 'permission.requested', {request_id: 'p' + i, tool_name: 'T', input: {}})), true);
  assert.strictEqual(core.applyEvent(s, ev(seq++, 'permission.requested', {request_id: 'over', tool_name: 'T', input: {}})), false); // 未決が上限
  assert.strictEqual(core.applyEvent(s, ev(seq++, 'permission.requested', {request_id: 'p0', tool_name: 'Dup', input: {}})), false); // 重複
  core.applyEvent(s, ev(seq++, 'permission.resolved', {request_id: 'p0', outcome: 'allow_once', by: 'human'}));
  assert.strictEqual(core.applyEvent(s, ev(seq++, 'permission.requested', {request_id: 'after', tool_name: 'T', input: {}})), true); // 決着で、1 つ空く
  assert.strictEqual(s.items.find((i) => i.requestId === 'p0').toolName, 'T');
});

test('N-A: outcome は許可リスト。決着済みが、未決 (pending) に見えず、固定の対象にもならない', () => {
  const s = stateWith([ev(0, 'permission.requested', {request_id: 'r', tool_name: 'T', input: {}}), ev(1, 'permission.resolved', {request_id: 'r', outcome: 'pending', by: 'agent'})]);
  assert.strictEqual(s.items[0].state, 'unknown');
  for (const [i, oc] of ['allow_once', 'reject_once', 'cancelled'].entries()) {
    core.applyEvent(s, ev(10 + i * 2, 'permission.requested', {request_id: 'k' + i, tool_name: 'T', input: {}}));
    core.applyEvent(s, ev(11 + i * 2, 'permission.resolved', {request_id: 'k' + i, outcome: oc, by: 'human'}));
    assert.strictEqual(s.items.find((x) => x.requestId === 'k' + i).state, oc);
  }
  for (const [i, bad] of [null, 5, {}, '__proto__', 'allow', 'ALLOW_ONCE', 'pending'].entries()) {
    core.applyEvent(s, ev(100 + i * 2, 'permission.requested', {request_id: 'z' + i, tool_name: 'T', input: {}}));
    core.applyEvent(s, ev(101 + i * 2, 'permission.resolved', {request_id: 'z' + i, outcome: bad}));
    assert.strictEqual(s.items.find((x) => x.requestId === 'z' + i).state, 'unknown', JSON.stringify(bad));
  }
});

test('N-B: 世代の無い・壊れた hello は受けない (世代を承認に使うため)', () => {
  const s = core.createState();
  for (const bad of [{}, {generation: null}, {generation: ''}, {generation: 'g1'}, {generation: 5}, {generation: G1.toUpperCase()}, {generation: G1 + 'a'}, null, 'x']) {
    assert.strictEqual(core.applyHello(s, bad).ok, false, JSON.stringify(bad));
    assert.strictEqual(s.generation, null);
  }
  assert.strictEqual(core.applyHello(s, {generation: G1, first_seq: 0}).ok, true);
  assert.strictEqual(s.generation, G1);
});

test('N-B: UI は、世代の無い hello の接続を使わず、閉じて、間隔を空けて繋ぎ直す', () => {
  const h = harness();
  h.es().fire('hello', {first_seq: 0});
  assert.ok(h.es().closed);
  assert.notStrictEqual(h.doc.byId.status.textContent, '接続済み');
  assert.ok(h.timers.length >= 1);
  assert.strictEqual(h.app.state.generation, null);
});

test('L-B: 短い項目は、空白に見える文字・改行・タブ・異体字選択子を、印にする。長い本文は、全角空白・絵文字の選択子を残す', () => {
  const invisibles = [0x3164, 0x115f, 0xffa0, 0x2800, 0x00ad, 0x034f, 0x17b4, 0xfffc, 0xfe0f, 0xe0100, 0x180b, 0x202f, 0x2003, 0x3000, 0x00a0, 0x0a, 0x09];
  for (const c of invisibles) {
    const s = stateWith([ev(0, 'permission.requested', {request_id: 'r', tool_name: cp(c), title: 'a' + cp(c) + 'b', input: {}})]);
    const p = s.items[0];
    assert.ok(/^<U\+[0-9A-F]{4,6}>$/.test(p.toolName), 'U+' + c.toString(16) + ' → ' + JSON.stringify(p.toolName));
    assert.ok(!p.title.includes(cp(c)));
  }
  // 名前が、見えない文字だけでも、印が見える (空白に見えない)。
  const nm = stateWith([ev(0, 'tool.call', {call_id: 'c', name: cp(0x3164) + cp(0x2800), kind: 'other', status: 'in_progress', input: {}})]).items[0].name;
  assert.ok(nm.length > 0 && nm.includes('<U+3164>') && nm.includes('<U+2800>'));
  const text = '日本語' + cp(0x3000) + 'の文 ' + cp(0x2764) + cp(0xfe0f) + ' ok';
  assert.strictEqual(core.sanitize(text), text); // 長い本文 (既定) は、そのまま
  for (const c of [0x3164, 0x2800, 0x00ad, 0xfffc, 0xe0100, 0x034f]) assert.ok(core.sanitize('a' + cp(c)).includes('<U+'), 'U+' + c.toString(16));
});

test('T-2: 状態・種類が、class を注入できない (項目の見た目を偽造できない)', () => {
  const h = harness();
  h.es().fire('hello', {first_seq: 0, generation: G1});
  h.es().fire('message', ev(0, 'tool.call', {call_id: 'c', name: 'T', kind: 'other', status: 'completed item-permission state-pending', input: {}}));
  h.es().fire('message', ev(1, 'permission.requested', {request_id: 'r', tool_name: 'T', input: {}}));
  h.es().fire('message', ev(2, 'permission.resolved', {request_id: 'r', outcome: 'cancelled item-error', by: 'x'}));
  h.runTimers();
  const kids = h.doc.byId.log.children;
  assert.ok(!/item-permission|state-pending/.test(kids[0].className), kids[0].className);
  assert.ok(!/item-error/.test(kids[1].className), kids[1].className);
});

test('T-4: usage の値は、数だけ。文字列は入らない', () => {
  const s = stateWith([ev(0, 'usage', {input_tokens: '<b>x</b>', output_tokens: 5, cost_usd: '$$', context_window: null})]);
  assert.strictEqual(s.items[0].inputTokens, null);
  assert.strictEqual(s.items[0].outputTokens, 5);
  assert.strictEqual(s.items[0].costUSD, null);
});

test('T-1: 描画を挟みながら流しても、DOM の要素は、実際に上限で止まる (外し漏れで増え続けない)', () => {
  const h = harness();
  h.es().fire('hello', {first_seq: 0, generation: G1});
  const N = 60000;
  let peak = 0;
  for (let i = 0; i < N; i++) {
    h.es().fire('message', ev(i, 'message.text', {text: 'x' + i}));
    if (i % 500 === 499) { // 50ms ごとの描画が、途中に入る (実ブラウザと同じ)
      h.runTimers();
      peak = Math.max(peak, h.doc.byId.log.children.length);
    }
  }
  h.runTimers();
  const n = h.doc.byId.log.children.length;
  assert.ok(n <= core.LIMITS.maxItems + 260 + 1, 'nodes=' + n);
  assert.ok(peak <= core.LIMITS.maxItems + 260 + 1, 'peak=' + peak);
  assert.strictEqual(h.app.elements.size, n, '要素の表と、DOM の子の数が食い違う');
});

// ---- PR⑦b: 送信欄・終了・権限ダイアログ ----


const dialogEls = (h) => h.doc.byId.dialogs.children;
function findBtn(dlg, cls) {
  const row = dlg.children.find((c) => c.className === 'dialog-buttons');
  return row.children.find((c) => c.className === cls);
}
function pendingReq(rid, extra) {
  return Object.assign({request_id: rid, tool_name: 'Write', title: 'new.txt', input: {file_path: '/work/new.txt'}}, extra || {});
}

test('送信欄: hello 前・ターン中・終了後・空は無効。送ると、指示だけの JSON を、固定の URL へ。turn.started まで次の送信を止める', async () => {
  const h = harness();
  const {send, msg} = {send: h.doc.byId.send, msg: h.doc.byId.msg};
  assert.strictEqual(send.disabled, true, 'hello 前に有効');
  h.hello();
  assert.strictEqual(send.disabled, true, '空で有効');
  msg.value = '  ';
  h.click(send);
  assert.strictEqual(h.calls.length, 0);
  msg.value = 'こんにちは';
  msg.listeners.input[0]();
  assert.strictEqual(send.disabled, false);
  h.click(send);
  assert.strictEqual(send.disabled, true, '送信中に有効');
  assert.strictEqual(h.calls.length, 1);
  assert.strictEqual(h.calls[0].url, BASE + '/message');
  assert.strictEqual(h.calls[0].opts.method, 'POST');
  assert.strictEqual(h.calls[0].opts.headers['Content-Type'], 'application/json');
  assert.deepStrictEqual(h.calls[0].body, {text: 'こんにちは'});
  h.calls[0].respond(200);
  await h.tick();
  assert.strictEqual(msg.value, '', '送れたのに、入力欄が残った');
  msg.value = 'next';
  msg.listeners.input[0]();
  assert.strictEqual(send.disabled, true, 'turn.started の前に、次の送信が有効');
  h.fire(ev(0, 'turn.started', {text: 'こんにちは'}));
  h.runTimers();
  assert.strictEqual(send.disabled, true, 'ターン中に有効');
  h.fire(ev(1, 'turn.completed', {stop_reason: 'end_turn', is_error: false}));
  h.runTimers();
  assert.strictEqual(send.disabled, false, 'ターンが終わっても無効');
  h.es().fire('end', {exit: 0});
  h.runTimers();
  assert.strictEqual(send.disabled, true, '終了後に有効');
  assert.strictEqual(msg.disabled, true);
});

test('送信欄: 失敗の status を画面に出し、入力は消さず、また送れる。大きすぎる指示は送らない', async () => {
  const h = harness();
  h.hello();
  const msg = h.doc.byId.msg;
  const ws = h.doc.byId.write_status || h.doc.byId['write-status'];
  for (const [status, word] of [[409, '状態が合わない'], [403, '拒否された'], [404, '起動していない'], [400, '不正'], [413, '大きすぎる'], [415, 'Content-Type'], [502, '繋がらない'], [504, '応答しない'], [500, '失敗した']]) {
    msg.value = 'x';
    msg.listeners.input[0]();
    h.click(h.doc.byId.send);
    h.calls[h.calls.length - 1].respond(status);
    await h.tick();
    assert.ok(ws.textContent.includes(word), status + ': ' + ws.textContent);
    assert.strictEqual(msg.value, 'x');
    assert.strictEqual(h.doc.byId.send.disabled, false);
  }
  msg.value = 'x';
  h.click(h.doc.byId.send);
  h.calls[h.calls.length - 1].fail(); // 通信の失敗
  await h.tick();
  assert.ok(ws.textContent.includes('通信の失敗'));
  const n = h.calls.length;
  msg.value = 'あ'.repeat(30000); // 90,000 バイト
  msg.listeners.input[0]();
  h.click(h.doc.byId.send);
  assert.strictEqual(h.calls.length, n);
  assert.ok(ws.textContent.includes('大きすぎる'));
});

test('終了ボタン: 2 回押しで /stop を送る。終了後・要求後は無効', async () => {
  const h = harness();
  const stop = h.doc.byId.stop;
  assert.strictEqual(stop.disabled, true);
  h.hello();
  assert.strictEqual(stop.disabled, false);
  h.click(stop);
  assert.strictEqual(h.calls.length, 0, '1 回で送った');
  assert.strictEqual(stop.textContent, '本当に終了する');
  h.runTimers(); // 猶予が過ぎると、戻る
  assert.strictEqual(stop.textContent, '終了');
  h.click(stop);
  h.click(stop);
  assert.strictEqual(h.calls.length, 1);
  assert.strictEqual(h.calls[0].url, BASE + '/stop');
  assert.deepStrictEqual(h.calls[0].body, {});
  assert.strictEqual(stop.disabled, true);
  h.calls[0].respond(200);
  await h.tick();
  assert.ok(h.doc.byId['write-status'].textContent.includes('終了を要求'));
  assert.strictEqual(stop.disabled, true);
});

test('ダイアログ: 未決の権限要求からだけ作る。許可は、世代・request_id・outcome だけを、固定の URL へ送り、決着の表示は permission.resolved からだけ', async () => {
  const h = harness();
  h.hello();
  assert.strictEqual(dialogEls(h).length, 0);
  h.fire(ev(0, 'permission.requested', pendingReq('req-1')));
  h.runTimers();
  assert.strictEqual(dialogEls(h).length, 1);
  const dlg = dialogEls(h)[0];
  assert.ok(dlg.textContent.includes('Write') && dlg.textContent.includes('/work/new.txt'));
  const approve = findBtn(dlg, 'approve');
  const deny = findBtn(dlg, 'deny');
  assert.strictEqual(approve.disabled, false);
  h.click(approve);
  assert.strictEqual(approve.disabled, true, '二重クリックを防ぐ');
  assert.strictEqual(deny.disabled, true);
  h.click(approve);
  h.click(deny);
  assert.strictEqual(h.calls.length, 1);
  assert.strictEqual(h.calls[0].url, BASE + '/permission');
  assert.deepStrictEqual(h.calls[0].body, {generation: G1, request_id: 'req-1', outcome: 'allow_once'});
  h.calls[0].respond(200);
  await h.tick();
  const shown = h.doc.byId.log.textContent + h.doc.byId.dialogs.textContent;
  assert.ok(!shown.includes('決着: allow_once'), '自分のクリックで、先に、決着と表示した');
  assert.ok(!shown.includes('承認済み'));
  assert.strictEqual(approve.disabled, true, '200 の後、resolved を待つ間に有効');
  h.fire(ev(1, 'permission.resolved', {request_id: 'req-1', outcome: 'allow_once', by: 'human'}));
  h.runTimers();
  assert.strictEqual(dialogEls(h).length, 0, '決着した要求のダイアログが残った');
  assert.ok(h.doc.byId.log.textContent.includes('決着: allow_once (human)'));
});

test('ダイアログ: 拒否は reject_once。409・404 は「決着済み」として、有効に戻さない。ほかの失敗は、もう一度押せる', async () => {
  const h = harness();
  h.hello();
  for (const [i, rid] of ['a', 'b', 'c'].entries()) h.fire(ev(i, 'permission.requested', pendingReq(rid)));
  h.runTimers();
  const [da, db, dc] = dialogEls(h);
  h.click(findBtn(da, 'deny'));
  assert.deepStrictEqual(h.calls[0].body, {generation: G1, request_id: 'a', outcome: 'reject_once'});
  h.calls[0].respond(409);
  await h.tick();
  assert.ok(da.textContent.includes('すでに決着済み'));
  assert.strictEqual(findBtn(da, 'deny').disabled, true);
  assert.strictEqual(findBtn(da, 'approve').disabled, true);
  h.click(findBtn(db, 'approve'));
  h.calls[1].respond(404);
  await h.tick();
  assert.ok(db.textContent.includes('もう無い'));
  assert.strictEqual(findBtn(db, 'approve').disabled, true);
  h.click(findBtn(dc, 'approve'));
  h.calls[2].respond(504);
  await h.tick();
  assert.strictEqual(findBtn(dc, 'approve').disabled, false, '一時的な失敗は、もう一度押せる');
  h.click(findBtn(dc, 'approve'));
  h.calls[3].fail();
  await h.tick();
  assert.ok(dc.textContent.includes('通信の失敗'));
});

test('ダイアログ: 複数の要求で、押したものの request_id だけを送る (DOM から読み戻さない)', () => {
  const h = harness();
  h.hello();
  const ids = ['__proto__', 'constructor', 'a"b\'c<d>', 'x'.repeat(300)];
  for (const [i, rid] of ids.entries()) h.fire(ev(i, 'permission.requested', pendingReq(rid, {title: 'T' + i})));
  h.runTimers();
  const dl = dialogEls(h);
  assert.strictEqual(dl.length, ids.length);
  for (let i = ids.length - 1; i >= 0; i--) {
    const btn = findBtn(dl[i], i === 3 ? 'deny' : 'approve'); // 300 文字の ID は、表示で切れる (許可できない) ので、拒否だけ
    h.click(btn);
    assert.strictEqual(h.calls[h.calls.length - 1].body.request_id, ids[i]);
  }
});

test('ダイアログ: input を全部は表示できないとき・request_id に見えない文字があるときは、許可できない (拒否だけ)。押しても送らない', () => {
  const h = harness();
  h.hello();
  h.fire(ev(0, 'permission.requested', pendingReq('big', {input: {command: 'y'.repeat(50000)}})));
  h.fire(ev(1, 'permission.requested', pendingReq('x' + cp(0x200b))));
  h.fire(ev(2, 'permission.requested', pendingReq('x')));
  h.runTimers();
  const [big, inv, ok] = dialogEls(h);
  for (const d of [big, inv]) {
    assert.strictEqual(findBtn(d, 'approve').disabled, true);
    assert.strictEqual(findBtn(d, 'deny').disabled, false);
    assert.ok(d.textContent.includes('許可できない'));
    h.click(findBtn(d, 'approve')); // 無効なボタンの click (テストからの直接の呼び出し) でも、送らない
  }
  assert.strictEqual(h.calls.length, 0);
  assert.strictEqual(findBtn(ok, 'approve').disabled, false);
  assert.ok(inv.textContent.includes('<U+200B>'), '見えない文字が、印になっていない');
  h.click(findBtn(inv, 'deny'));
  assert.strictEqual(h.calls[0].body.request_id, 'x' + cp(0x200b), '生の request_id を返していない');
});

test('request_id: 見えない文字だけが違う 2 つの要求は、別の要求として扱う (表示した要求と別の要求を承認させない)', () => {
  const s = stateWith([ev(0, 'permission.requested', pendingReq('x', {input: {a: 'safe'}})), ev(1, 'permission.requested', pendingReq('x' + cp(0x200b), {input: {a: 'EVIL'}}))]);
  const p = s.items.filter((i) => i.kind === 'permission');
  assert.strictEqual(p.length, 2);
  assert.notStrictEqual(p[0].requestId, p[1].requestId);
  assert.strictEqual(p[0].idPlain, true);
  assert.strictEqual(p[1].idPlain, false);
  core.applyEvent(s, ev(2, 'permission.resolved', {request_id: 'x' + cp(0x200b), outcome: 'reject_once', by: 'human'}));
  assert.strictEqual(p[0].state, 'pending');
  assert.strictEqual(p[1].state, 'reject_once');
  assert.strictEqual(core.applyEvent(s, ev(3, 'permission.requested', pendingReq('z'.repeat(core.LIMITS.maxRequestID + 1)))), false);
});

test('世代: hello 前・世代の無い hello では、ダイアログを出さず、送らない', () => {
  const h = harness();
  h.fire(ev(0, 'permission.requested', pendingReq('early'))); // hello の前の Event は使わない
  h.runTimers();
  assert.strictEqual(dialogEls(h).length, 0);
  assert.strictEqual(h.app.state.items.length, 0, 'hello の前の Event を、状態に入れた');
  h.es().fire('hello', {first_seq: 0}); // 世代が無い
  h.fire(ev(0, 'permission.requested', pendingReq('nogen')));
  h.runTimers();
  assert.strictEqual(dialogEls(h).length, 0);
  assert.strictEqual(h.calls.length, 0);
  assert.strictEqual(h.app.state.generation, null);
});

test('世代: ダイアログは、見せた時点の世代を持つ。世代が変わる (serve の再起動) と、古いダイアログは消え、その click は何も送らない。新しい世代は、新しい要求だけが使う', () => {
  const h = harness();
  h.hello(G1);
  h.fire(ev(0, 'permission.requested', pendingReq('r')));
  h.runTimers();
  const oldDlg = dialogEls(h)[0];
  const oldApprove = findBtn(oldDlg, 'approve');
  h.es().fire('error', {}); // serve が落ちた: 接続が切れると、無効
  h.runTimers();
  assert.strictEqual(oldApprove.disabled, true, '切断中に有効');
  h.runTimers(); // 再接続
  h.es().fire('hello', {first_seq: 0, generation: G2}); // 別の起動 (新しい serve)
  h.es().fire('message', ev(0, 'permission.requested', pendingReq('r', {input: {file_path: '/etc/passwd'}}))); // 同じ request_id の、別の input
  h.runTimers();
  assert.ok(!dialogEls(h).includes(oldDlg), '古いダイアログが残った');
  assert.strictEqual(dialogEls(h).length, 1);
  h.click(oldApprove); // 古い画面の click
  assert.strictEqual(h.calls.length, 0, '古い画面の許可が、送られた');
  const cur = dialogEls(h)[0];
  assert.ok(cur.textContent.includes('/etc/passwd'));
  h.click(findBtn(cur, 'deny'));
  assert.strictEqual(h.calls[0].body.generation, G2);
});

test('世代: 同じ世代の繋ぎ直しでは、ダイアログは 1 つのまま (重複しない)。切断中は無効で、繋がると戻る', () => {
  const h = harness();
  h.hello();
  h.fire(ev(0, 'permission.requested', pendingReq('r')));
  h.runTimers();
  const dlg = dialogEls(h)[0];
  h.es().fire('error', {});
  h.runTimers();
  assert.strictEqual(findBtn(dlg, 'deny').disabled, true);
  h.runTimers();
  h.es().fire('hello', {first_seq: 0, generation: G1});
  h.es().fire('message', ev(0, 'permission.requested', pendingReq('r')));
  h.runTimers();
  assert.strictEqual(dialogEls(h).length, 1);
  assert.strictEqual(dialogEls(h)[0], dlg);
  assert.strictEqual(findBtn(dlg, 'deny').disabled, false);
});

test('世代: 終了後は、ダイアログを操作できない', () => {
  const h = harness();
  h.hello();
  h.fire(ev(0, 'permission.requested', pendingReq('r')));
  h.runTimers();
  const dlg = dialogEls(h)[0];
  h.es().fire('end', {exit: 0});
  h.runTimers();
  h.click(findBtn(dlg, 'deny'));
  assert.strictEqual(h.calls.length, 0);
});

test('敵対的な tool 名・title・input は、ダイアログでも textContent だけ。空白に見える名前は印になる', () => {
  const h = harness();
  h.hello();
  const evil = '<img src=x onerror=alert(1)>' + cp(0x202e);
  h.fire(ev(0, 'permission.requested', pendingReq('r1', {tool_name: evil, title: evil + '\nAPPROVED', input: {c: evil}})));
  h.fire(ev(1, 'permission.requested', pendingReq('r2', {tool_name: cp(0x3164) + cp(0x2800), title: ''})));
  h.runTimers();
  for (const bad of ['script', 'img', 'iframe', 'a', 'svg', 'style', 'link', 'form', 'input', 'select']) assert.ok(!h.doc.created.includes(bad), bad);
  const [d1, d2] = dialogEls(h);
  assert.ok(d1.textContent.includes('<img src=x onerror=alert(1)>'));
  assert.ok(!d1.textContent.includes(cp(0x202e)));
  assert.ok(!d1.textContent.includes('\nAPPROVED'), '短い項目の改行が、行を偽造できる');
  assert.ok(d2.textContent.includes('<U+3164><U+2800>'));
});

test('N-C: 開いた <details> は、項目の更新で閉じない', () => {
  const h = harness();
  h.hello();
  h.fire(ev(0, 'tool.call', {call_id: 'c', name: 'T', kind: 'other', status: 'in_progress', input: {a: 1}}));
  h.runTimers();
  const item = h.doc.byId.log.children[0];
  let det = item.children.find((c) => c.tagName === 'details');
  det.open = true;
  det.listeners.toggle[0]();
  h.fire(ev(1, 'tool.update', {call_id: 'c', status: 'completed', output: 'ok'}));
  h.runTimers();
  det = h.doc.byId.log.children[0].children.find((c) => c.tagName === 'details');
  assert.strictEqual(det.open, true, '更新で、閉じた');
});

test('送信欄・ダイアログは、項目の作り直しの外にある (入力途中の指示が消えない)', () => {
  const h = harness();
  h.hello();
  h.doc.byId.msg.value = '入力の途中';
  for (let i = 0; i < 30; i++) h.fire(ev(i, 'message.text', {text: 'm' + i}));
  h.es().fire('hello', {first_seq: 0, generation: G2}); // 世代が変わって、表示が全部作り直される
  h.runTimers();
  assert.strictEqual(h.doc.byId.msg.value, '入力の途中');
});

test('未決の件数が、枠の外の見出しに出る', () => {
  const h = harness();
  h.hello();
  assert.strictEqual(h.doc.byId['dialogs-head'].textContent, '');
  h.fire(ev(0, 'permission.requested', pendingReq('a')));
  h.fire(ev(1, 'permission.requested', pendingReq('b')));
  h.runTimers();
  assert.ok(h.doc.byId['dialogs-head'].textContent.includes('2 件'));
  h.fire(ev(2, 'permission.resolved', {request_id: 'a', outcome: 'reject_once', by: 'human'}));
  h.fire(ev(3, 'permission.resolved', {request_id: 'b', outcome: 'reject_once', by: 'human'}));
  h.runTimers();
  assert.strictEqual(h.doc.byId['dialogs-head'].textContent, '');
});
