'use strict';
// chat-core.js: 構造化チャットの表示の、純関数の側 (DOM に触れない。node で試験できる)。
//
// 入力は、goronation web の SSE (/s/{id}/events) が届ける Event の JSON (hello・data・end)。エージェントの出力は敵対入力なので、
// ここで、長さを切り、表示に危険な文字 (制御文字・双方向制御・行区切り) を、見える形の印にする。DOM への描画は chat.js が、textContent だけで行う。
(function (root) {
  const LIMITS = {
    maxItems: 1500,     // 保持・表示する項目の数 (超えたら、古い方から捨てる。未決の権限要求は捨てない)
    maxText: 20000,     // 1 つの発言・送った文の、文字数
    maxInput: 4000,     // tool の input・出力の、文字数
    maxShort: 300,      // 名前・title・message の、文字数
    maxPending: 64,     // 未決の権限要求の数 (これ以上は、表示しない)
  };

  const TRIM_SLACK = 250;

  // 表示に危険な文字の判定 (コードポイントで持つ: 原稿に、見えない文字を書かない)。
  function isDangerous(cp) {
    if (cp < 0x20) return cp !== 0x0a && cp !== 0x09;          // C0 (改行・タブは残す)
    if (cp >= 0x7f && cp <= 0x9f) return true;                 // DEL・C1
    if (cp === 0x061c || cp === 0x180e) return true;           // ALM・MVS
    if (cp >= 0x200b && cp <= 0x200f) return true;             // ゼロ幅・LRM・RLM
    if (cp >= 0x2028 && cp <= 0x202e) return true;             // 行・段落区切り・双方向の埋め込み・上書き
    if (cp >= 0x2060 && cp <= 0x206f) return true;             // 単語結合子・不可視の演算子・双方向の分離
    if (cp === 0xfeff || cp === 0xfffe || cp === 0xffff) return true; // BOM・非文字
    if (cp >= 0xfff9 && cp <= 0xfffb) return true;             // 注釈記号
    if (cp >= 0xd800 && cp <= 0xdfff) return true;             // 対になっていないサロゲート
    if (cp >= 0xe0000 && cp <= 0xe007f) return true;           // タグ文字
    return false;
  }

  function mark(cp) {
    let h = cp.toString(16).toUpperCase();
    while (h.length < 4) h = '0' + h;
    return '<U+' + h + '>';
  }

  // sanitize は、s を、危険な文字を見える印 (<U+202E> など) にした文字列にする。
  function sanitize(s) {
    let out = '';
    let last = 0;
    for (let i = 0; i < s.length; i++) {
      const c = s.charCodeAt(i);
      let cp = c;
      let width = 1;
      if (c >= 0xd800 && c <= 0xdbff) {
        const d = i + 1 < s.length ? s.charCodeAt(i + 1) : 0;
        if (d >= 0xdc00 && d <= 0xdfff) {
          cp = 0x10000 + ((c - 0xd800) << 10) + (d - 0xdc00);
          width = 2;
        }
      }
      if (isDangerous(cp)) {
        out += s.slice(last, i) + mark(cp);
        last = i + width;
      }
      i += width - 1;
    }
    return last === 0 ? s : out + s.slice(last);
  }

  // clip は、値を文字列にして、n 文字までにする (サロゲートの途中で切らない)。cut は、切ったか (元の長さは total)。
  function clip(v, n) {
    let s = typeof v === 'string' ? v : (v === null || v === undefined ? '' : safeJSON(v));
    const total = s.length;
    let cut = false;
    if (s.length > n) {
      let end = n;
      const c = s.charCodeAt(end - 1);
      if (c >= 0xd800 && c <= 0xdbff) end--;
      s = s.slice(0, end);
      cut = true;
    }
    return {text: sanitize(s), cut: cut, total: total};
  }

  function safeJSON(v) {
    try {
      const s = JSON.stringify(v, null, 2);
      return typeof s === 'string' ? s : '';
    } catch (e) {
      return '';
    }
  }

  function obj(v) {
    return v !== null && typeof v === 'object' && !Array.isArray(v) ? v : {};
  }

  function createState() {
    return {
      generation: null,   // 最後の hello の世代 (変わったら、表示を作り直す)
      firstSeq: 0,
      lastSeq: -1,
      items: [],          // 表示する項目 (古い順)
      byCall: new Map(),  // call_id → tool の項目
      pending: new Map(), // request_id → permission の項目 (未決・決着の両方。項目が捨てられるまで)
      nextId: 1,
      trimmed: false,     // 古い項目を捨てた
      omitted: false,     // 履歴の欠け (hello の first_seq > 0、または、繋ぎ直しの間の欠け)
      ended: null,        // {exit} (end を受けた)
      ignored: 0,         // 表示しなかった Event の数 (agent.frame・未知の type・不正な形・重複)
      dirty: new Map(),   // id → 項目 (描画が要る)
      removed: [],        // 描画から外す項目の id
      reset: false,       // 表示を全部作り直す (世代が変わった)
    };
  }

  function touch(state, item) {
    state.dirty.set(item.id, item);
  }

  function add(state, item) {
    item.id = state.nextId++;
    state.items.push(item);
    touch(state, item);
    if (state.items.length > LIMITS.maxItems + TRIM_SLACK) trim(state); // 1 件ごとに O(n) にしない: 余裕を持って、まとめて捨てる
    return item;
  }

  // trim は、maxItems を超えた分を、古い方から捨てる (未決の権限要求は、決着まで捨てない)。1 回に、まとめて捨てる。
  function trim(state) {
    const excess = state.items.length - LIMITS.maxItems;
    const keep = [];
    let dropped = 0;
    for (const it of state.items) {
      const pinned = it.kind === 'permission' && it.state === 'pending';
      if (dropped < excess && !pinned) {
        dropped++;
        state.dirty.delete(it.id);
        state.removed.push(it.id);
        if (it.kind === 'tool') state.byCall.delete(it.callId);
        if (it.kind === 'permission') state.pending.delete(it.requestId);
      } else {
        keep.push(it);
      }
    }
    if (dropped > 0) state.trimmed = true;
    state.items = keep;
  }

  // applyHello は、hello の data を受ける。世代が (前と) 変わったら、表示を作り直す (serve の再起動で、seq が振り直される)。
  // 同じ世代の繋ぎ直しは、Snapshot が全部届くので、seq で、すでに表示した分を捨てる (applyEvent)。
  function applyHello(state, data) {
    const d = obj(data);
    const gen = typeof d.generation === 'string' ? d.generation : null;
    const first = Number.isSafeInteger(d.first_seq) && d.first_seq >= 0 ? d.first_seq : 0;
    if (state.generation !== null && gen !== state.generation) {
      for (const it of state.items) state.removed.push(it.id);
      state.items = [];
      state.byCall = new Map();
      state.pending = new Map();
      state.dirty = new Map();
      state.lastSeq = -1;
      state.trimmed = false;
      state.omitted = false;
      state.ended = null;
      state.reset = true;
    }
    state.generation = gen;
    state.firstSeq = first;
    if (first > state.lastSeq + 1 && first > 0) state.omitted = true; // 溢れて捨てた分がある (繋ぎ直しの間の欠けも含む)
    return {reset: state.reset};
  }

  function applyEnd(state, data) {
    const d = obj(data);
    state.ended = {exit: Number.isSafeInteger(d.exit) ? d.exit : null};
  }

  // applyEvent は、Event 1 つを受ける。表示に反映したら true。
  function applyEvent(state, ev) {
    const e = obj(ev);
    if (typeof e.type !== 'string' || !Number.isSafeInteger(e.seq) || e.seq < 0) {
      state.ignored++;
      return false;
    }
    if (e.seq <= state.lastSeq) { // 繋ぎ直しで、また届いた分
      state.ignored++;
      return false;
    }
    state.lastSeq = e.seq;
    const d = obj(e.data);
    switch (e.type) {
      case 'session.started': {
        const model = clip(d.model, LIMITS.maxShort);
        const cwd = clip(d.cwd, LIMITS.maxShort);
        add(state, {kind: 'session', agent: clip(d.agent, LIMITS.maxShort).text, model: model.text, cwd: cwd.text});
        return true;
      }
      case 'turn.started': {
        const t = clip(d.text, LIMITS.maxText);
        add(state, {kind: 'user', text: t.text, cut: t.cut, total: t.total});
        return true;
      }
      case 'message.text': {
        const t = clip(d.text, LIMITS.maxText);
        add(state, {kind: 'assistant', text: t.text, cut: t.cut, total: t.total});
        return true;
      }
      case 'tool.call': {
        const callId = clip(d.call_id, LIMITS.maxShort).text;
        let it = callId ? state.byCall.get(callId) : undefined;
        const input = d.input === undefined || d.input === null ? {text: '', cut: false, total: 0} : clip(d.input, LIMITS.maxInput);
        if (!it) {
          it = add(state, {kind: 'tool', callId: callId, name: '', toolKind: '', status: '', input: input, output: null, error: null});
          if (callId) state.byCall.set(callId, it);
        }
        it.name = clip(d.name, LIMITS.maxShort).text;
        it.toolKind = clip(d.kind, LIMITS.maxShort).text;
        it.status = clip(d.status, LIMITS.maxShort).text;
        it.input = input;
        touch(state, it);
        return true;
      }
      case 'tool.update': {
        const callId = clip(d.call_id, LIMITS.maxShort).text;
        let it = callId ? state.byCall.get(callId) : undefined;
        if (!it) { // 呼び出しが、履歴から省略された
          it = add(state, {kind: 'tool', callId: callId, name: '(履歴から省略)', toolKind: '', status: '', input: {text: '', cut: false, total: 0}, output: null, error: null});
          if (callId) state.byCall.set(callId, it);
        }
        it.status = clip(d.status, LIMITS.maxShort).text;
        if (d.output !== undefined && d.output !== null) it.output = clip(d.output, LIMITS.maxInput);
        if (d.error !== undefined && d.error !== null) it.error = clip(d.error, LIMITS.maxInput);
        touch(state, it);
        return true;
      }
      case 'permission.requested': {
        const rid = clip(d.request_id, LIMITS.maxShort).text;
        if (!rid || state.pending.has(rid) || state.pending.size >= LIMITS.maxPending) {
          state.ignored++;
          return false;
        }
        const it = add(state, {
          kind: 'permission', requestId: rid, callId: clip(d.call_id, LIMITS.maxShort).text,
          toolName: clip(d.tool_name, LIMITS.maxShort).text, toolKind: clip(d.kind, LIMITS.maxShort).text,
          title: clip(d.title, LIMITS.maxShort).text, input: clip(d.input, LIMITS.maxInput),
          state: 'pending', by: '',
        });
        state.pending.set(rid, it);
        return true;
      }
      case 'permission.resolved': {
        const rid = clip(d.request_id, LIMITS.maxShort).text;
        const it = rid ? state.pending.get(rid) : undefined;
        if (!it) { // 要求が、履歴から省略された (決着だけが残った)。表示しない
          state.ignored++;
          return false;
        }
        it.state = clip(d.outcome, LIMITS.maxShort).text || 'cancelled';
        it.by = clip(d.by, LIMITS.maxShort).text;
        touch(state, it);
        return true;
      }
      case 'usage': {
        const num = (v) => (Number.isFinite(v) ? v : null);
        add(state, {kind: 'usage', inputTokens: num(d.input_tokens), outputTokens: num(d.output_tokens), costUSD: num(d.cost_usd), contextWindow: num(d.context_window)});
        return true;
      }
      case 'turn.completed': {
        add(state, {kind: 'turn_end', stopReason: clip(d.stop_reason, LIMITS.maxShort).text, isError: d.is_error === true});
        return true;
      }
      case 'error': {
        const m = clip(d.message, LIMITS.maxInput);
        add(state, {kind: 'error', status: Number.isFinite(d.status) ? d.status : null, message: m.text, cut: m.cut});
        return true;
      }
      default: // agent.frame・未知の type
        state.ignored++;
        return false;
    }
  }

  // drain は、描画が要る項目 (upserts) と、外す項目の id (removes) を返し、印を空にする。reset なら、呼び手は、表示を全部作り直す。
  function drain(state) {
    const out = {reset: state.reset, upserts: Array.from(state.dirty.values()), removes: state.removed};
    state.dirty = new Map();
    state.removed = [];
    state.reset = false;
    return out;
  }

  const api = {LIMITS: LIMITS, sanitize: sanitize, clip: clip, createState: createState, applyHello: applyHello, applyEnd: applyEnd, applyEvent: applyEvent, drain: drain};
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  else root.GoroChatCore = api;
})(typeof self !== 'undefined' ? self : this);
