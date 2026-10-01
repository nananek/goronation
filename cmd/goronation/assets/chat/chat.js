'use strict';
// chat.js: 構造化チャットの画面。EventSource で /s/{id}/events を受け、chat-core.js の状態から、DOM を作る。書く側 (指示の送信・終了・
// 権限の承認/拒否) は fetch (POST /s/{id}/message・permission・stop)。
//
// 描画は textContent と createElement だけ (エージェントの出力は敵対入力。innerHTML などの、文字列を HTML として解釈する API は使わない)。
// cmd/goronation/chat_ui_test.go の検査は、静的な見落としの防ぎにすぎない (動的な参照は見逃す): 安全の根拠は、エージェント由来の値が、
// 属性名・タグ名・URL・コード・class・id に入らないことで、実行時の試験 (chat.test.js) と、実ブラウザでの確認で確かめる。
//
// 承認 (permission ダイアログ) の規則 (ADR 0016・0048):
//   - ダイアログは、kind が permission で、state が pending の項目からだけ作る。request_id は、項目 (JS のオブジェクト) の値を、クロージャで使う
//     (DOM の属性から読み戻さない)。
//   - 世代は、検証済みの state.generation (chat-core.js の applyHello が受けた値) の写しを、ダイアログを見せた時点で持ち、それだけを送る。
//     世代が無い間・接続が切れている間・世代が変わった後は、送らない。
//   - 「決着」の表示は、permission.resolved を受けた項目の state からだけ。ボタンを押しただけでは、承認済みと表示しない。
//   - 許可できるのは、詳細 (details) があれば詳細の全項目を、なければ input を、切らずに、見た範囲・時間つきで見せているときだけ (ADR 0048 決定 1)。
//   - 応答には、受け取った content_hash を、そのまま写す (照合はサーバー。ADR 0042・0046)。
// form (質問) のダイアログの規則 (ADR 0048): form.requested の未決の項目からだけ作る。エージェントの文は textContent だけで出し、「回答はエージェントに渡る」を
// 常に出す。回答のキーは、項目が持つ生の key・選択肢の value (表示用の文字列でも、DOM から読み戻した値でもない)。送るのは POST /form だけ。
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
    let formSeq = 0; // form ダイアログごとの、ラジオの name の接頭辞 (同時に複数の form があっても、選択が混ざらない)

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

    // 帰属 (origin。ADR 0045): エージェントの申告で、検証されていない (帰属はセキュリティの境界でなく、表示の手がかり)。null はメインのエージェント。
    // 壊れた origin (型違い・長すぎる・id が無い) は、メインに見せない (サブエージェント扱い。値は出さない)。
    const ORIGIN_NOTE = '(エージェントの申告。検証されていない)';
    const ORIGIN_ID_SHOWN = 16; // ラベルに出す id の長さ (全部は、ダイアログの要求元に出す)

    function originLabel(o) {
      if (o.bad) return 'サブエージェント (帰属が壊れている)';
      return 'サブエージェント ' + (o.id.length > ORIGIN_ID_SHOWN ? o.id.slice(0, ORIGIN_ID_SHOWN) + '…' : o.id);
    }

    // originSource は、権限・form のダイアログの「要求元」の行 (メインにも出す: 出さないことが、メインの印にならないように)。
    function originSource(o) {
      if (o === null) return '要求元: メインのエージェント ' + ORIGIN_NOTE;
      if (o.bad) return '要求元: サブエージェント (帰属が壊れていて、どこから来たか分からない。メインのエージェントとは見なさない) ' + ORIGIN_NOTE;
      return '要求元: サブエージェント ' + o.id + ' (起動した tool 呼び出し: ' + (o.hasParent ? o.parent : '不明') + ') ' + ORIGIN_NOTE;
    }

    function sourceRow(o) {
      return el('div', o === null ? 'dialog-origin' : 'dialog-origin dialog-origin-sub', originSource(o));
    }

    // fill は、項目 it の要素 e の中身を、作り直す (textContent だけ)。
    function fill(e, it) {
      e.textContent = '';
      if (it.origin) e.appendChild(el('div', 'meta origin', originLabel(it.origin) + ' ' + ORIGIN_NOTE)); // 先頭の行 (class は下で足す)
      switch (it.kind) {
        case 'session':
          e.className = 'item item-session';
          e.appendChild(el('span', 'label', 'セッション開始'));
          e.appendChild(el('span', 'meta', [it.agent, it.model, it.cwd, it.permissionMode ? '権限モード: ' + it.permissionMode : ''].filter(Boolean).join('  ')));
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
          e.appendChild(el('div', 'label', '権限の要求: ' + (it.summary || it.toolName || '?') + (it.title ? ' — ' + it.title : '')));
          if (it.state === 'pending') {
            e.appendChild(el('div', 'meta', '未決 (下の枠で、許可・拒否する)'));
          } else {
            if (it.details.has) e.appendChild(el('pre', 'input', it.details.text + (it.detailsTruncated ? '\n… (詳細が長すぎて、全部は表示していない)' : '')));
            else e.appendChild(el('pre', 'input', it.input.text + clipNote(it.input)));
            e.appendChild(el('div', 'meta', '決着: ' + it.state + (it.by ? ' (' + it.by + ')' : '')));
          }
          break;
        }
        case 'form': {
          e.className = 'item item-form state-' + safeClass(it.state);
          e.appendChild(el('div', 'label', '質問 (エージェントから)' + (it.title ? ': ' + it.title : '')));
          if (it.state === 'pending') {
            e.appendChild(el('div', 'meta', '未決 (下の枠で、回答する)'));
            break;
          }
          for (const f of it.fields) {
            e.appendChild(el('div', 'form-q', (f.title || f.keyShown) + (f.description ? ' — ' + f.description : '')));
            const a = it.answer === null ? undefined : it.answer.find((x) => x.key === f.key);
            if (a) e.appendChild(el('div', 'form-a', '→ ' + a.text));
          }
          e.appendChild(el('div', 'meta', '決着: ' + it.state + (it.by ? ' (' + it.by + ')' : '')));
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
      if (it.origin) { // サブエージェントの項目: ラベル・インデント・色 (class は固定の語だけ)
        e.className += it.origin.bad ? ' item-sub item-sub-bad' : ' item-sub';
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
      if (state.durable === false) notices.appendChild(el('div', 'notice', '履歴は、ディスクに残らない。再読み込みで、古い分が欠けることがある')); // 明示の false だけ (欠落では出さない)。固定の文
      // 権限モードが default でない (auto・acceptEdits など): tool が、人間の承認なしで実行されうる。権限ダイアログは出ない (PR⑧ の実物の確認で、claude 2.1.285 の既定が auto と分かった)。
      // session.started に権限モードが無い・空: 確認できない (claude が名前を変えた・出さない版。承認が働くかは分からない)。
      if (state.sessionStarted && state.permissionMode === '') notices.appendChild(el('div', 'notice notice-danger', '警告: 権限モードを確認できない。tool が、人間の承認なしで実行されうる (権限ダイアログが出るとは限らない)'));
      if (state.permissionMode !== '' && state.permissionMode !== 'default') notices.appendChild(el('div', 'notice notice-danger', '警告: 権限モードが "' + state.permissionMode + '"。tool が、人間の承認なしで実行されうる (権限ダイアログは出ない)'));
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
    // 失敗 (400 以上) の応答の本文の error は、lastCode に入れる (表示には使わず、決まった語との比較だけ)。
    let lastCode = '';
    async function post(path, body) {
      lastCode = '';
      try {
        const resp = await env.fetch(base + path, {
          method: 'POST', credentials: 'same-origin', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(body),
        });
        if (resp.status >= 400 && typeof resp.json === 'function') {
          try {
            const j = await resp.json();
            if (j !== null && typeof j === 'object' && typeof j.error === 'string') lastCode = j.error;
          } catch (e) {
            lastCode = '';
          }
        }
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

    // 見た範囲と、見た時間の追跡: input の枠 (pre) の内容を、CELL_PX ごとの区画に分け、各区画が、画面に完全に出ていた時間を、累積する。
    // すべての区画が DWELL_MS 以上になるまで、許可させない。
    //   - 末尾に着いただけ (End キー・フリック・スクロールバーのドラッグの一発のジャンプ) では、途中の区画が 0 のまま残る。
    //   - 速いスクロール (smooth scroll・連続の PageDown) は、区画が画面に出ている時間が短く、条件に届かない。
    // これは、意図的な高速操作の抑止で、完全な防御ではない (人間が、実際に読んだかは、分からない。待てば条件は満たせる。M2 の内容ハッシュ束縛とは別の問題)。
    const CELL_PX = 16;
    const DWELL_MS = 500; // 各区画を、画面に出しておく時間
    const TICK_MS = 100; // 静止している間も、時間を数える間隔
    const CREDIT_CAP_MS = 300; // 1 回に数える時間の上限 (背景のタブ・重い処理で、タイマーが遅れた間は、数えない)
    const SEEN_TOLERANCE = 2;

    function nowMs() {
      return env.now ? env.now() : Date.now();
    }

    // preOnScreen は、枠が、下の領域 (スクロールする) の中で、画面に出ているか。DOM に、位置の API が無い環境 (試験) では、出ているとみなす。
    function preOnScreen(d) {
      if (doc.hidden === true) return false; // 背景のタブ
      if (typeof d.pre.getBoundingClientRect !== 'function' || typeof dialogsEl.getBoundingClientRect !== 'function') return true;
      const r = d.pre.getBoundingClientRect();
      const c = dialogsEl.getBoundingClientRect();
      return r.top >= c.top - 1 && r.bottom <= c.bottom + 1;
    }

    // observe は、直前の表示範囲に、経過時間 (上限つき) を足し、いまの表示範囲を、次の起点にする。スクロールのたびと、TICK_MS ごとに呼ぶ。
    function observe(d) {
      const t = nowMs();
      const n = Math.max(1, Math.ceil(d.pre.scrollHeight / CELL_PX));
      while (d.cells.length < n) d.cells.push(0);
      d.cells.length = n;
      if (d.view) {
        const dt = Math.min(t - d.view.t, CREDIT_CAP_MS);
        if (dt > 0) {
          for (let i = 0; i < n; i++) {
            const top = i * CELL_PX;
            const bottom = Math.min((i + 1) * CELL_PX, d.pre.scrollHeight);
            if (top >= d.view.a - SEEN_TOLERANCE && bottom <= d.view.b + SEEN_TOLERANCE) d.cells[i] += dt; // 完全に、画面に出ていた区画だけ
          }
        }
      }
      d.view = preOnScreen(d) && d.pre.clientHeight > 0 ? {a: d.pre.scrollTop, b: d.pre.scrollTop + d.pre.clientHeight, t: t} : null;
    }

    function dwellProgress(d) {
      if (d.cells.length === 0) return 0;
      let done = 0;
      for (const c of d.cells) if (c >= DWELL_MS) done++;
      return done / d.cells.length;
    }

    // hiddenPart は、input の枠が、スクロールしないと見えない部分を持ち、その全体を、まだ十分な時間、画面に出していないか。枠の高さは限ってあるので、字数が
    // 上限以内でも起きる (エージェントが、key の順を決められる: 危険な内容を、途中や末尾に置ける)。
    function hiddenPart(d) {
      if (d.pre === null) return false;
      // 溢れない枠も、画面に完全に出ている時間を数える (許可ボタンが見えていても、枠が領域の外に押し出されることがある)。位置の API が無い環境 (試験) だけ、免除。
      if (d.pre.scrollHeight <= d.pre.clientHeight + 1 && (typeof d.pre.getBoundingClientRect !== 'function' || typeof dialogsEl.getBoundingClientRect !== 'function')) return false;
      observe(d);
      if (dwellProgress(d) >= 1) return false;
      scheduleTick(d);
      return true;
    }

    // scheduleTick は、静止している間も、時間が経つように、TICK_MS 後に、ダイアログを更新する (未決で、まだ条件を満たさない間だけ)。
    function scheduleTick(d) {
      if (d.tick !== null || d.removed) return;
      d.tick = env.setTimeout(() => {
        d.tick = null;
        if (!d.removed) refreshDialog(d.item, d);
      }, TICK_MS);
    }

    function approvable(item, d) {
      // 詳細 (なければ input) を全部は表示できない (打ち切った・details_truncated・枠の見えない部分を見ていない)・request_id が見た目どおりでない
      // (見えない文字・長さ) ときは、許可させない (拒否だけ)。
      return core.shownFully(item) && !hiddenPart(d);
    }

    function dialogCanAnswer(d, item) {
      return connected && !d.busy && !d.done && state.ended === null && item.state === 'pending' &&
        d.gen !== null && d.gen === state.generation && core.isGeneration(d.gen);
    }

    function refreshDialog(item, d) {
      const can = dialogCanAnswer(d, item);
      d.approve.disabled = !can || !approvable(item, d);
      d.deny.disabled = !can;
      if (d.hint) d.hint.textContent = core.shownFully(item) && hiddenPart(d) ? '内容の枠が、画面に完全に出ていない・収まらない。枠の全体を、上から下まで、途切れなく、どの部分も 0.5 秒以上、画面に出すと、許可できる (いま ' + Math.floor(dwellProgress(d) * 100) + '%。一気に飛ばす・速く送ると、足りない)' : '';
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
      const body = {generation: gen, request_id: requestId, outcome: outcome};
      if (item.contentHash !== '') body.content_hash = item.contentHash; // 見た要求の値を、そのまま写す (無ければ、欄を付けない)
      const status = await post('/permission', body);
      d.busy = false;
      if (status === 200) {
        d.done = true; // 決着の表示は、permission.resolved を受けてから (ここでは出さない)
        d.note.textContent = '送信した。決着 (permission.resolved) を待っている';
      } else if (status === 409 && lastCode === 'content_changed') { // 決着済みとは別: 要求は、まだ未決
        d.note.textContent = '承認した内容と、いまの要求が違う (応答は受け付けられなかった)。画面を読み込み直して、もう一度、内容を見て決める';
      } else if (status === 400 && lastCode === 'content_hash_required') {
        d.note.textContent = '内容の照合に要る値 (content_hash) が、この要求に無い (応答は受け付けられなかった)';
      } else if (status === 404 || status === 409) {
        d.done = true;
        d.note.textContent = status === 409 ? 'すでに決着済み、または別の起動の画面 (応答は受け付けられなかった)' : 'この要求は、もう無い';
      } else {
        d.note.textContent = status === 0 ? '送れなかった (通信の失敗)。もう一度押せる' : describe(status, '応答') + '。もう一度押せる';
      }
      refreshDialog(item, d);
    }

    function buildDialog(item) {
      const d = {el: el('div', 'dialog'), gen: state.generation, busy: false, done: false, approve: null, deny: null, note: null, hint: null, pre: null, cells: [], view: null, tick: null, removed: false, item: item};
      if (item.origin) d.el.className += ' dialog-sub';
      d.el.appendChild(el('div', 'label', item.summary ? '権限の要求: ' + item.summary : '権限の要求'));
      d.el.appendChild(sourceRow(item.origin)); // tool: の行の上
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
      if (!core.shownFully(item)) {
        let why = 'input が長すぎる・深すぎる・形が不正で、全部は表示できないので、許可できない (拒否だけ)';
        if (item.idPlain !== true) why = 'request_id に見えない文字・長さがあり、許可できない (拒否だけ)';
        else if (item.details.has) why = '詳細が長すぎる・多すぎる・形が不正で、全部は表示できないので、許可できない (拒否だけ)';
        d.el.appendChild(el('div', 'warn', why));
      }
      d.hint = el('div', 'warn', '');
      d.el.appendChild(d.hint);
      d.el.appendChild(el('div', 'meta', 'request_id: ' + item.idShown));
      // 関門の枠 (d.pre) は、詳細があれば詳細、なければ input。詳細があるときの生の input は、畳んだ参考 (関門に使わない)。
      d.pre = el('pre', 'input', item.details.has ? item.details.text + (item.detailsTruncated ? '\n… (詳細が長すぎて、全部は表示していない)' : '') : item.input.text + clipNote(item.input));
      d.pre.addEventListener('scroll', () => { // スクロールのたびに、見た範囲・時間を足す (一発のジャンプ・速いスクロールでは、足りない)
        refreshDialog(item, d);
      });
      d.el.appendChild(d.pre);
      if (item.details.has) {
        const raw = el('details', 'detail');
        raw.appendChild(el('summary', null, '生の input (参考。許可の判断は、上の詳細で)'));
        raw.appendChild(el('pre', 'input', item.input.text + clipNote(item.input)));
        d.el.appendChild(raw);
      }
      return d;
    }

    // ---- form (質問) ダイアログ ----

    // buildFormDialog は、未決の form から、ダイアログを作る。入力部品は、form.requested の項目 (エージェントが出した要求) からだけ作る。
    // 値は、項目が持つ生の key・選択肢の value に結ぶ (DOM の属性・表示用の文字列から、キーを読み戻さない)。
    function buildFormDialog(item) {
      const d = {el: el('div', 'dialog dialog-form'), gen: state.generation, busy: false, done: false, submit: null, cancel: null, note: null, controls: [], removed: false, item: item};
      if (item.origin) d.el.className += ' dialog-sub';
      d.el.appendChild(el('div', 'label', '質問 (エージェントから)' + (item.title ? ': ' + item.title : '')));
      d.el.appendChild(sourceRow(item.origin));
      d.el.appendChild(el('div', 'warn', 'この回答は、エージェントに渡ります。秘密・パスワードは、入力しない。質問の文は、エージェントが書いたもの (検証されていない)'));
      d.cancel = el('button', 'deny', '回答しない (取り消す)');
      d.submit = el('button', 'approve', '回答を送る');
      d.cancel.addEventListener('click', () => { answerForm(item, d, 'cancelled'); });
      d.submit.addEventListener('click', () => { answerForm(item, d, 'answered'); });
      const row = el('div', 'dialog-buttons');
      row.appendChild(d.cancel);
      row.appendChild(d.submit);
      d.el.appendChild(row);
      d.note = el('div', 'meta', '');
      d.el.appendChild(d.note);
      if (!item.answerable) d.el.appendChild(el('div', 'warn', 'この質問は、数・形・長さ・request_id が想定外で、答えられない (取り消しだけ)'));
      d.el.appendChild(el('div', 'meta', 'request_id: ' + item.idShown));
      if (!item.answerable) return d;
      const changed = () => { refreshFormDialog(item, d); };
      const prefix = 'f' + (formSeq++) + '_';
      const choice = (type, name, value, label, description) => { // 選択肢 1 つ (ラジオかチェックボックス)。value は、送り返す生の値
        const box = el('label', 'form-option');
        const inp = doc.createElement('input');
        inp.type = type;
        inp.name = name;
        inp.addEventListener('change', changed);
        box.appendChild(inp);
        box.appendChild(el('span', null, label + (description ? ' — ' + description : '')));
        return {box: box, input: inp, value: value};
      };
      const textInput = (extra) => {
        const inp = doc.createElement('input');
        inp.type = 'text';
        inp.autocomplete = 'off';
        inp.maxLength = core.LIMITS.maxAnswer;
        inp.addEventListener('input', changed);
        inp.addEventListener('change', changed);
        if (extra) inp.placeholder = extra;
        return inp;
      };
      item.fields.forEach((f, idx) => {
        const box = el('div', 'form-field');
        box.appendChild(el('div', 'form-q', (f.title || f.keyShown) + (f.required ? ' (必須)' : '')));
        if (f.description) box.appendChild(el('div', 'meta', f.description));
        const c = {field: f, options: [], other: null, otherText: null, text: null};
        if (f.type === 'text') {
          c.text = textInput('');
          box.appendChild(c.text);
        } else {
          const kind = f.type === 'select' ? 'radio' : 'checkbox';
          for (const o of f.options) {
            const ch = choice(kind, prefix + idx, o.value, o.label, o.description);
            c.options.push(ch);
            box.appendChild(ch.box);
          }
          if (f.custom) { // options に無い文字列 (その他)
            c.other = choice(kind, prefix + idx, '', 'その他', '');
            c.otherText = textInput('その他の内容');
            c.otherText.addEventListener('input', () => { if (c.otherText.value !== '') c.other.input.checked = true; changed(); }); // 書き始めたら、その他を選ぶ
            c.other.box.appendChild(c.otherText);
            box.appendChild(c.other.box);
          }
        }
        d.controls.push(c);
        d.el.appendChild(box);
      });
      return d;
    }

    // formAnswerOf は、入力欄から、回答 (キー → 文字列か文字列の配列) の組と、必須が満たされているかを作る。キーは、項目の生の key。空の回答は、含めない。
    function formAnswerOf(d) {
      const pairs = [];
      let ok = true;
      for (const c of d.controls) {
        const f = c.field;
        let value = null;
        if (f.type === 'text') {
          if (c.text.value.trim() !== '') value = c.text.value;
        } else if (f.type === 'select') {
          const sel = c.options.find((o) => o.input.checked === true);
          if (sel) value = sel.value;
          else if (c.other && c.other.input.checked === true && c.otherText.value.trim() !== '') value = c.otherText.value;
        } else {
          const vals = c.options.filter((o) => o.input.checked === true).map((o) => o.value);
          if (c.other && c.other.input.checked === true && c.otherText.value.trim() !== '' && !vals.includes(c.otherText.value)) vals.push(c.otherText.value);
          if (vals.length > 0) value = vals;
        }
        if (value === null) {
          if (f.required) ok = false;
        } else {
          pairs.push([f.key, value]);
        }
      }
      return {pairs: pairs, ok: ok};
    }

    function refreshFormDialog(item, d) {
      const can = dialogCanAnswer(d, item);
      d.cancel.disabled = !can;
      d.submit.disabled = !can || !item.answerable || !formAnswerOf(d).ok;
    }

    async function answerForm(item, d, outcome) {
      if (!dialogCanAnswer(d, item)) return;
      const body = {generation: d.gen, request_id: item.requestId, outcome: outcome};
      if (outcome === 'answered') {
        if (!item.answerable) return;
        const a = formAnswerOf(d);
        if (!a.ok) return;
        body.answer = Object.fromEntries(a.pairs); // 生の key を、own property にする (__proto__ という key でも、プロトタイプを変えない)
      }
      if (item.contentHash !== '') body.content_hash = item.contentHash;
      d.busy = true;
      refreshFormDialog(item, d);
      d.note.textContent = '送信中';
      const status = await post('/form', body);
      d.busy = false;
      if (status === 200) {
        d.done = true; // 決着の表示は、form.resolved を受けてから
        d.note.textContent = '送信した。決着 (form.resolved) を待っている';
      } else if (status === 409 && lastCode === 'content_changed') {
        d.note.textContent = '見た内容と、いまの質問が違う (応答は受け付けられなかった)。画面を読み込み直して、もう一度、内容を見て決める';
      } else if (status === 404 || status === 409) {
        d.done = true;
        d.note.textContent = status === 409 ? 'すでに決着済み、または別の起動の画面 (応答は受け付けられなかった)' : 'この質問は、もう無い';
      } else if (status === 400 && lastCode === 'bad_answer') {
        d.note.textContent = '回答の形が、サーバーに拒否された (必須の欠け・選択肢にない値・長さ)。直して、もう一度送れる';
      } else if (status === 400 && lastCode === 'content_hash_required') {
        d.note.textContent = '内容の照合に要る値 (content_hash) が、この質問に無い (応答は受け付けられなかった)';
      } else {
        d.note.textContent = status === 0 ? '送れなかった (通信の失敗)。もう一度押せる' : describe(status, '回答') + '。もう一度押せる';
      }
      refreshFormDialog(item, d);
    }

    // syncDialogs は、未決の権限要求 (項目) だけから、ダイアログを作り・外し・有効/無効を更新する。
    function syncDialogs() {
      for (const [item, d] of Array.from(dialogs.entries())) {
        if (item.state !== 'pending' || state.pending.get(item.requestId) !== item) { // 決着した・履歴から外れた・世代が変わった
          d.removed = true;
          dialogsEl.removeChild(d.el);
          dialogs.delete(item);
        }
      }
      if (connected && state.generation !== null) {
        for (const item of state.pending.values()) {
          if ((item.kind === 'permission' || item.kind === 'form') && item.state === 'pending' && !dialogs.has(item)) {
            const d = item.kind === 'form' ? buildFormDialog(item) : buildDialog(item);
            dialogs.set(item, d);
            dialogsEl.appendChild(d.el);
          }
        }
      }
      let forms = 0;
      for (const [item, d] of dialogs) {
        if (item.kind === 'form') {
          forms++;
          refreshFormDialog(item, d);
        } else {
          refreshDialog(item, d);
        }
      }
      // 未決が複数あるとき、枠の中をスクロールして、全部に答える (枠の高さは限ってある)。件数を、枠の外に出す。
      dialogsHead.textContent = dialogs.size === 0 ? '' : (forms === 0 ? '未決の権限要求: ' : '未決の要求 (権限・質問): ') + dialogs.size + ' 件' + (dialogs.size > 1 ? ' (下の枠の中をスクロールして、すべてに答える)' : '');
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

    // eventsURL は、EventSource の URL を作る唯一の場所。固定の url に、検査済みの 2 値 (applyHello が GENERATION_RE で検査した世代・
    // applyEvent が検査した lastSeq) だけを付ける。まだ何も受けていない (初回・世代が変わった直後) ときは、url のまま (全再送)。
    function eventsURL() {
      if (state.generation !== null && core.isGeneration(state.generation) && Number.isSafeInteger(state.lastSeq) && state.lastSeq >= 0) {
        return url + '?after=' + state.lastSeq + '&generation=' + state.generation;
      }
      return url;
    }

    function connect() {
      if (stopped) return;
      closeSource();
      setStatus('接続中');
      const es = new env.EventSource(eventsURL());
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
        document: root.document, EventSource: root.EventSource, fetch: root.fetch.bind(root), setTimeout: root.setTimeout.bind(root), now: root.performance.now.bind(root.performance),
        TextEncoder: root.TextEncoder, scroller: root.document.scrollingElement,
      });
      ui.start('/s/' + m[1] + '/events', '/s/' + m[1]);
    }
  }
})(typeof self !== 'undefined' ? self : this);
