//go:build linux

package main

import "embed"

// xtermVendor は、ビルド済みの xterm.js 6.0.0・@xterm/addon-fit 0.11.0 (どちらも MIT。vendor/xterm/ に
// LICENSE も置いてある) を、goro の単一バイナリに埋め込む (ADR 0005・goro-serve-plan §0-3 の決定)。
// npm・バンドラは使わない。危険な機能 (OSC 52 の書き込み・リンクの自動起動) につながる addon
// (addon-web-links 等) は、意図的に含めていない。
//
//go:embed vendor/xterm/xterm.js vendor/xterm/xterm.css vendor/xterm/addon-fit.js
var xtermVendor embed.FS

// indexHTML・appJS は、goro serve の最小限のフロントエンド (登録・ログインの骨組みだけ。端末ビュー・
// チャット UI は、まだ無い)。ビルド時の依存を増やさないよう、素の HTML・JS を文字列で埋め込む (npm・
// バンドラは使わない)。インライン <script> は使わない (serve.go の Content-Security-Policy が禁じる)。
const indexHTML = `<!doctype html>
<html lang="ja">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>goro serve</title>
</head>
<body>
<h1>goro serve</h1>
<section id="register">
<h2>登録 (最初の 1 回だけ)</h2>
<p><code>goro serve token</code> で発行したトークンを貼る。</p>
<input id="token" type="text" placeholder="ブートストラップトークン" autocomplete="off">
<button id="register-btn">passkey を登録する</button>
</section>
<section id="login">
<h2>ログイン</h2>
<button id="login-btn">passkey でログインする</button>
</section>
<p id="status"></p>
<script src="/static/app.js"></script>
</body>
</html>
`

const appJS = `
function b64ToBuf(b64) {
  b64 = b64.replace(/-/g, '+').replace(/_/g, '/');
  while (b64.length % 4) b64 += '=';
  const bin = atob(b64);
  const buf = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) buf[i] = bin.charCodeAt(i);
  return buf.buffer;
}
function bufToB64(buf) {
  const bytes = new Uint8Array(buf);
  let bin = '';
  for (const b of bytes) bin += String.fromCharCode(b);
  return btoa(bin).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}
function setStatus(msg) {
  document.getElementById('status').textContent = msg;
}
async function postJSON(url, body) {
  const resp = await fetch(url, {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const data = await resp.json().catch(() => ({}));
  if (!resp.ok) {
    throw new Error(data.error || ('要求が失敗した (' + resp.status + ')'));
  }
  return data;
}
async function register() {
  const token = document.getElementById('token').value.trim();
  if (!token) { setStatus('トークンを入力してください'); return; }
  try {
    const begin = await postJSON('/webauthn/register/begin', {token});
    const options = begin.options;
    options.challenge = b64ToBuf(options.challenge);
    options.user.id = b64ToBuf(options.user.id);
    const cred = await navigator.credentials.create({publicKey: options});
    await postJSON('/webauthn/register/finish', {
      state: begin.state,
      credential: {
        id: cred.id,
        response: {
          clientDataJSON: bufToB64(cred.response.clientDataJSON),
          attestationObject: bufToB64(cred.response.attestationObject),
        },
      },
    });
    setStatus('登録できました。ログインしてください。');
  } catch (e) {
    setStatus('登録に失敗しました: ' + e.message);
  }
}
async function login() {
  try {
    const begin = await postJSON('/webauthn/login/begin');
    const options = begin.options;
    options.challenge = b64ToBuf(options.challenge);
    if (options.allowCredentials) {
      for (const c of options.allowCredentials) c.id = b64ToBuf(c.id);
    }
    const assertion = await navigator.credentials.get({publicKey: options});
    await postJSON('/webauthn/login/finish', {
      state: begin.state,
      credential: {
        id: assertion.id,
        response: {
          clientDataJSON: bufToB64(assertion.response.clientDataJSON),
          authenticatorData: bufToB64(assertion.response.authenticatorData),
          signature: bufToB64(assertion.response.signature),
        },
      },
    });
    setStatus('ログインできました。');
    location.href = '/terminal';
  } catch (e) {
    setStatus('ログインに失敗しました: ' + e.message);
  }
}
document.getElementById('register-btn').addEventListener('click', register);
document.getElementById('login-btn').addEventListener('click', login);
`

// terminalHTML は、端末ビューのページ (requireSession で保護される)。xterm.js 本体・addon-fit は
// vendor から (/static/vendor/*)、中継の配線は terminal.js (自前) から読む。インライン <script> は
// 使わない (CSP)。
const terminalHTML = `<!doctype html>
<html lang="ja">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>goro serve — 端末</title>
<link rel="stylesheet" href="/static/vendor/xterm.css">
<style>
  html, body { margin: 0; height: 100%; background: #000; }
  #term { height: 100%; }
</style>
</head>
<body>
<div id="term"></div>
<script src="/static/vendor/xterm.js"></script>
<script src="/static/vendor/addon-fit.js"></script>
<script src="/static/terminal.js"></script>
</body>
</html>
`

// terminalJS は、端末ビューの中継 (自前。xterm.js 本体・addon-fit は vendor)。危険な機能
// (addon-web-links によるリンクの自動起動・OSC 52 のクリップボード書き込み) は、どちらも配線しない
// (Issue #1 の要求)。xterm.js の中身 (エスケープシーケンスの解釈) には手を入れず、入出力の生バイト列を
// そのまま WebSocket と往復させるだけ。
const terminalJS = `
const term = new Terminal({cursorBlink: true, scrollback: 5000});
const fit = new FitAddon.FitAddon();
term.loadAddon(fit);
term.open(document.getElementById('term'));
fit.fit();

const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
const ws = new WebSocket(proto + '//' + location.host + '/ws/terminal');
ws.binaryType = 'arraybuffer';

const enc = new TextEncoder();
term.onData((data) => {
  if (ws.readyState === WebSocket.OPEN) ws.send(enc.encode(data));
});
term.onResize(({cols, rows}) => {
  if (ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({cols, rows}));
});

ws.addEventListener('open', () => {
  ws.send(JSON.stringify({cols: term.cols, rows: term.rows}));
});
ws.addEventListener('message', (ev) => {
  term.write(new Uint8Array(ev.data));
});
ws.addEventListener('close', () => {
  term.write('\r\n\x1b[31m[接続が切れました。ページを再読み込みすると繋がり直します]\x1b[0m\r\n');
});
ws.addEventListener('error', () => {
  term.write('\r\n\x1b[31m[端末ビューに接続できません (--repo/--session なしで起動した可能性があります)]\x1b[0m\r\n');
});

window.addEventListener('resize', () => fit.fit());
`
