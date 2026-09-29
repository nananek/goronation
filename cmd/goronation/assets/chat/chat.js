'use strict';
// chat.js: 構造化チャットの画面。EventSource で /s/{id}/events を受け、chat-core.js の状態から、DOM を作る。書く側 (指示の送信・終了・
// 権限の承認/拒否) は fetch (POST /s/{id}/message・permission・stop)。
//
// 描画は textContent と createElement だけ (エージェントの出力は敵対入力。innerHTML などの、文字列を HTML として解釈する API は使わない)。
// cmd/goronation/chat_ui_test.go の検査は、静的な見落としの防ぎにすぎない (動的な参照は見逃す): 安全の根拠は、エージェント由来の値が、
// 属性名・タグ名・URL・コード・class・id に入らないことで、実行時の試験 (chat.test.js) と、実ブラウザでの確認で確かめる。
//
// 承認 (permission ダイアログ) の規則 (ADR 0016):
//   - ダイアログは、kind が permission で、state が pending の項目からだけ作る。request_id は、項目 (JS のオブジェクト) の値を、クロージャで使う
//     (DOM の属性から読み戻さない)。
//   - 世代は、検証済みの state.generation (chat-core.js の applyHello が受けた値) の写しを、ダイアログを見せた時点で持ち、それだけを送る。
//     世代が無い間・接続が切れている間・世代が変わった後は、送らない。
//   - 「決着」の表示は、permission.resolved を受けた項目の state からだけ。ボタンを押しただけでは、承認済みと表示しない。
(function (root) {
  const core = typeof require === 'function' && typeof module !== 'undefined' ? require('./chat-core.js') : root.GoroChatCore;

  const RENDER_DELAY_MS = 50;
  const RETRY_FIRST_MS = 1000;
  const RETRY_MAX_MS = 30000;
  const GIVE_UP_AFTER = 10; // hello に届かない失敗が、これだけ続いたら、自動の再接続をやめる (ボタンで再開)
  const STOP_ARM_MS = 4000; // 「終了」を押してから、もう一度押すまでの猶予 (押し間違いの防ぎ)
  const MAX_MESSAGE_BYTES = 64 * 1024; // serve の text の上限 (chat.MaxMessageBytes)
  const BASE_RE = /^\/s\/[0-9]{8}-[0-9]{6}-[0-9a-f]{6}$/;

  // create は、UI を組み立てる。env は、document・EventSource・fetch・setTimeout (試験で差し替える)。
  function create(env) {
    const doc = env.document;
    const state = core.createState();
    const log = doc.getElementById('log');
    const statusEl = doc.getElementById('status');
    const notices = doc.getElementById('notices');
    const dialogsEl = doc.getElementById('dialogs');
    const dialogsHead = doc.getElementById('dialogs-head');
    const msgEl = doc.getElementById('msg');
    const sendBtn = doc.getElementById('send');
    const stopBtn = doc.getElementById('stop');
    const writeStatus = doc.getElementById('write-status');
    const els = new Map(); // 項目の id → 要素
    const dialogs = new Map(); // 項目 (オブジェクト) → ダイアログ {el, gen, busy, done, approve, deny, note}
    let source = null;
    let connected = false; // 検証済みの hello を受けて、接続が生きている
    let failures = 0;
    let retryMs = RETRY_FIRST_MS;
    let retryTimer = null;
    let renderTimer = null;
    let stopped = false;
    let url = '';
    let base = '';
    let sending = false;
    let awaitTurn = -1; // 指示を送って、200 を受けた時の turnCount。これより増える (turn.started を受ける) まで、送信欄は無効
    let stopArmed = false;
    let stopRequested = false;

    function el(tag, cls, text) {
      const e = doc.createElement(tag);
      if (cls) e.className = cls;
      if (text !== undefined && text !== null) e.textContent = text;
      return e;
    }

    function setStatus(msg) {
      statusEl.textContent = msg;
    }

    function setWriteStatus(msg) {
      writeStatus.textContent = msg;
    }

    function clipNote(c) {
      if (!c.cut) return '';
      return c.total === null ? '\n… (以降は表示しない)' : '\n… (全体 ' + c.total + ' 文字のうち、先頭だけ表示)';
    }

    function safeClass(v) {
      return /^[a-z_]{1,20}$/.test(v) ? v : 'other';
    }

    // fill は、項目 it の要素 e の中身を、作り直す (textContent だけ)。
    function fill(e, it) {
      e.textContent = '';
      switch (it.kind) {
        case 'session':
          e.className = 'item item-session';
          e.appendChild(el('span', 'label', 'セッション開始'));
          e.appendChild(el('span', 'meta', [it.agent, it.model, it.cwd].filter(Boolean).join('  ')));
          break;
        case 'user':
          e.className = 'item item-user';
          e.appendChild(el('div', 'label', 'あなた'));
          e.appendChild(el('div', 'text', it.text + clipNote(it)));
          break;
        case 'assistant':
          e.className = 'item item-assistant';
          e.appendChild(el('div', 'label', 'エージェント'));
          e.appendChild(el('div', 'text', it.text + clipNote(it)));
          break;
        case 'tool': {
          e.className = 'item item-tool status-' + safeClass(it.status);
          e.appendChild(el('div', 'label', 'tool: ' + (it.name || '?') + (it.toolKind ? ' (' + it.toolKind + ')' : '') + '  [' + (it.status || '?') + ']'));
          const det = el('details', 'detail');
          det.open = it.open === true; // 更新で、開いた入力・出力が閉じない
          det.addEventListener('toggle', () => { it.open = det.open === true; });
          det.appendChild(el('summary', null, '入力・出力'));
          det.appendChild(el('pre', 'input', it.input.text + clipNote(it.input)));
          if (it.output) det.appendChild(el('pre', 'output', it.output.text + clipNote(it.output)));
          if (it.error) det.appendChild(el('pre', 'error-text', it.error.text + clipNote(it.error)));
          e.appendChild(det);
          break;
        }
        case 'permission': {
          e.className = 'item item-permission state-' + safeClass(it.state);
          e.appendChild(el('div', 'label', '権限の要求: ' + (it.toolName || '?') + (it.title ? ' — ' + it.title : '')));
          if (it.state === 'pending') {
            e.appendChild(el('div', 'meta', '未決 (下の枠で、許可・拒否する)'));
          } else {
            e.appendChild(el('pre', 'input', it.input.text + clipNote(it.input)));
            e.appendChild(el('div', 'meta', '決着: ' + it.state + (it.by ? ' (' + it.by + ')' : '')));
          }
          break;
        }
        case 'usage': {
          e.className = 'item item-usage';
          const parts = [];
          if (it.inputTokens !== null) parts.push('入力 ' + it.inputTokens);
          if (it.outputTokens !== null) parts.push('出力 ' + it.outputTokens);
          if (it.costUSD !== null) parts.push('$' + it.costUSD);
          if (it.contextWindow !== null) parts.push('窓 ' + it.contextWindow);
          e.appendChild(el('span', 'meta', '使用量: ' + (parts.join('  ') || '-')));
          break;
        }
        case 'turn_end':
          e.className = 'item item-turn-end' + (it.isError ? ' is-error' : '');
          e.appendChild(el('span', 'meta', 'ターン終了: ' + (it.stopReason || '-') + (it.isError ? ' (失敗)' : '')));
          break;
        case 'error':
          e.className = 'item item-error';
          e.appendChild(el('div', 'label', 'エラー' + (it.status !== null ? ' (' + it.status + ')' : '')));
          e.appendChild(el('div', 'text', it.message + clipNote(it)));
          break;
        default:
          e.className = 'item';
      }
    }

    function nearBottom() {
      const s = env.scroller;
      if (!s) return true;
      return s.scrollHeight - s.scrollTop - s.clientHeight < 80;
    }

    function renderNotices() {
      // 再接続のボタンは、この関数の外で足す。ここでは、ボタン以外の注意書きだけを作り直す。
      for (const c of Array.from(notices.children)) if (c.tagName !== 'button') notices.removeChild(c);
      if (state.omitted || state.trimmed) notices.appendChild(el('div', 'notice', '古い分は省略している'));
      if (state.ended) notices.appendChild(el('div', 'notice notice-end', '終了' + (state.ended.exit !== null ? ' (exit ' + state.ended.exit + ')' : '')));
    }

    // ---- 送信欄・終了ボタン ----

    function messageBytes(t) {
      return env.TextEncoder ? new env.TextEncoder().encode(t).length : t.length * 3;
    }

    function updateComposer() {
      const text = typeof msgEl.value === 'string' ? msgEl.value : '';
      const blocked = !connected || state.ended !== null || state.turnActive || sending || awaitTurn >= 0 || stopRequested;
      sendBtn.disabled = blocked || text.trim() === '';
      msgEl.disabled = state.ended !== null;
      stopBtn.disabled = state.ended !== null || stopRequested || !connected;
      stopBtn.textContent = stopArmed ? '本当に終了する' : '終了';
    }

    // describe は、書き込みの応答の status を、画面に出す文にする。
    function describe(status, what) {
      switch (status) {
        case 400: return what + ': 内容が不正 (空・不正な文字・世代の欠落)';
        case 403: return what + ': 拒否された (Origin・ログインを確かめる)';
        case 404: return what + ': チャットが起動していない、または対象が無い';
        case 409: return what + ': 状態が合わない (ターン中・終了済み・すでに決着済み・別の起動)';
        case 413: return what + ': 大きすぎる';
        case 415: return what + ': Content-Type の誤り';
        case 502: return what + ': チャットに繋がらない';
        case 504: return what + ': チャットが応答しない';
        default: return what + ': 失敗した (' + status + ')';
      }
    }

    // post は、base + path へ JSON を POST し、status を返す (通信の失敗は 0)。
    async function post(path, body) {
      try {
        const resp = await env.fetch(base + path, {
          method: 'POST', credentials: 'same-origin', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(body),
        });
        return resp.status;
      } catch (e) {
        return 0;
      }
    }

    async function sendMessage() {
      if (sendBtn.disabled || sending) return;
      const text = msgEl.value;
      if (text.trim() === '') return;
      if (messageBytes(text) > MAX_MESSAGE_BYTES) {
        setWriteStatus('指示が大きすぎる (64 KiB まで)');
        return;
      }
      sending = true;
      updateComposer();
      setWriteStatus('送信中');
      const turnsBefore = state.turnCount;
      const status = await post('/message', {text: text});
      sending = false;
      if (status === 200) {
        msgEl.value = '';
        setWriteStatus('送った。エージェントの応答を待っている');
        if (state.turnCount === turnsBefore) awaitTurn = turnsBefore; // turn.started が届くまで、次の送信を止める
      } else {
        setWriteStatus(status === 0 ? '指示: 送れなかった (通信の失敗)' : describe(status, '指示'));
      }
      updateComposer();
    }

    function stopClicked() {
      if (stopBtn.disabled) return;
      if (!stopArmed) {
        stopArmed = true;
        env.setTimeout(() => { stopArmed = false; updateComposer(); }, STOP_ARM_MS);
        updateComposer();
        return;
      }
      stopArmed = false;
      stopRequested = true;
      updateComposer();
      post('/stop', {}).then((status) => {
        if (status === 200) {
          setWriteStatus('終了を要求した');
        } else {
          stopRequested = false;
          setWriteStatus(status === 0 ? '終了: 送れなかった (通信の失敗)' : describe(status, '終了'));
        }
        updateComposer();
      });
    }

    // ---- 権限ダイアログ ----

    // 見た範囲の追跡: 枠 (pre) が、スクロールの各位置で、画面に出していた範囲 [scrollTop, scrollTop + clientHeight] の和が、全体 [0, scrollHeight] を
    // 覆うまで、許可させない。末尾に一度着いただけ (End キー・フリック・スクロールバーのドラッグの一発のジャンプ) では、途中を見ていない。
    // スクロールの 1 歩が clientHeight を超えると、その間は、未表示のまま残る (戻って見るまで、許可できない)。
    const SEEN_TOLERANCE = 2;

    // preOnScreen は、枠が、下の領域 (スクロールする) の中で、画面に出ているか。DOM に、位置の API が無い環境 (試験) では、出ているとみなす。
    function preOnScreen(d) {
      if (typeof d.pre.getBoundingClientRect !== 'function' || typeof dialogsEl.getBoundingClientRect !== 'function') return true;
      const r = d.pre.getBoundingClientRect();
      const c = dialogsEl.getBoundingClientRect();
      return r.top >= c.top - 1 && r.bottom <= c.bottom + 1;
    }

    // recordView は、いまの枠の表示範囲を、見た範囲に足す (連続する・重なる範囲は、まとめる)。
    function recordView(d) {
      if (!preOnScreen(d) || d.pre.clientHeight <= 0) return;
      let a = d.pre.scrollTop;
      let b = a + d.pre.clientHeight;
      const rest = [];
      for (const r of d.seen) {
        if (r[1] < a - SEEN_TOLERANCE || r[0] > b + SEEN_TOLERANCE) {
          rest.push(r);
        } else {
          a = Math.min(a, r[0]);
          b = Math.max(b, r[1]);
        }
      }
      rest.push([a, b]);
      d.seen = rest;
    }

    // hiddenPart は、input の枠が、スクロールしないと見えない部分を持ち、その全体を、まだ画面に出していないか。枠の高さは限ってあるので、字数が上限以内でも起きる
    // (エージェントが、key の順を決められる: 危険な内容を、途中や末尾に置ける)。
    function hiddenPart(d) {
      if (d.pre === null || d.pre.scrollHeight <= d.pre.clientHeight + 1) return false;
      recordView(d);
      return !(d.seen.length === 1 && d.seen[0][0] <= SEEN_TOLERANCE && d.seen[0][1] >= d.pre.scrollHeight - SEEN_TOLERANCE);
    }

    function approvable(item, d) {
      // input を全部は表示できない (打ち切った・枠の見えない部分を見ていない)・request_id が見た目どおりでない (見えない文字・長さ) ときは、許可させない (拒否だけ)。
      return item.input.cut === false && item.idPlain === true && !hiddenPart(d);
    }

    function dialogCanAnswer(d, item) {
      return connected && !d.busy && !d.done && state.ended === null && item.state === 'pending' &&
        d.gen !== null && d.gen === state.generation && core.isGeneration(d.gen);
    }

    function refreshDialog(item, d) {
      const can = dialogCanAnswer(d, item);
      d.approve.disabled = !can || !approvable(item, d);
      d.deny.disabled = !can;
      if (d.hint) d.hint.textContent = item.input.cut === false && item.idPlain === true && hiddenPart(d) ? 'input が枠に収まらない。上から下まで、途切れなくスクロールして全部を表示すると、許可できる (一気に飛ばすと、間が未表示のまま残る)' : '';
    }

    async function answer(item, d, outcome) {
      // クロージャの item (JS のオブジェクト) の値だけを使う。世代は、ダイアログを見せた時点の写し (d.gen)。それが、いまの検証済みの世代と違えば送らない。
      if (!dialogCanAnswer(d, item)) return;
      if (outcome === 'allow_once' && !approvable(item, d)) return;
      const gen = d.gen;
      const requestId = item.requestId;
      d.busy = true;
      refreshDialog(item, d);
      d.note.textContent = '送信中';
      const status = await post('/permission', {generation: gen, request_id: requestId, outcome: outcome});
      d.busy = false;
      if (status === 200) {
        d.done = true; // 決着の表示は、permission.resolved を受けてから (ここでは出さない)
        d.note.textContent = '送信した。決着 (permission.resolved) を待っている';
      } else if (status === 404 || status === 409) {
        d.done = true;
        d.note.textContent = status === 409 ? 'すでに決着済み、または別の起動の画面 (応答は受け付けられなかった)' : 'この要求は、もう無い';
      } else {
        d.note.textContent = status === 0 ? '送れなかった (通信の失敗)。もう一度押せる' : describe(status, '応答') + '。もう一度押せる';
      }
      refreshDialog(item, d);
    }

    function buildDialog(item) {
      const d = {el: el('div', 'dialog'), gen: state.generation, busy: false, done: false, approve: null, deny: null, note: null, hint: null, pre: null, seen: []};
      d.el.appendChild(el('div', 'label', '権限の要求'));
      d.el.appendChild(el('div', 'dialog-tool', 'tool: ' + (item.toolName || '(名前なし)') + (item.toolKind ? ' (' + item.toolKind + ')' : '')));
      if (item.title) d.el.appendChild(el('div', 'dialog-title', '説明 (エージェントの自己申告。検証されていない): ' + item.title));
      d.approve = el('button', 'approve', '許可 (今回だけ)');
      d.deny = el('button', 'deny', '拒否');
      d.approve.addEventListener('click', () => { answer(item, d, 'allow_once'); });
      d.deny.addEventListener('click', () => { answer(item, d, 'reject_once'); });
      const row = el('div', 'dialog-buttons');
      row.appendChild(d.deny); // 拒否が先 (左)。許可は、離して置く (誤クリックを減らす)
      row.appendChild(d.approve);
      d.el.appendChild(row); // ボタンは、input より上 (巨大な input が、ボタンを押し出さない)
      d.note = el('div', 'meta', '');
      d.el.appendChild(d.note);
      if (item.input.cut || item.idPlain !== true) {
        d.el.appendChild(el('div', 'warn', item.input.cut ? 'input が長すぎる・深すぎる・形が不正で、全部は表示できないので、許可できない (拒否だけ)' : 'request_id に見えない文字・長さがあり、許可できない (拒否だけ)'));
      }
      d.hint = el('div', 'warn', '');
      d.el.appendChild(d.hint);
      d.el.appendChild(el('div', 'meta', 'request_id: ' + item.idShown));
      d.pre = el('pre', 'input', item.input.text + clipNote(item.input));
      d.pre.addEventListener('scroll', () => { // スクロールのたびに、見た範囲を足す (一発のジャンプでは、間が残る)
        recordView(d);
        refreshDialog(item, d);
      });
      d.el.appendChild(d.pre);
      return d;
    }

    // syncDialogs は、未決の権限要求 (項目) だけから、ダイアログを作り・外し・有効/無効を更新する。
    function syncDialogs() {
      for (const [item, d] of Array.from(dialogs.entries())) {
        if (item.state !== 'pending' || state.pending.get(item.requestId) !== item) { // 決着した・履歴から外れた・世代が変わった
          dialogsEl.removeChild(d.el);
          dialogs.delete(item);
        }
      }
      if (connected && state.generation !== null) {
        for (const item of state.pending.values()) {
          if (item.kind === 'permission' && item.state === 'pending' && !dialogs.has(item)) {
            const d = buildDialog(item);
            dialogs.set(item, d);
            dialogsEl.appendChild(d.el);
          }
        }
      }
      for (const [item, d] of dialogs) refreshDialog(item, d);
      // 未決が複数あるとき、枠の中をスクロールして、全部に答える (枠の高さは限ってある)。件数を、枠の外に出す。
      dialogsHead.textContent = dialogs.size === 0 ? '' : '未決の権限要求: ' + dialogs.size + ' 件' + (dialogs.size > 1 ? ' (下の枠の中をスクロールして、すべてに答える)' : '');
    }

    function render() {
      renderTimer = null;
      const stick = nearBottom();
      const d = core.drain(state);
      if (d.reset) {
        log.textContent = '';
        els.clear();
        awaitTurn = -1;
      }
      for (const id of d.removes) {
        const e = els.get(id);
        if (e) {
          log.removeChild(e);
          els.delete(id);
        }
      }
      for (const it of d.upserts) {
        let e = els.get(it.id);
        if (!e) {
          e = doc.createElement('div');
          els.set(it.id, e);
          log.appendChild(e);
        }
        fill(e, it);
      }
      if (awaitTurn >= 0 && state.turnCount > awaitTurn) awaitTurn = -1;
      renderNotices();
      syncDialogs();
      updateComposer();
      if (stick && env.scroller) env.scroller.scrollTop = env.scroller.scrollHeight;
    }

    function scheduleRender() {
      if (renderTimer === null) renderTimer = env.setTimeout(render, RENDER_DELAY_MS);
    }

    function parse(s) {
      try {
        return JSON.parse(s);
      } catch (e) {
        return null;
      }
    }

    function closeSource() {
      if (source) {
        source.close();
        source = null;
      }
      connected = false;
    }

    function scheduleRetry() {
      scheduleRender(); // 接続が切れた: ダイアログ・送信欄を無効にする
      if (stopped || retryTimer !== null) return;
      if (failures >= GIVE_UP_AFTER) {
        setStatus('接続できない (チャットが起動していない可能性)。再接続を押す');
        showRetryButton();
        return;
      }
      setStatus('切断。' + Math.round(retryMs / 1000) + ' 秒後に再接続する');
      retryTimer = env.setTimeout(() => {
        retryTimer = null;
        connect();
      }, retryMs);
      retryMs = Math.min(retryMs * 2, RETRY_MAX_MS);
    }

    function showRetryButton() {
      const b = el('button', 'retry', '再接続');
      b.addEventListener('click', () => {
        notices.removeChild(b);
        failures = 0;
        retryMs = RETRY_FIRST_MS;
        connect();
      });
      notices.appendChild(b);
    }

    function connect() {
      if (stopped) return;
      closeSource();
      setStatus('接続中');
      const es = new env.EventSource(url);
      source = es;
      es.addEventListener('hello', (m) => {
        if (source !== es) return;
        const data = parse(m.data);
        if (data === null) return;
        if (!core.applyHello(state, data).ok) { // 世代の無い・壊れた hello: この接続は使わない (承認を出さない)
          closeSource();
          failures++;
          scheduleRetry();
          return;
        }
        failures = 0;
        retryMs = RETRY_FIRST_MS;
        connected = true;
        awaitTurn = -1; // 繋ぎ直した: turn.started を取りこぼしていても、送信欄が固まらない (Snapshot が、状態を作り直す)
        setStatus('接続済み');
        scheduleRender();
      });
      es.addEventListener('message', (m) => {
        if (source !== es || !connected) return; // 検証済みの hello の前の Event は、使わない
        const ev = parse(m.data);
        if (ev !== null && core.applyEvent(state, ev)) scheduleRender();
      });
      es.addEventListener('end', (m) => {
        if (source !== es) return;
        core.applyEnd(state, parse(m.data));
        stopped = true; // 終わった会話: 再接続しない
        closeSource();
        setStatus('終了');
        scheduleRender();
      });
      es.addEventListener('error', () => {
        if (source !== es) return;
        // EventSource の自動の再接続に任せず、自前で間隔を空ける (503・404 は、status が見えず、閉じるだけ。素の EOF は、すぐ繋ぎ直す)。
        closeSource();
        failures++;
        scheduleRetry();
      });
    }

    function start(chatURL, baseURL) {
      if (typeof baseURL !== 'string' || !BASE_RE.test(baseURL)) throw new Error('壊れた base');
      url = chatURL;
      base = baseURL;
      sendBtn.addEventListener('click', () => { sendMessage(); });
      stopBtn.addEventListener('click', stopClicked);
      msgEl.addEventListener('input', updateComposer);
      msgEl.addEventListener('keydown', (ev) => {
        if (ev.key === 'Enter' && (ev.ctrlKey || ev.metaKey)) {
          ev.preventDefault();
          sendMessage();
        }
      });
      updateComposer();
      connect();
    }

    return {start: start, state: state, render: render, connect: connect, elements: els, dialogs: dialogs};
  }

  const api = {create: create, RENDER_DELAY_MS: RENDER_DELAY_MS, RETRY_FIRST_MS: RETRY_FIRST_MS, RETRY_MAX_MS: RETRY_MAX_MS, GIVE_UP_AFTER: GIVE_UP_AFTER, STOP_ARM_MS: STOP_ARM_MS};
  if (typeof module !== 'undefined' && module.exports) {
    module.exports = api;
  } else {
    root.GoroChat = api;
    const m = /^\/s\/([0-9]{8}-[0-9]{6}-[0-9a-f]{6})\/chat$/.exec(root.location.pathname);
    if (m && root.document.getElementById('log')) {
      const ui = create({
        document: root.document, EventSource: root.EventSource, fetch: root.fetch.bind(root), setTimeout: root.setTimeout.bind(root),
        TextEncoder: root.TextEncoder, scroller: root.document.scrollingElement,
      });
      ui.start('/s/' + m[1] + '/events', '/s/' + m[1]);
    }
  }
})(typeof self !== 'undefined' ? self : this);
