// Hearth T2 social-layer e2e: two deviceKeys become friends over REST, then a
// WS presence round trip — friend accept event, presence join/offline events,
// and the online flag in the friend list. Additive envelopes only
// ({t:'friend'}, {t:'friend_presence'}); PROTOCOL.md frozen contract untouched.
//
// Run against any live server: HEARTH_URL=http://host:8090 node social-test.cjs
// (baseline: node e2e-test.cjs must be 4/4 first).
let WS;
try {
  ({ WebSocket } = require('ws')); // npm install ws (local dev)
} catch {
  ({ WebSocket } = require('/usr/share/nodejs/ws/index.js')); // Debian package (box)
}
const { randomUUID } = require('node:crypto');

const base = process.env.HEARTH_URL || 'http://127.0.0.1:8090';
const wsBase = base.replace(/^http/, 'ws');
const env = (t, d) => JSON.stringify({ v: 1, t, id: randomUUID(), ts: Date.now(), d });

let pass = 0, fail = 0;
const check = (name, ok, extra = '') => {
  if (ok) { pass++; console.log('PASS', name); }
  else { fail++; console.log('FAIL', name, extra); }
};

const guest = async (deviceKey, name) => {
  const r = await fetch(base + '/api/auth/guest', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ deviceKey, name }),
  });
  const body = await r.json();
  const sc = r.headers.get('set-cookie') || '';
  const cookie = sc.split(';')[0]; // hearth_session=...
  return { ...body, cookie };
};

const rest = async (cookie, path, init = {}) => {
  const headers = { Accept: 'application/json', ...(init.headers || {}) };
  if (cookie) headers.Cookie = cookie;
  if (init.body) headers['Content-Type'] = 'application/json';
  const r = await fetch(base + path, { ...init, headers });
  return { code: r.status, body: await r.json() };
};

const wsJoin = (deviceKey, name) =>
  new Promise((resolve, reject) => {
    const ws = new WebSocket(`${wsBase}/ws?deviceKey=${deviceKey}`);
    ws.on('open', () => ws.send(env('join', { name, lang: 'en', space: 'town-square', guest: true })));
    ws.on('message', (m) => {
      const j = JSON.parse(m);
      if (j.t === 'welcome') resolve(ws);
      if (j.t === 'error') reject(new Error('join error: ' + JSON.stringify(j.d)));
    });
    ws.on('error', reject);
    setTimeout(() => reject(new Error('join timeout')), 6000);
  });

const waitFor = (ws, predicate, what, ms = 8000) =>
  new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      ws.off('message', onMsg);
      reject(new Error('timeout waiting for ' + what));
    }, ms);
    const onMsg = (m) => {
      const j = JSON.parse(m);
      if (j.t === 'error') { clearTimeout(timer); reject(new Error('server error: ' + JSON.stringify(j.d))); return; }
      if (predicate(j)) { clearTimeout(timer); ws.off('message', onMsg); resolve(j); }
    };
    ws.on('message', onMsg);
  });

(async () => {
  const tag = Date.now();
  const a = await guest('t2-a-' + tag, 'T2Alice');
  const b = await guest('t2-b-' + tag, 'T2Bob');
  check('guest auth A', !!a.sessionId && !!a.cookie);
  check('guest auth B', !!b.sessionId && !!b.cookie);

  // find Bob by name
  const search = await rest(a.cookie, '/api/users?q=' + encodeURIComponent('T2Bob'));
  const bob = (search.body.users || []).find((u) => u.name === 'T2Bob');
  check('user search finds T2Bob', !!bob, JSON.stringify(search.body));

  // A -> B request
  const req = await rest(a.cookie, '/api/friends', {
    method: 'POST', body: JSON.stringify({ friendId: bob.id }),
  });
  check('request created', req.code === 201 && req.body.status === 'pending', req.code + ' ' + JSON.stringify(req.body));

  // B sees incoming
  const bList = await rest(b.cookie, '/api/friends');
  const bRow = (bList.body.friends || []).find((f) => f.friendId === a.userId);
  check('B sees incoming request', !!bRow && bRow.status === 'requested', JSON.stringify(bList.body));

  // A connects first (will receive the accept + presence events)
  let aliceWS;
  try { aliceWS = await wsJoin('t2-a-' + tag, 'T2Alice'); } catch (e) { check('A WS join', false, e.message); }
  check('A WS join', !!aliceWS);

  // B accepts -> A gets {t:'friend', event:'accept'}
  const acc = await rest(b.cookie, '/api/friends/' + a.userId + '/accept', { method: 'POST' });
  check('accept ok', acc.code === 200 && acc.body.ok === true, JSON.stringify(acc.body));
  const friendEvt = await waitFor(aliceWS, (j) => j.t === 'friend' && j.d?.event === 'accept' && j.d?.userId === b.userId, 'friend accept event');
  check('friend accept event delivered to A', friendEvt.d.status === 'accepted');

  // B joins town-square -> A gets {t:'friend_presence', event:'join'}
  let bobWS;
  try { bobWS = await wsJoin('t2-b-' + tag, 'T2Bob'); } catch (e) { check('B WS join', false, e.message); }
  check('B WS join', !!bobWS);
  const presEvt = await waitFor(aliceWS, (j) => j.t === 'friend_presence' && j.d?.event === 'join' && j.d?.userId === b.userId, 'presence join event');
  check('presence join event delivered to A', presEvt.d.online === true && !!presEvt.d.spaceId, JSON.stringify(presEvt.d));

  // A's list shows B online in town-square
  const aList = await rest(a.cookie, '/api/friends');
  const aRow = (aList.body.friends || []).find((f) => f.friendId === b.userId);
  check('A list shows B online', !!aRow && aRow.online === true && aRow.space === 'town-square', JSON.stringify(aList.body));

  // B disconnects -> A gets offline event
  bobWS.close();
  const offEvt = await waitFor(aliceWS, (j) => j.t === 'friend_presence' && j.d?.event === 'offline' && j.d?.userId === b.userId, 'offline event');
  check('offline event delivered to A', offEvt.d.online === false, JSON.stringify(offEvt.d));

  aliceWS.close();
  console.log(`\nRESULT: ${pass} pass, ${fail} fail`);
  process.exit(fail === 0 ? 0 : 1);
})().catch((e) => {
  console.error('FATAL', e.message);
  console.log(`\nRESULT: ${pass} pass, ${fail} fail`);
  process.exit(1);
});
