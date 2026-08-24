// Hearth T2 avatars-stream e2e: custom asset upload → round-trip
// (create → persist → reload → identical render), entitlement enforcement
// (deny + allow at avatar_update time), and safe-archive blocks-worn (409).
// Additive envelopes only ({t:'avatar_update'}); PROTOCOL.md frozen contract
// untouched.
//
// Run against any live server: HEARTH_URL=http://host:8090 node avatars-test.cjs
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

// 1x1 transparent PNG (valid magic + IHDR dims) — same fixture as the Go tests.
const tinyPNG = 'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==';

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
  const ct = r.headers.get('content-type') || '';
  const body = ct.includes('json') ? await r.json() : { raw: true };
  return { code: r.status, body };
};

const wsJoin = (deviceKey, name) =>
  new Promise((resolve, reject) => {
    const ws = new WebSocket(`${wsBase}/ws?deviceKey=${deviceKey}`);
    ws.on('open', () => ws.send(env('join', { name, lang: 'en', space: 'town-square', guest: true })));
    ws.on('message', (m) => {
      const j = JSON.parse(m);
      if (j.t === 'welcome') resolve({ ws, welcome: j });
      if (j.t === 'error') reject(new Error('join error: ' + JSON.stringify(j.d)));
    });
    ws.on('error', reject);
    setTimeout(() => reject(new Error('join timeout')), 6000);
  });

const nextMsg = (ws, predicate, what, ms = 8000) =>
  new Promise((resolve, reject) => {
    const to = setTimeout(() => { cleanup(); reject(new Error('timeout waiting for ' + what)); }, ms);
    const onMsg = (m) => {
      const j = JSON.parse(m);
      if (predicate(j)) { cleanup(); resolve(j); }
    };
    const cleanup = () => { clearTimeout(to); ws.off('message', onMsg); };
    ws.on('message', onMsg);
  });

const specEq = (a, b) =>
  !!a && !!b && a.body === b.body && a.skin === b.skin && a.hair === b.hair &&
  a.outfit === b.outfit && a.accessory === b.accessory;

const S = () => ({ body: 'round', skin: 'warm', hair: 'bob', outfit: 'tee', accessory: 'none' });

