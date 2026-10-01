'use strict';
// 表示に出ない文字 (書式制御 Cf・Default_Ignorable) は、全て印にする (異体字選択子 U+FE00-FE0F は、絵文字に要るので、厳しい判定だけ)。
// 手で挙げた範囲に、漏れがある (U+1D173-1D17A・U+13430-1343F・U+1BCA0-1BCA3・U+FFF0-FFF8・U+E0080-E00FF・U+E01F0-E0FFF・U+0600-0605 など)。
// 見えない文字だけが違う 2 つの request_id・コマンド・path が、同じに見える。chat.test.js と同じ node:test で動く。
const test = require('node:test');
const assert = require('node:assert');
const core = require('./chat-core.js');

const cp = (n) => String.fromCodePoint(n);
const G1 = '0123456789abcdef0123456789abcdef';

test('sanitize: 書式制御・既定で無視される文字は、(U+FE00-FE0F を除いて) 全て印になる。request_id にあれば、許可させない', () => {
  const miss = [];
  for (let c = 0; c <= 0x10ffff; c++) {
    if (c >= 0xd800 && c <= 0xdfff) continue;
    if (c >= 0xfe00 && c <= 0xfe0f) continue;
    const ch = cp(c);
    if (!/[\p{Cf}\p{Default_Ignorable_Code_Point}]/u.test(ch)) continue;
    if (core.sanitize('a' + ch + 'b') === 'a' + ch + 'b') miss.push('U+' + c.toString(16).toUpperCase());
  }
  assert.deepStrictEqual(miss.slice(0, 12), [], '印にならない文字がある (' + miss.length + ' 個)');
  const s = core.createState();
  core.applyHello(s, {generation: G1, first_seq: 0});
  core.applyEvent(s, {v: 0, seq: 0, type: 'permission.requested', data: {request_id: 'r' + cp(0x1d173) + '1', tool_name: 'Bash', input: {command: 'ls'}}});
  const p = s.items.find((i) => i.kind === 'permission');
  assert.strictEqual(p.idPlain, false);
  assert.strictEqual(core.shownFully(p), false);
});
