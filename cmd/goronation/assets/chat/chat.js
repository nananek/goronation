'use strict';
// chat.js: 構造化チャットの表示 (読む側)。EventSource で /s/{id}/events を受け、chat-core.js の状態から、DOM を作る。
// 描画は textContent と createElement だけ (エージェントの出力は敵対入力。innerHTML などの、文字列を HTML として解釈する API は使わない。
// cmd/goronation/chat_ui_test.go が、この禁止を検査する)。送信欄・権限の承認は、次の版 (PR⑦b)。
(function (root) {
  const core = typeof require === 'function' && typeof module !== 'undefined' ? require('./chat-core.js') : root.GoroChatCore;

  const RENDER_DELAY_MS = 50;
  const RETRY_FIRST_MS = 1000;
  const RETRY_MAX_MS = 30000;
  const GIVE_UP_AFTER = 10; // hello に届かない失敗が、これだけ続いたら、自動の再接続をやめる (ボタンで再開)

  // create は、UI を組み立てる。env は、document・EventSource・setTimeout・clearTimeout (試験で差し替える)。
  function create(env) {
    const doc = env.document;
    const state = core.createState();
    const log = doc.getElementById('log');
    const statusEl = doc.getElementById('status');
    const notices = doc.getElementById('notices');
    const els = new Map(); // 項目の id → 要素
    let source = null;
    let failures = 0;
    let retryMs = RETRY_FIRST_MS;
    let retryTimer = null;
    let renderTimer = null;
    let stopped = false;
    let url = '';

    function el(tag, cls, text) {
      const e = doc.createElement(tag);
      if (cls) e.className = cls;
      if (text !== undefined && text !== null) e.textContent = text;
      return e;
    }

    function setStatus(msg) {
      statusEl.textContent = msg;
    }

    function clipNote(c) {
      return c.cut ? '\n… (全体 ' + c.total + ' 文字のうち、先頭だけ表示)' : '';
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
          e.className = 'item item-tool status-' + (/^[a-z_]{1,20}$/.test(it.status) ? it.status : 'other');
          const head = el('div', 'label', 'tool: ' + (it.name || '?') + (it.toolKind ? ' (' + it.toolKind + ')' : '') + '  [' + (it.status || '?') + ']');
          e.appendChild(head);
          const det = el('details', 'detail');
          det.appendChild(el('summary', null, '入力・出力'));
          det.appendChild(el('pre', 'input', it.input.text + clipNote(it.input)));
          if (it.output) det.appendChild(el('pre', 'output', it.output.text + clipNote(it.output)));
          if (it.error) det.appendChild(el('pre', 'error-text', it.error.text + clipNote(it.error)));
          e.appendChild(det);
          break;
        }
        case 'permission': {
          e.className = 'item item-permission state-' + (/^[a-z_]{1,20}$/.test(it.state) ? it.state : 'other');
          e.appendChild(el('div', 'label', '権限の要求: ' + (it.toolName || '?') + (it.title ? ' — ' + it.title : '')));
          e.appendChild(el('pre', 'input', it.input.text + clipNote(it.input)));
          const st = it.state === 'pending' ? '未決 (この版は、表示だけ。承認・拒否は次の版)' : '決着: ' + it.state + (it.by ? ' (' + it.by + ')' : '');
          e.appendChild(el('div', 'meta', st));
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
      notices.textContent = '';
      if (state.omitted || state.trimmed) notices.appendChild(el('div', 'notice', '古い分は省略している'));
      if (state.ended) notices.appendChild(el('div', 'notice notice-end', '終了' + (state.ended.exit !== null ? ' (exit ' + state.ended.exit + ')' : '')));
    }

    function render() {
      renderTimer = null;
      const stick = nearBottom();
      const d = core.drain(state);
      if (d.reset) {
        log.textContent = '';
        els.clear();
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
      renderNotices();
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
    }

    function scheduleRetry() {
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
        if (!core.applyHello(state, data).ok) { // 世代の無い・壊れた hello: この接続は使わない
          closeSource();
          failures++;
          scheduleRetry();
          return;
        }
        failures = 0;
        retryMs = RETRY_FIRST_MS;
        setStatus('接続済み');
        scheduleRender();
      });
      es.addEventListener('message', (m) => {
        if (source !== es) return;
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

    function start(chatURL) {
      url = chatURL;
      connect();
    }

    return {start: start, state: state, render: render, connect: connect, elements: els};
  }

  const api = {create: create, RENDER_DELAY_MS: RENDER_DELAY_MS, RETRY_FIRST_MS: RETRY_FIRST_MS, RETRY_MAX_MS: RETRY_MAX_MS, GIVE_UP_AFTER: GIVE_UP_AFTER};
  if (typeof module !== 'undefined' && module.exports) {
    module.exports = api;
  } else {
    root.GoroChat = api;
    const m = /^\/s\/([0-9]{8}-[0-9]{6}-[0-9a-f]{6})\/chat$/.exec(root.location.pathname);
    if (m && root.document.getElementById('log')) {
      const ui = create({
        document: root.document, EventSource: root.EventSource, setTimeout: root.setTimeout.bind(root),
        scroller: root.document.scrollingElement,
      });
      ui.start('/s/' + m[1] + '/events');
    }
  }
})(typeof self !== 'undefined' ? self : this);
