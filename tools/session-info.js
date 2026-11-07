#!/usr/bin/env node
// 查询 Loomy 账号信息（含会话到期时间）
const crypto = require('crypto');
const CFG = {
  baseUrl: 'https://account.xfinfr.com',
  accessKeyId: process.env.LOOMY_ACCESS_KEY_ID || "",
  accessKeySecret: process.env.LOOMY_ACCESS_KEY_SECRET || "",
  appId: 'GM3LOOMY'
};
if (!CFG.accessKeyId || !CFG.accessKeySecret) {
  throw new Error("Set LOOMY_ACCESS_KEY_ID and LOOMY_ACCESS_KEY_SECRET in the environment.");
}
const md5 = (b) => crypto.createHash('md5').update(b, 'utf8').digest('base64');
const esc = (p) => p.split('/').map((s) => (s ? encodeURIComponent(s).replace(/[!'()*]/g, (c) => '%' + c.charCodeAt(0).toString(16).toUpperCase()) : '')).join('/');

async function call(path, param) {
  const body = JSON.stringify({
    base: { appid: CFG.appId, modelid: 'Web', version: '1.0.0', devid: 'web', ua: 'Loomy|Desktop|Electron|Windows', traceid: crypto.randomUUID().replace(/-/g, '') },
    param
  });
  const date = new Date().toUTCString();
  const nonce = crypto.randomUUID();
  const sts = ['POST', esc(path), '', md5(body), 'application/json', date, nonce, '', ''].join('\n');
  const sig = crypto.createHmac('sha1', CFG.accessKeySecret).update(sts).digest('base64');
  const res = await fetch(CFG.baseUrl + path, {
    method: 'POST',
    headers: {
      Authorization: `account ${CFG.accessKeyId}:${sig}`,
      Date: date, Nonce: nonce, 'Content-Type': 'application/json',
      'Content-MD5': md5(body), Accept: 'application/json'
    },
    body, signal: AbortSignal.timeout(20000)
  });
  return { status: res.status, json: await res.json().catch(() => ({})) };
}

(async () => {
  const session = process.argv[2];
  const r = await call('/userinfo/query/baseInfo', { session });
  console.log('http', r.status);
  console.log(JSON.stringify(r.json, null, 2).slice(0, 1200));
})();
