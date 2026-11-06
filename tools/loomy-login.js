#!/usr/bin/env node
// Loomy 账号体系（account.xfinfr.com）登录工具
// 用法：
//   node loomy-login.js send   <phone>
//   node loomy-login.js verify <phone> <code> <msgid>
//   node loomy-login.js pwd    <phone> <password>
const crypto = require('crypto');

const CFG = {
  baseUrl: 'https://account.xfinfr.com',
  accessKeyId: '2thryby66wxi53sk',
  accessKeySecret: 'zsak6eadrbawz683wf5r3m2snrwj868r',
  appId: 'GM3LOOMY'
};

const contentMD5 = (body) => crypto.createHash('md5').update(body, 'utf8').digest('base64');

const escapedPath = (p) => {
  const clean = p.startsWith('/') ? p : '/' + p;
  return clean.split('/')
    .map((seg) => (seg ? encodeURIComponent(seg).replace(/[!'()*]/g, (c) => '%' + c.charCodeAt(0).toString(16).toUpperCase()) : ''))
    .join('/');
};

function sign({ method, path, body, date, nonce }) {
  const parts = [method.toUpperCase(), escapedPath(path), '', contentMD5(body), 'application/json', date, nonce, '', ''];
  return crypto.createHmac('sha1', CFG.accessKeySecret).update(parts.join('\n'), 'utf8').digest('base64');
}

const makeBase = () => ({
  appid: CFG.appId,
  modelid: 'Web',
  version: '1.0.0',
  devid: 'web',
  ua: 'Loomy|Desktop|Electron|Windows',
  traceid: crypto.randomUUID().replace(/-/g, '')
});

async function request(path, param) {
  const body = JSON.stringify({ base: makeBase(), param });
  const date = new Date().toUTCString();
  const nonce = crypto.randomUUID();
  const res = await fetch(CFG.baseUrl + path, {
    method: 'POST',
    headers: {
      Authorization: `account ${CFG.accessKeyId}:${sign({ method: 'POST', path, body, date, nonce })}`,
      Date: date,
      Nonce: nonce,
      'Content-Type': 'application/json',
      'Content-MD5': contentMD5(body),
      Accept: 'application/json'
    },
    body,
    signal: AbortSignal.timeout(20000)
  });
  const json = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(`HTTP ${res.status}: ${JSON.stringify(json).slice(0, 300)}`);
  if (json.code && json.code !== '000000') throw new Error(`code=${json.code} ${json.desc || ''}`);
  return json.data || {};
}

(async () => {
  const [cmd, a, b, c] = process.argv.slice(2);
  try {
    if (cmd === 'send') {
      const data = await request('/login/phone/sendMsgCode', { ccode: '86', phone: a, expire: 300 });
      console.log(JSON.stringify({ msgid: data.msgid, raw: data }));
    } else if (cmd === 'verify') {
      const data = await request('/login/phone/checkCode', {
        ccode: '86', phone: a, mcode: b, msgid: c, expire: 14 * 24 * 3600
      });
      console.log(JSON.stringify({ session: data.session, userid: data.userid || data.uid }));
    } else if (cmd === 'pwd') {
      const data = await request('/login/phone/checkPwd', {
        ccode: '86', phone: a, pwd: b, expire: 14 * 24 * 3600
      });
      console.log(JSON.stringify({ session: data.session, userid: data.userid || data.uid }));
    } else {
      console.log('usage: send <phone> | verify <phone> <code> <msgid> | pwd <phone> <password>');
    }
  } catch (e) {
    console.error('ERR ' + e.message);
    process.exit(1);
  }
})();
