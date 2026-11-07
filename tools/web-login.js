#!/usr/bin/env node
// 网页版（loomy.xunfei.cn）登录 + 绑定邀请码
//   node web-login.js send  <phone>
//   node web-login.js login <phone> <code>
//   node web-login.js bind  <code>          # 读取 ./web-cookie.txt
const fs = require('fs');
const BASE = 'https://loomy.xunfei.cn';
const ORIGIN = { Origin: BASE, Referer: BASE + '/invite' };

async function post(path, body, extraHeaders = {}) {
  const res = await fetch(BASE + path, {
    method: 'POST',
    headers: { 'content-type': 'application/json', ...extraHeaders },
    body: JSON.stringify(body),
    signal: AbortSignal.timeout(25000)
  });
  const text = await res.text();
  return { status: res.status, text, setCookie: res.headers.getSetCookie ? res.headers.getSetCookie() : [] };
}

(async () => {
  const [cmd, a, b] = process.argv.slice(2);
  try {
    if (cmd === 'send') {
      const r = await post('/api/auth/sms-code', { phone: a });
      console.log(r.status, r.text.slice(0, 300));
    } else if (cmd === 'login') {
      const r = await post('/api/auth/login', { phone: a, code: b });
      console.log(r.status, r.text.slice(0, 200));
      const cookie = r.setCookie.map((c) => c.split(';')[0]).join('; ');
      if (cookie) {
        fs.writeFileSync('web-cookie.txt', cookie);
        console.log('cookie saved:', cookie.slice(0, 40) + '...');
      }
    } else if (cmd === 'bind') {
      const cookie = fs.readFileSync('web-cookie.txt', 'utf8').trim();
      const r = await post('/api/invitation/bind', { code: a }, { ...ORIGIN, Cookie: cookie });
      console.log(r.status, r.text.slice(0, 300));
    } else {
      console.log('usage: send <phone> | login <phone> <code> | bind <code>');
    }
  } catch (e) {
    console.error('ERR', e.message);
    process.exit(1);
  }
})();
