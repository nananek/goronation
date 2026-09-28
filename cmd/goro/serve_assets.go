//go:build linux

package main

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
  } catch (e) {
    setStatus('ログインに失敗しました: ' + e.message);
  }
}
document.getElementById('register-btn').addEventListener('click', register);
document.getElementById('login-btn').addEventListener('click', login);
`
