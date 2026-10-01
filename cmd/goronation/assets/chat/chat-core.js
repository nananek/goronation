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
    maxRequestID: 1000, // request_id の長さ (これより長い要求は、扱えないので、表示しない)
    // 要約・詳細・form (spec/v0 の上限は文字数 (rune)。ここは UTF-16 の単位なので、サーバーが通す値を、切らない大きさにしてある)。
    maxSummary: 400,    // 要約 (spec/v0: 200 字)
    maxDetails: 16,     // 詳細の項目の数 (spec/v0: 16)
    maxDetailText: 8000, // 詳細の 1 項目の本文 (spec/v0: 4,000 字)
    maxFields: 16,      // form のフィールドの数 (spec/v0: 16)
    maxOptions: 32,     // 1 つのフィールドの選択肢の数 (spec/v0: 32)
    maxKey: 128,        // フィールドの key (spec/v0: 64 字)。送り返す値なので、切らずに、超えたら答えられない形にする
    maxValue: 400,      // 選択肢の value (spec/v0: 200 字)。同上
    maxFormTitle: 400,  // form・フィールドの title・選択肢の label (spec/v0: 200 字)
    maxFormDesc: 2000,  // description (spec/v0: 1,000 字)
    maxAnswer: 4000,    // 回答 1 つの長さ (spec/v0: 4,000 字。入力欄の maxlength)
    maxAnswerValues: 40, // multiselect の回答の個数 (spec/v0: 40)
  };

  const TRIM_SLACK = 250;
  const MAX_DEPTH = 40; // 表示する入れ子の深さ (これより深い input は、打ち切る = 全部は表示できない)
  const GENERATION_RE = /^[0-9a-f]{32}$/; // serve の世代 (ADR 0013)
  const OUTCOMES = new Set(['allow_once', 'reject_once', 'cancelled']); // permission.resolved の outcome (これ以外は unknown)
  const FORM_OUTCOMES = new Set(['answered', 'cancelled']); // form.resolved の outcome (permission とは別の集合。これ以外は unknown)
  const DETAIL_KINDS = new Set(['text', 'command', 'path', 'url']); // 詳細の項目の kind (知らない kind は text)
  const FIELD_TYPES = new Set(['text', 'select', 'multiselect']); // form のフィールドの type (知らない type があれば、答えられない)

  // 表示に危険な文字の判定 (コードポイントで持つ: 原稿に、見えない文字を書かない)。
  // Unicode の文字クラス: 書式制御 (Cf)・既定で無視される文字は、手で挙げた範囲 (下) に漏れがあるので、全て印にする。異体字選択子 (U+FE00-FE0F) は、
  // 絵文字に要るので、厳しい判定 (isDangerousStrict) だけ。
  const INVISIBLE_RE = /^[\p{Cf}\p{Default_Ignorable_Code_Point}]$/u;
  const STRICT_EXTRA_RE = /^[\p{Z}\p{Co}]$/u;

  function isDangerous(cp) {
    if (cp >= 0x20 && cp < 0x7f) return false; // ASCII の表示できる文字 (速い道)
    if (cp < 0xd800 || cp > 0xdfff) { // 対になっていないサロゲートは、下で印にする
      if ((cp < 0xfe00 || cp > 0xfe0f) && INVISIBLE_RE.test(String.fromCodePoint(cp))) return true;
    }
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
    if (cp > 0x7f && STRICT_EXTRA_RE.test(String.fromCodePoint(cp))) return true; // 空白に見える文字・私用領域
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

  // boundedJSON は、v を、JSON.stringify(v, null, 2) と同じ形の文字列にする。ただし、出力が max 文字を超える・入れ子が MAX_DEPTH を超えるところで打ち切る
  // (cut: true)。JSON.stringify は、深い入れ子で例外を投げ (B1: 例外を「空の input」に化けさせない)、深さの 2 乗の長さの文字列を作る (L-E: CPU の増幅)。
  // 打ち切った値は、全体の長さが分からない (total: null)。
  function boundedJSON(v, max) {
    const out = [];
    let len = 0;
    let cut = false;
    function emit(t) {
      if (cut) return false;
      if (len + t.length > max) {
        out.push(t.slice(0, max - len));
        len = max;
        cut = true;
        return false;
      }
      out.push(t);
      len += t.length;
      return true;
    }
    function quote(t) {
      const room = max - len + 1;
      return JSON.stringify(t.length > room ? t.slice(0, room) : t); // 予算を超える分は、切ってから引用する (巨大な文字列を、全部は処理しない)
    }
    function walk(x, depth) {
      if (cut) return;
      if (x === null || x === undefined) { emit('null'); return; }
      switch (typeof x) {
        case 'string': emit(quote(x)); return;
        case 'number': emit(Number.isFinite(x) ? JSON.stringify(x) : 'null'); return;
        case 'boolean': emit(x ? 'true' : 'false'); return;
        case 'object': break;
        default: emit('null'); return;
      }
      if (depth >= MAX_DEPTH) { // 深すぎる: ここから先は、表示しない
        emit('"..."');
        cut = true;
        return;
      }
      const pad = '\n' + '  '.repeat(depth + 1);
      const end = '\n' + '  '.repeat(depth);
      if (Array.isArray(x)) {
        if (x.length === 0) { emit('[]'); return; }
        emit('[');
        for (let i = 0; i < x.length && !cut; i++) {
          emit((i === 0 ? '' : ',') + pad);
          walk(x[i], depth + 1);
        }
        if (!cut) emit(end + ']');
        return;
      }
      const keys = Object.keys(x);
      if (keys.length === 0) { emit('{}'); return; }
      emit('{');
      for (let i = 0; i < keys.length && !cut; i++) {
        emit((i === 0 ? '' : ',') + pad + quote(keys[i]) + ': ');
        walk(x[keys[i]], depth + 1);
      }
      if (!cut) emit(end + '}');
    }
    walk(v, 0);
    return {text: out.join(''), cut: cut, total: cut ? null : len};
  }

  // clip は、値を文字列にして、n 文字までにする (サロゲートの途中で切らない)。cut は、切ったか (元の長さは total。分からなければ null)。
  // 文字列以外の値は、boundedJSON (深さ・長さを限る)。
  function clip(v, n, strict) {
    if (v === null || v === undefined) return {text: '', cut: false, total: 0};
    if (typeof v !== 'string') {
      const b = boundedJSON(v, n);
      return {text: sanitize(b.text, strict), cut: b.cut, total: b.total};
    }
    let s = v;
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

  // permissionInput は、権限要求の input の表示。input が無い・オブジェクトでない要求は、何を許可するのか見せられないので、「全部は表示できない」(cut) にする。
  function permissionInput(v) {
    if (v === null || typeof v !== 'object' || Array.isArray(v)) return {text: clip(v, LIMITS.maxInput).text, cut: true, total: null};
    return clip(v, LIMITS.maxInput);
  }

  function obj(v) {
    return v !== null && typeof v === 'object' && !Array.isArray(v) ? v : {};
  }

  function isObj(v) {
    return v !== null && typeof v === 'object' && !Array.isArray(v);
  }

  // normalizeDetails は、permission.requested の詳細 (ADR 0041) を表示用にする。配列でない・空は「詳細なし」(旧い形。input の表示に戻る)。
  // 1 つでも、切った・形が不正・数が上限を超える項目があれば、全体を cut にする (承認の関門。見せていないものを承認させない)。
  // label は、エージェントが決めたキーを含むので、厳しい印 (short)。text の行は、4 字下げて出す: 本文の改行で、見出しの行 (■ label) を偽造させない。
  function normalizeDetails(v) {
    if (!Array.isArray(v) || v.length === 0) return {has: false, items: [], cut: false, text: ''};
    let cut = v.length > LIMITS.maxDetails;
    const items = [];
    for (let i = 0; i < v.length && i < LIMITS.maxDetails; i++) {
      const x = v[i];
      if (!isObj(x) || typeof x.text !== 'string' || typeof x.label !== 'string') { // 形が不正: 何を承認するのか、見せられない
        cut = true;
        continue;
      }
      const t = clip(x.text, LIMITS.maxDetailText);
      if (t.cut) cut = true;
      items.push({label: short(x.label), kind: typeof x.kind === 'string' && DETAIL_KINDS.has(x.kind) ? x.kind : 'text', text: t.text, cut: t.cut});
    }
    const lines = [];
    for (const it of items) {
      lines.push('■ ' + it.label + (it.kind !== 'text' ? ' [' + it.kind + ']' : ''));
      lines.push('    ' + (it.text === '' ? '(空)' : it.text.split('\n').join('\n    ')) + (it.cut ? ' …(以降は表示しない)' : ''));
    }
    return {has: true, items: items, cut: cut, text: lines.join('\n')};
  }

  // shownFully は、承認 (許可) してよい内容を、全部見せているか (chat.js の、枠を見た範囲・時間の判定は、別に要る)。詳細があれば詳細 (全項目を切らずに・
  // details_truncated でない)、旧い形は input。request_id が見た目どおりでなければ、どちらでも許可させない。
  function shownFully(item) {
    if (item.idPlain !== true || item.detailsTruncated !== false) return false; // details_truncated は、詳細が空・無くても、拒否だけ (ADR 0048 決定 1)
    if (item.details.has) return item.details.cut === false;
    return item.input.cut === false;
  }

  // normalizeFields は、form のフィールドを表示・送信用にする。key・選択肢の value は、送り返す値なので、生のまま持つ (表示は keyShown・label)。
  // 上限を超える・形が不正・key の重複・知らない type があれば、答えられない (answerable: false。取り消しだけ)。
  function normalizeFields(v) {
    let ok = Array.isArray(v) && v.length > 0 && v.length <= LIMITS.maxFields;
    const fields = [];
    if (Array.isArray(v)) {
      const keys = new Set();
      for (let i = 0; i < v.length && i < LIMITS.maxFields; i++) {
        const x = v[i];
        if (!isObj(x)) { ok = false; continue; }
        const key = typeof x.key === 'string' ? x.key : '';
        if (key === '' || key.length > LIMITS.maxKey || keys.has(key)) ok = false;
        keys.add(key);
        const type = typeof x.type === 'string' && FIELD_TYPES.has(x.type) ? x.type : '';
        if (type === '') ok = false;
        const options = [];
        if (type === 'select' || type === 'multiselect') {
          const o = Array.isArray(x.options) ? x.options : [];
          if (o.length === 0 || o.length > LIMITS.maxOptions) ok = false;
          const vals = new Set();
          for (let j = 0; j < o.length && j < LIMITS.maxOptions; j++) {
            const op = obj(o[j]);
            const value = typeof op.value === 'string' ? op.value : '';
            if (value === '' || value.length > LIMITS.maxValue || vals.has(value)) ok = false;
            vals.add(value);
            options.push({value: value, label: clip(op.label, LIMITS.maxFormTitle, true).text, description: clip(op.description, LIMITS.maxFormDesc).text});
          }
        }
        fields.push({key: key, keyShown: short(key), type: type, title: clip(x.title, LIMITS.maxFormTitle, true).text, description: clip(x.description, LIMITS.maxFormDesc).text,
          required: x.required === true, custom: x.custom === true && type !== 'text', options: options});
      }
    }
    return {fields: fields, answerable: ok};
  }

  // formAnswer は、form.resolved の answer を、表示用にする (キー → 文字列か文字列の配列の値だけ。型が違えば、その項目は出さない)。
  function formAnswer(v) {
    if (!isObj(v)) return null;
    const out = [];
    for (const k of Object.keys(v)) {
      if (out.length >= LIMITS.maxFields) break;
      const x = v[k];
      let text = null;
      if (typeof x === 'string') text = clip(x, LIMITS.maxAnswer).text;
      else if (Array.isArray(x) && x.every((e) => typeof e === 'string')) text = x.slice(0, LIMITS.maxAnswerValues).map((e) => clip(e, LIMITS.maxAnswer).text).join(', ');
      if (text !== null) out.push({key: k, text: text});
    }
    return out;
  }

  function createState() {
    return {
      generation: null,   // 最後の hello の世代 (変わったら、表示を作り直す)
      firstSeq: 0,
      durable: null,      // 最後の hello の durable (true・false。欠落・不正なら null)。false のときだけ、画面が注意を出す
      lastSeq: -1,
      items: [],          // 表示する項目 (古い順)
      byCall: new Map(),  // call_id と帰属の鍵 → tool の項目
      pending: new Map(), // request_id → permission の項目 (未決・決着の両方。項目が捨てられるまで)
      nextId: 1,
      trimmed: false,     // 古い項目を捨てた
      omitted: false,     // 履歴の欠け (hello の first_seq > 0、または、繋ぎ直しの間の欠け)
      ended: null,        // {exit} (end を受けた)
      ignored: 0,         // 表示しなかった Event の数 (agent.frame・未知の type・不正な形・重複)
      dirty: new Map(),   // id → 項目 (描画が要る)
      removed: [],        // 描画から外す項目の id
      reset: false,       // 表示を全部作り直す (世代が変わった)
      sessionStarted: false, // session.started を受けたか (権限モードを確認できたかの判断に使う)
      permissionMode: '', // claude の権限モード (session.started の permission_mode。default 以外は、tool が承認なしで実行されうる。無ければ '')
      turnActive: false,  // ターンの途中か (turn.started から turn.completed まで。送信欄を無効にする)
      turnCount: 0,       // 受けた turn.started の数 (送った指示が、受け付けられたかの確認に使う)
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
      const pinned = (it.kind === 'permission' || it.kind === 'form') && it.state === 'pending';
      if (dropped < excess && !pinned) {
        dropped++;
        state.dirty.delete(it.id);
        state.removed.push(it.id);
        if (it.kind === 'tool' && it.callKey) state.byCall.delete(it.callKey);
        if (it.kind === 'permission' || it.kind === 'form') state.pending.delete(it.requestId);
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
      state.turnActive = false;
      state.turnCount = 0;
      state.permissionMode = '';
      state.sessionStarted = false;
      state.reset = true;
    }
    state.generation = gen;
    state.firstSeq = first;
    state.durable = d.durable === true ? true : d.durable === false ? false : null; // boolean だけ (文字列・オブジェクトは、欠落と同じ)
    // 省略の検出は、resumed に依らない (ADR 0024 決定 7): first_seq > lastSeq + 1 は、まだ見ていない範囲が GC で消えた欠落。
    if (first > state.lastSeq + 1 && first > 0) state.omitted = true; // 溢れて捨てた分がある (繋ぎ直しの間の欠けも含む)
    return {ok: true, reset: state.reset};
  }

  function applyEnd(state, data) {
    const d = obj(data);
    state.ended = {exit: Number.isSafeInteger(d.exit) ? d.exit : null};
  }

  // normalizeOrigin は、Event の origin (ADR 0045: サブエージェントの帰属。{id, parent}) を、表示用にする。無い (undefined・null) だけが、メインのエージェント (null を返す)。
  // 型が違う・id が文字列でない/空/長すぎる・parent が文字列でない/長すぎる、は「帰属が壊れている」(bad): 不明な origin を、メインのものに見せない
  // (サブエージェント扱いで、id・parent は出さない)。値はエージェントの申告で、検証されていない (帰属はセキュリティの境界でなく、表示の手がかり)。
  // 値は short() を通す (制御文字・双方向制御は印)。id・parent は、比較の鍵 (key) にも使う (長さを限った生の値)。
  function normalizeOrigin(v) {
    if (v === undefined || v === null) return null;
    const bad = {bad: true, id: '', parent: '', hasParent: false, key: '!'};
    if (!isObj(v)) return bad;
    if (typeof v.id !== 'string' || v.id === '' || v.id.length > LIMITS.maxShort) return bad;
    const hasParent = v.parent !== undefined && v.parent !== null && v.parent !== '';
    if (hasParent && (typeof v.parent !== 'string' || v.parent.length > LIMITS.maxShort)) return bad;
    return {bad: false, id: short(v.id), parent: hasParent ? short(v.parent) : '', hasParent: hasParent, key: v.id + '\u0000' + (hasParent ? v.parent : '')};
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
    const origin = normalizeOrigin(e.origin); // null = メインのエージェント。サブエージェントの出力は、状態機械 (ターン・権限モード) を動かさない
    const main = origin === null;
    const put = (item) => { item.origin = origin; return add(state, item); };
    switch (e.type) {
      case 'session.started': {
        const mode = short(d.permission_mode);
        if (main) {
          state.sessionStarted = true;
          state.permissionMode = mode;
        }
        put({kind: 'session', agent: short(d.agent), model: short(d.model), cwd: short(d.cwd), permissionMode: mode});
        return true;
      }
      case 'turn.started': {
        if (main) {
          state.turnActive = true;
          state.turnCount++;
        }
        const t = clip(d.text, LIMITS.maxText);
        put({kind: 'user', text: t.text, cut: t.cut, total: t.total});
        return true;
      }
      case 'message.text': {
        const t = clip(d.text, LIMITS.maxText);
        put({kind: 'assistant', text: t.text, cut: t.cut, total: t.total});
        return true;
      }
      case 'tool.call': {
        const callId = short(d.call_id);
        const key = callId ? callId + '\u0000' + (main ? '' : origin.key) : ''; // 別の帰属の同じ call_id は、別の項目 (子が、メインの tool の枠を書き換えない)
        let it = key ? state.byCall.get(key) : undefined;
        const input = d.input === undefined || d.input === null ? {text: '', cut: false, total: 0} : clip(d.input, LIMITS.maxInput);
        if (!it) {
          it = put({kind: 'tool', callId: callId, name: '', toolKind: '', status: '', input: input, output: null, error: null, callKey: key});
          if (key) state.byCall.set(key, it);
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
        const key = callId ? callId + '\u0000' + (main ? '' : origin.key) : '';
        let it = key ? state.byCall.get(key) : undefined;
        if (!it) { // 呼び出しが、履歴から省略された
          it = put({kind: 'tool', callId: callId, name: '(履歴から省略)', toolKind: '', status: '', input: {text: '', cut: false, total: 0}, output: null, error: null, callKey: key});
          if (key) state.byCall.set(key, it);
        }
        it.status = short(d.status);
        if (d.output !== undefined && d.output !== null) it.output = clip(d.output, LIMITS.maxInput);
        if (d.error !== undefined && d.error !== null) it.error = clip(d.error, LIMITS.maxInput);
        touch(state, it);
        return true;
      }
      case 'permission.requested': {
        // request_id は、生の値のまま持つ (承認で、そのまま返す。表示用に、印をつけたり切ったりした値を、返さない: 見えない文字だけが違う
        // 2 つの要求が、同じ ID に見えて、表示した要求と別の要求を承認させられる)。表示は idShown。
        const rid = typeof d.request_id === 'string' ? d.request_id : '';
        if (!rid || rid.length > LIMITS.maxRequestID || state.pending.has(rid) || unresolved(state) >= LIMITS.maxPending) {
          state.ignored++;
          return false;
        }
        const shown = clip(rid, LIMITS.maxShort, true);
        const it = put({
          kind: 'permission', requestId: rid, idShown: shown.text, idPlain: !shown.cut && shown.text === rid,
          callId: short(d.call_id),
          toolName: short(d.tool_name), toolKind: short(d.kind),
          title: short(d.title), input: permissionInput(d.input),
          summary: typeof d.summary === 'string' ? clip(d.summary, LIMITS.maxSummary, true).text : '',
          details: normalizeDetails(d.details), detailsTruncated: d.details_truncated === true,
          contentHash: typeof d.content_hash === 'string' ? d.content_hash : '', // 生の値のまま (承認の応答に、そのまま写す。検査・加工しない)
          state: 'pending', by: '',
        });
        state.pending.set(rid, it);
        return true;
      }
      case 'form.requested': {
        // permission.requested と同じ名前空間・上限 (request_id は、サーバーでも共通)。key・選択肢の value・request_id は、生の値のまま持つ。
        const rid = typeof d.request_id === 'string' ? d.request_id : '';
        if (!rid || rid.length > LIMITS.maxRequestID || state.pending.has(rid) || unresolved(state) >= LIMITS.maxPending) {
          state.ignored++;
          return false;
        }
        const shown = clip(rid, LIMITS.maxShort, true);
        const f = normalizeFields(d.fields);
        const idPlain = !shown.cut && shown.text === rid;
        const it = put({
          kind: 'form', requestId: rid, idShown: shown.text, idPlain: idPlain,
          formKind: short(d.kind), title: clip(d.title, LIMITS.maxFormTitle, true).text, fields: f.fields, answerable: f.answerable && idPlain,
          contentHash: typeof d.content_hash === 'string' ? d.content_hash : '',
          state: 'pending', by: '', answer: null,
        });
        state.pending.set(rid, it);
        return true;
      }
      case 'form.resolved': {
        const rid = typeof d.request_id === 'string' ? d.request_id : '';
        const it = rid ? state.pending.get(rid) : undefined;
        if (!it || it.kind !== 'form') { // 要求が履歴から省略された・種類が違う決着。表示しない
          state.ignored++;
          return false;
        }
        it.state = FORM_OUTCOMES.has(d.outcome) ? d.outcome : 'unknown';
        it.by = short(d.by);
        it.answer = it.state === 'answered' ? formAnswer(d.answer) : null;
        touch(state, it);
        return true;
      }
      case 'permission.resolved': {
        const rid = typeof d.request_id === 'string' ? d.request_id : '';
        const it = rid ? state.pending.get(rid) : undefined;
        if (!it || it.kind !== 'permission') { // 要求が、履歴から省略された (決着だけが残った)。表示しない
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
        put({kind: 'usage', inputTokens: num(d.input_tokens), outputTokens: num(d.output_tokens), costUSD: num(d.cost_usd), contextWindow: num(d.context_window)});
        return true;
      }
      case 'turn.completed': {
        if (main) state.turnActive = false;
        put({kind: 'turn_end', stopReason: short(d.stop_reason), isError: d.is_error === true});
        return true;
      }
      case 'error': {
        const m = clip(d.message, LIMITS.maxInput);
        put({kind: 'error', status: Number.isFinite(d.status) ? d.status : null, message: m.text, cut: m.cut});
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

  const api = {isGeneration: (g) => typeof g === 'string' && GENERATION_RE.test(g), LIMITS: LIMITS, sanitize: sanitize, clip: clip, shownFully: shownFully, createState: createState, normalizeOrigin: normalizeOrigin, applyHello: applyHello, applyEnd: applyEnd, applyEvent: applyEvent, drain: drain};
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  else root.GoroChatCore = api;
})(typeof self !== 'undefined' ? self : this);
