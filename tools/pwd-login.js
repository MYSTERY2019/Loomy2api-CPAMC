#!/usr/bin/env node
// 密码登录：getPuKey → RSA(PKCS#1 v1.5) → byPwd
const crypto = require('crypto');

const CFG = {
  baseUrl: 'https://account.xfinfr.com',
  accessKeyId: '2thryby66wxi53sk',
  accessKeySecret: 'zsak6eadrbawz683wf5r3m2snrwj868r',
  appId: 'GM3LOOMY'
};

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
  const sig = crypto.createHmac('sha1', CFG.accessKeySecret).update(sts, 'utf8').digest('base64');
  const res = await fetch(CFG.baseUrl + path, {
    method: 'POST',
    headers: {
      Authorization: `account ${CFG.accessKeyId}:${sig}`,
      Date: date, Nonce: nonce, 'Content-Type': 'application/json',
      'Content-MD5': md5(body), Accept: 'application/json'
    },
    body, signal: AbortSignal.timeout(20000)
  });
  const json = await res.json().catch(() => ({}));
  return { status: res.status, json };
}

function encryptPassword(pukeyB64, password) {
  const der = Buffer.from(pukeyB64, 'base64');
  let key;
  try {
    key = crypto.createPublicKey({ key: der, format: 'der', type: 'spki' });
  } catch {
    key = crypto.createPublicKey({ key: der, format: 'der', type: 'pkcs1' });
  }
  return crypto.publicEncrypt({ key, padding: crypto.constants.RSA_PKCS1_PADDING }, Buffer.from(password, 'utf8')).toString('base64');
}

(async () => {
  const [phone, password, expireDays] = process.argv.slice(2);
  const days = Number(expireDays || 14);

  const k = await call('/login/account/getPuKey', {});
  const pukey = k.json?.data?.pukey;
  const rcode = k.json?.data?.rcode;
  console.log('getPuKey:', k.status, 'code=' + k.json?.code, 'pukey_len=' + (pukey ? pukey.length : 0));
  if (!pukey) { console.log(JSON.stringify(k.json).slice(0, 300)); return; }

  const enc = encryptPassword(pukey, password);
  const r = await call('/login/account/byPwd', {
    loginid: phone, password: enc, rcode, type: 1, expire: days * 24 * 3600
  });
  console.log('byPwd:', r.status, 'code=' + r.json?.code, 'desc=' + (r.json?.desc || ''));
  const d = r.json?.data || {};
  console.log('data keys:', Object.keys(d).join(','));
  if (d.session) console.log('SESSION=' + d.session);
  if (d.expire || d.expireAt || d.expireTime) console.log('expire meta:', d.expire || d.expireAt || d.expireTime);
  if (!d.session) console.log(JSON.stringify(r.json).slice(0, 400));
})();
