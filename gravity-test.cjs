// Hearth T2 directory-gravity e2e: synthetic activity drives the gravity
// ranking. Creates two published worlds, pours chat/edit/join activity into
// world A only, forces a recompute (POST /api/worlds/recompute), and asserts:
//   - the active world ranks ABOVE the quiet world (gravity desc),
//   - a second recompute yields the SAME ordering (determinism — no map-order
//     flapping),
//   - live headcount reflects presence (a WS visitor counts immediately).
//
// Run against any live server: HEARTH_URL=http://host:8090 node gravity-test.cjs
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
  const cookie = sc.split(';')[0];
  return { ...body, cookie };
};

const rest = async (cookie, path, init = {}) => {
  const headers = { Accept: 'application/json', ...(init.headers || {}) };
  if (cookie) headers.Cookie = cookie;
  if (init.body) headers['Content-Type'] = 'application/json';
  const r = await fetch(base + path, { ...init, headers });
  return { code: r.status, body: await r.json() };
};

const wsJoin = (deviceKey, name, space) =>
  new Promise((resolve, reject) => {
    const ws = new WebSocket(`${wsBase}/ws?deviceKey=${deviceKey}`);
    ws.on('open', () => ws.send(env('join', { name, lang: 'en', space, guest: true })));
    ws.on('message', (m) => {
      const j = JSON.parse(m);
      if (j.t === 'welcome') resolve(ws);
      if (j.t === 'error') reject(new Error('join error: ' + JSON.stringify(j.d)));
    });
    ws.on('error', reject);
    setTimeout(() => reject(new Error('join timeout')), 6000);
  });

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

(async () => {
  const tag = Date.now();
  const me = await guest('t2-grav-' + tag, 'GravUser-' + tag);
  check('guest auth', !!me.sessionId && !!me.cookie, JSON.stringify(me));

  // two blank-canvas worlds, both published (owner = this session)
  const mk = async (name) => {
    const c = await rest(me.cookie, '/api/worlds', {
      method: 'POST',
      body: JSON.stringify({ name: name + '-' + tag, width: 24, height: 24 }),
    });
    if (c.code !== 201) throw new Error('create ' + name + ': ' + c.code + ' ' + JSON.stringify(c.body));
    const id = c.body.id;
    const p = await rest(me.cookie, '/api/worlds/' + id + '/publish', { method: 'POST' });
    if (p.code !== 200) throw new Error('publish ' + name + ': ' + p.code + ' ' + JSON.stringify(p.body));
    return { id, name: c.body.name };
  };
  const active = await mk('GravityActive');
  const quiet = await mk('GravityQuiet');
  check('two worlds created + published', !!active.id && !!quiet.id, active.id + ' / ' + quiet.id);

  // pour activity into the ACTIVE world only: joins + chat + edits. The WS
  // joins use the SAME deviceKey as the REST session so the session maps to
  // the world owner (user worlds are owner-edit only; showcase worlds are
  // open) — the edit ops must actually apply, not hit the owner gate.
  const a1 = await wsJoin('t2-grav-' + tag, 'GravA-' + tag, active.id); // resolves on welcome
  for (let i = 0; i < 3; i++) {
    a1.send(env('chat', { channel: 'space', text: 'hello gravity ' + i, nonce: 'n' + i }));
    a1.send(env('edit', { op: 'paint', x: 2 + i, y: 2, tileId: 3 }));
  }
  await sleep(400); // let the emits land

  // a second connection in the ACTIVE world (headcount ≥ 2; same owner user)
  const a2 = await wsJoin('t2-grav-' + tag, 'GravB-' + tag, active.id); // resolves on welcome
  await sleep(300);

  // force recompute, then read the directory twice
  const rc = await rest(me.cookie, '/api/worlds/recompute', { method: 'POST' });
  check('recompute ok', rc.code === 200 && rc.body.ok === true, rc.code + ' ' + JSON.stringify(rc.body));

  const dir = async () => {
    const d = await rest(me.cookie, '/api/worlds');
    return (d.body.worlds || []).filter((w) => w.id === active.id || w.id === quiet.id);
  };
  const first = await dir();
  const second = await dir();
  check('active world in directory', first.some((w) => w.id === active.id), JSON.stringify(first.map((w) => [w.id, w.gravity?.gravity])));
  check('quiet world in directory', first.some((w) => w.id === quiet.id));

  const pos = (list, id) => list.findIndex((w) => w.id === id);
  const aPos1 = pos(first, active.id), qPos1 = pos(first, quiet.id);
  check('active world ranks ABOVE quiet world', aPos1 !== -1 && qPos1 !== -1 && aPos1 < qPos1,
    `active@${aPos1} quiet@${qPos1} gravities: ` + JSON.stringify(first.map((w) => w.gravity?.gravity)));

  // determinism: two runs → identical relative order
  const aPos2 = pos(second, active.id), qPos2 = pos(second, quiet.id);
  check('ranking deterministic across runs', aPos1 === aPos2 && qPos1 === qPos2,
    `run1: ${aPos1}/${qPos1} run2: ${aPos2}/${qPos2}`);

  // gravity of the active world must be strictly greater than the quiet one
  const gActive = first.find((w) => w.id === active.id)?.gravity?.gravity || 0;
  const gQuiet = first.find((w) => w.id === quiet.id)?.gravity?.gravity || 0;
  check('active gravity > quiet gravity', gActive > gQuiet, `${gActive} vs ${gQuiet}`);

  // live headcount reflects presence (A1 + A2 connected in the active world)
  const doc = await rest(me.cookie, '/api/worlds/' + active.id);
  const hc = doc.body.world?.headcount;
  check('headcount reflects live presence', typeof hc === 'number' && hc >= 2, 'headcount=' + hc);

  // thumbnail endpoint serves a PNG for the active world
  const thumb = await fetch(base + '/api/worlds/' + active.id + '/thumbnail');
  const buf = Buffer.from(await thumb.arrayBuffer());
  check('thumbnail is a PNG', thumb.status === 200 && thumb.headers.get('content-type') === 'image/png' && buf.length > 8,
    thumb.status + ' ' + thumb.headers.get('content-type') + ' ' + buf.length + 'B');

  a1.close();
  a2.close();
  console.log(`\nRESULT: ${pass} pass, ${fail} fail`);
  process.exit(fail === 0 ? 0 : 1);
})().catch((e) => {
  console.error('FATAL', e.message);
  console.log(`\nRESULT: ${pass} pass, ${fail} fail`);
  process.exit(1);
});
