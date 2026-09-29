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
  const GENERATION_RE = /^[0-9a-f]{32}$/; // serve の世代 (ADR 0013)
  const OUTCOMES = new Set(['allow_once', 'reject_once', 'cancelled']); // permission.resolved の outcome (これ以外は unknown)

  // 表示に危険な文字の判定 (コードポイントで持つ: 原稿に、見えない文字を書かない)。
  function isDangerous(cp) {
    if (cp < 0x20) return cp !== 0x0a && cp !== 0x09;          // C0 (改行・タブは残す)
    if (cp >= 0x7f && cp <= 0x9f) return true;                 // DEL・C1
    if (cp === 0x00ad || cp === 0x034f) return true;           // 軟ハイフン・結合用の不可視
    if (cp === 0x061c || cp === 0x180e) return true;           // ALM・MVS
    if (cp === 0x115f || cp === 0x1160 || cp === 0x3164 || cp === 0xffa0) return true; // ハングルの埋め字 (空白に見える)
    if (cp === 0x17b4 || cp === 0x17b5) return true;           // クメールの固有母音 (不可視)
    if (cp >= 0x180b && cp <= 0x180f) return true;             // モンゴル語の異体字選択子
    if (cp >= 0x200b && cp <= 0x200f) return true;             // ゼロ幅・LRM・RLM
    if (cp >= 0x2028 && cp <= 0x202e) return true;             // 行・段落区切り・双方向の埋め込み・上書き
    if (cp >= 0x2060 && cp <= 0x206f) return true;             // 単語結合子・不可視の演算子・双方向の分離
    if (cp === 0x2800) return true;                            // 点字の空白
    if (cp === 0xfeff || cp === 0xfffe || cp === 0xffff) return true; // BOM・非文字
    if (cp >= 0xfff9 && cp <= 0xfffc) return true;             // 注釈記号・物体の置換文字
    if (cp >= 0xd800 && cp <= 0xdfff) return true;             // 対になっていないサロゲート
    if (cp >= 0xe0000 && cp <= 0xe007f) return true;           // タグ文字
    if (cp >= 0xe0100 && cp <= 0xe01ef) return true;           // 異体字選択子の補助
    return false;
  }

  // 短い項目 (名前・title・ID・状態) 用の、厳しい判定: 通常の空白 (U+0020) 以外の、空白に見える文字・改行・タブ・異体字選択子も、印にする。
  // 名前が「空白に見える」・ラベルの中に、行を偽造できる、のを防ぐ (承認の画面が、tool 名を見せる)。長い本文は、日本語の全角空白・絵文字の
  // 異体字選択子などを、正当に含むので、この判定を使わない。
  function isDangerousStrict(cp) {
    if (isDangerous(cp)) return true;
    if (cp === 0x0a || cp === 0x09 || cp === 0x00a0 || cp === 0x1680) return true;
    if (cp >= 0x2000 && cp <= 0x200a) return true;
    if (cp === 0x202f || cp === 0x205f || cp === 0x3000) return true;
    if (cp >= 0xfe00 && cp <= 0xfe0f) return true;
    return false;
  }

  function mark(cp) {
    let h = cp.toString(16).toUpperCase();
    while (h.length < 4) h = '0' + h;
    return '<U+' + h + '>';
  }

  // sanitize は、s を、危険な文字を見える印 (<U+202E> など) にした文字列にする。
  function sanitize(s, strict) {
    const bad = strict ? isDangerousStrict : isDangerous;
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
      if (bad(cp)) {
        out += s.slice(last, i) + mark(cp);
        last = i + width;
      }
      i += width - 1;
    }
    return last === 0 ? s : out + s.slice(last);
  }

  // clip は、値を文字列にして、n 文字までにする (サロゲートの途中で切らない)。cut は、切ったか (元の長さは total)。
  function clip(v, n, strict) {
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
    return {text: sanitize(s, strict), cut: cut, total: total};
  }

  // short は、短い項目 (名前・title・ID・状態) の、厳しい表示用の文字列。
  function short(v) {
    return clip(v, LIMITS.maxShort, true).text;
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

  // unresolved は、未決 (決着していない) の権限要求の数。state.pending は、決着済みも (項目が捨てられるまで) 持つので、size は使わない。
  function unresolved(state) {
    let n = 0;
    for (const it of state.pending.values()) if (it.state === 'pending') n++;
    return n;
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
    if (typeof d.generation !== 'string' || !GENERATION_RE.test(d.generation)) return {ok: false, reset: false}; // 世代が無い・壊れた hello は受けない (世代を、承認に使うため)
    const gen = d.generation;
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
    return {ok: true, reset: state.reset};
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
        add(state, {kind: 'session', agent: short(d.agent), model: short(d.model), cwd: short(d.cwd)});
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
        const callId = short(d.call_id);
        let it = callId ? state.byCall.get(callId) : undefined;
        const input = d.input === undefined || d.input === null ? {text: '', cut: false, total: 0} : clip(d.input, LIMITS.maxInput);
        if (!it) {
          it = add(state, {kind: 'tool', callId: callId, name: '', toolKind: '', status: '', input: input, output: null, error: null});
          if (callId) state.byCall.set(callId, it);
        }
        it.name = short(d.name);
        it.toolKind = short(d.kind);
        it.status = short(d.status);
        it.input = input;
        touch(state, it);
        return true;
      }
      case 'tool.update': {
        const callId = short(d.call_id);
        let it = callId ? state.byCall.get(callId) : undefined;
        if (!it) { // 呼び出しが、履歴から省略された
          it = add(state, {kind: 'tool', callId: callId, name: '(履歴から省略)', toolKind: '', status: '', input: {text: '', cut: false, total: 0}, output: null, error: null});
          if (callId) state.byCall.set(callId, it);
        }
        it.status = short(d.status);
        if (d.output !== undefined && d.output !== null) it.output = clip(d.output, LIMITS.maxInput);
        if (d.error !== undefined && d.error !== null) it.error = clip(d.error, LIMITS.maxInput);
        touch(state, it);
        return true;
      }
      case 'permission.requested': {
        const rid = short(d.request_id);
        if (!rid || state.pending.has(rid) || unresolved(state) >= LIMITS.maxPending) {
          state.ignored++;
          return false;
        }
        const it = add(state, {
          kind: 'permission', requestId: rid, callId: short(d.call_id),
          toolName: short(d.tool_name), toolKind: short(d.kind),
          title: short(d.title), input: clip(d.input, LIMITS.maxInput),
          state: 'pending', by: '',
        });
        state.pending.set(rid, it);
        return true;
      }
      case 'permission.resolved': {
        const rid = short(d.request_id);
        const it = rid ? state.pending.get(rid) : undefined;
        if (!it) { // 要求が、履歴から省略された (決着だけが残った)。表示しない
          state.ignored++;
          return false;
        }
        it.state = OUTCOMES.has(d.outcome) ? d.outcome : 'unknown'; // 許可リスト: 決着済みが、「未決」(pending) に見えない
        it.by = short(d.by);
        touch(state, it);
        return true;
      }
      case 'usage': {
        const num = (v) => (Number.isFinite(v) ? v : null);
        add(state, {kind: 'usage', inputTokens: num(d.input_tokens), outputTokens: num(d.output_tokens), costUSD: num(d.cost_usd), contextWindow: num(d.context_window)});
        return true;
      }
      case 'turn.completed': {
        add(state, {kind: 'turn_end', stopReason: short(d.stop_reason), isError: d.is_error === true});
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