(async () => {
  const runId = randomUUID().slice(0, 8);
  const aliceKey = 'avatars-alice-' + runId, bobKey = 'avatars-bob-' + runId;
  const alice = await guest(aliceKey, 'AvA' + runId.slice(0, 4));
  const bob = await guest(bobKey, 'AvB' + runId.slice(0, 4));
  check('guest auth both users', alice.userId && bob.userId);

  // ---- 1. upload + list + raw image ----
  const up = await rest(alice.cookie, '/api/avatars/assets', {
    method: 'POST',
    body: JSON.stringify({ layer: 'accessory', name: 'aliceHalo', kind: 'image/png', data: tinyPNG }),
  });
  const asset = up.body && up.body.asset;
  check('upload 201 with asset id', up.code === 201 && !!asset && !!asset.id, JSON.stringify(up.body).slice(0, 120));
  const assetId = asset && asset.id;
  const option = 'asset:' + assetId;
  check('upload row sane', asset && asset.layer === 'accessory' && asset.status === 'active' && asset.width === 1 && asset.height === 1);

  const list1 = await rest(alice.cookie, '/api/avatars/assets');
  check('list shows the asset', list1.code === 200 && Array.isArray(list1.body.assets) && list1.body.assets.length === 1);
  const img = await rest(alice.cookie, '/api/avatars/assets/' + assetId + '/image');
  check('image endpoint serves png bytes', img.code === 200 && img.body.raw === true);

  // ---- 2. round-trip: create → persist → reload → identical render ----
  const specA = { ...S(), accessory: option };
  const { ws: wsA1 } = await wsJoin(aliceKey, 'AvA' + runId.slice(0, 4));
  const echoP = nextMsg(wsA1, (j) => j.t === 'avatar_update' && j.d && j.d.self === true, 'self avatar_update echo');
  wsA1.send(JSON.stringify({ v: 1, t: 'avatar_update', id: randomUUID(), ts: Date.now(), spec: specA }));
  const echo = await echoP;
  check('avatar_update self-echo received', !!echo && !!echo.d, JSON.stringify(echo).slice(0, 160));
  check('round-trip: echoed spec identical to sent (own asset allowed)', echo && echo.d && specEq(echo.d.spec, specA), JSON.stringify(echo && echo.d && echo.d.spec));

  wsA1.close();
  // fresh join with NO avatar: server must reload the persisted enforced spec
  const { ws: wsA2, welcome: wl } = await wsJoin(aliceKey, 'AvA' + runId.slice(0, 4));
  const reloadSpec = wl && wl.d && wl.d.avatar && wl.d.avatar.spec;
  check('reload: persisted spec identical after fresh join', specEq(reloadSpec, specA), JSON.stringify(reloadSpec));
  // roster must agree (same entity, same look)
  const rosterSelf = wl && wl.d && Array.isArray(wl.d.roster) && wl.d.roster.find((e) => e.id === wl.d.selfId);
  check('reload: roster entity renders same spec', rosterSelf && specEq(rosterSelf.avatar && rosterSelf.avatar.spec, specA));

  // ---- 3. entitlement: deny then allow ----
  const { ws: wsB } = await wsJoin(bobKey, 'AvB' + runId.slice(0, 4));

  // deny: bob wears alice's asset with no grant
  const denyP = nextMsg(wsB, (j) => j.t === 'avatar_update' && j.d && j.d.self === true, 'bob deny echo');
  wsB.send(JSON.stringify({ v: 1, t: 'avatar_update', id: randomUUID(), ts: Date.now(), spec: { ...S(), accessory: option } }));
  const denyEcho = await denyP;
  check('deny: bob echo received', !!denyEcho && !!denyEcho.d);
  const deniedAcc = denyEcho && denyEcho.d && denyEcho.d.spec && denyEcho.d.spec.accessory;
  check('deny: foreign asset normalized away', !!deniedAcc && deniedAcc !== option, 'got ' + deniedAcc);

  // allow: alice puts the asset in a user-granted set and grants bob directly
  const set = await rest(alice.cookie, '/api/avatars/sets', {
    method: 'POST',
    body: JSON.stringify({ name: 'Alice VIP', scope: 'user-granted', items: [{ layer: 'accessory', optionId: option }] }),
  });
  const setId = set.body && set.body.set && set.body.set.id;
  check('set created (user-granted, v1)', set.code === 201 && !!setId && set.body.set.version === 1, JSON.stringify(set.body).slice(0, 120));
  const grant = await rest(alice.cookie, '/api/avatars/grants', {
    method: 'POST',
    body: JSON.stringify({ setId, userId: bob.userId, kind: 'direct' }),
  });
  check('direct grant to bob ok', grant.code >= 200 && grant.code < 300 && grant.body.ok === true, JSON.stringify(grant.body).slice(0, 120));

  const allowP = nextMsg(wsB, (j) => j.t === 'avatar_update' && j.d && j.d.self === true, 'bob allow echo');
  wsB.send(JSON.stringify({ v: 1, t: 'avatar_update', id: randomUUID(), ts: Date.now(), spec: { ...S(), accessory: option } }));
  const allowEcho = await allowP;
  const allowedAcc = allowEcho && allowEcho.d && allowEcho.d.spec && allowEcho.d.spec.accessory;
  check('allow: granted asset kept by bob', allowedAcc === option, 'got ' + allowedAcc);

  // ---- 4. safe-archive: blocks while worn ----
  // alice's WS#2 entity AND bob's WS entity are both still wearing the asset
  const delWorn = await rest(alice.cookie, '/api/avatars/assets/' + assetId, { method: 'DELETE' });
  check('safe-archive: worn -> 409', delWorn.code === 409, 'code=' + delWorn.code + ' ' + JSON.stringify(delWorn.body).slice(0, 120));
  check('safe-archive: 409 mentions worn', /worn/i.test(JSON.stringify(delWorn.body)));

  // bob takes it off first
  const bobOffP = nextMsg(wsB, (j) => j.t === 'avatar_update' && j.d && j.d.self === true, 'bob take-off echo');
  wsB.send(JSON.stringify({ v: 1, t: 'avatar_update', id: randomUUID(), ts: Date.now(), spec: S() }));
  await bobOffP;
  // alice takes it off
  const offP = nextMsg(wsA2, (j) => j.t === 'avatar_update' && j.d && j.d.self === true, 'alice take-off echo');
  wsA2.send(JSON.stringify({ v: 1, t: 'avatar_update', id: randomUUID(), ts: Date.now(), spec: S() }));
  await offP;

  const delOk = await rest(alice.cookie, '/api/avatars/assets/' + assetId, { method: 'DELETE' });
  check('safe-archive: unworn -> 200', delOk.code === 200 && delOk.body.ok === true, 'code=' + delOk.code);
  const list2 = await rest(alice.cookie, '/api/avatars/assets');
  check('list empty after archive', list2.code === 200 && Array.isArray(list2.body.assets) && list2.body.assets.length === 0);

  wsA2.close(); wsB.close();

  console.log(`RESULT: ${pass} pass, ${fail} fail`);
  process.exit(fail === 0 ? 0 : 1);
})().catch((e) => {
  console.error('FATAL', e && e.message ? e.message : e);
  console.log(`RESULT: ${pass} pass, ${fail + 1} fail`);
  process.exit(1);
});
