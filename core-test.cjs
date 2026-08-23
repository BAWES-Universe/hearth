// core-test.cjs — S1 Core e2e: world CRUD + publish flow + universal hub
// routing + gravity determinism + audit. Runs against a LIVE hearth server
// (default http://127.0.0.1:8090, override with HEARTH_URL).
//
//   4 checks:
//   1. create world -> publish -> appears in /api/worlds (published, gravity)
//   2. gravity ordering determinism (two reads, same order; activity ranks up)
//   3. portal round-trip: town-square -> hearth -> garden (by world id)
//   4. audit row on publish (append-only activity feed shows publish event)
//
// Uses global WebSocket (node >= 21) with fallback to the Debian ws package.
const { randomUUID } = require('node:crypto');

const BASE = process.env.HEARTH_URL || 'http://127.0.0.1:8090';
const WS_URL = BASE.replace(/^http/, 'ws') + '/ws';
const WebSocketImpl = (() => {
  try { return WebSocket; } catch { return require('/usr/share/nodejs/ws/index.js'); }
})();

let pass = 0, fail = 0;
const check = (name, ok, extra = '') => {
  if (ok) { pass++; console.log('PASS', name); }
  else { fail++; console.log('FAIL', name, extra); }
};
const env = (t, d) => JSON.stringify({ v: 1, t, id: randomUUID(), ts: Date.now(), d });
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function api(method, path, body, cookie) {
  const headers = {};
  if (body) headers['Content-Type'] = 'application/json';
  if (cookie) headers['Cookie'] = cookie;
  const res = await fetch(BASE + path, {
    method, headers, body: body ? JSON.stringify(body) : undefined,
  });
  const text = await res.text();
  let j = null;
  try { j = JSON.parse(text); } catch {}
  return { status: res.status, j, text };
}

// openWS connects, optionally joins, and returns {ws, send, waitFor} —
// waitFor(t) resolves with the first envelope of type t (rejects on error).
function openWS(joinMsg) {
  return new Promise((resolve, reject) => {
    const ws = new WebSocketImpl(WS_URL);
    const pending = new Map();
    ws.on('open', () => {
      if (joinMsg) ws.send(env('join', joinMsg));
      resolve({
        ws,
        send: (t, d) => ws.send(env(t, d)),
        waitFor: (t, timeout = 6000) => new Promise((res, rej) => {
          const to = setTimeout(() => { pending.delete(t); rej(new Error('timeout waiting for ' + t)); }, timeout);
          pending.set(t, { res, rej, to });
        }),
      });
    });
    ws.on('message', (m) => {
      let j;
      try { j = JSON.parse(m.toString()); } catch { return; }
      if (j.t === 'error') {
        for (const [, p] of pending) { clearTimeout(p.to); p.rej(new Error('server error: ' + JSON.stringify(j.d))); }
        pending.clear();
        return;
      }
      const p = pending.get(j.t);
      if (p) { clearTimeout(p.to); pending.delete(j.t); p.res(j.d); }
    });
    ws.on('error', reject);
  });
}

// guestAuth logs in and returns the session cookie (hearth_session=...).
async function guestAuth(deviceKey, name) {
  const r = await fetch(BASE + '/api/auth/guest', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ deviceKey, name }),
  });
  const setCookie = r.headers.get('set-cookie') || '';
  const jar = setCookie.split(';')[0];
  return jar;
}

(async () => {
  const dk = 's1-core-' + Date.now();
  const jar = await guestAuth(dk, 'S1Core');
  check('guest auth sets session cookie', jar.startsWith('hearth_session='), `jar=${jar.slice(0, 30)}`);

  // ============ CHECK 1: create -> publish -> in /api/worlds ============
  const worldName = 'S1 Core World ' + (Date.now() % 100000);
  const created = await api('POST', '/api/worlds', { name: worldName }, jar);
  check('POST /api/worlds creates draft', created.status === 201 && created.j?.ok === true &&
    created.j?.is_published === false, `status=${created.status} body=${created.text?.slice(0, 120)}`);
  const worldId = created.j?.id;
  if (!worldId) { fail++; console.log('FAIL cannot continue without world id'); process.exit(1); }

  const dir0 = await api('GET', '/api/worlds', null, null);
  check('draft hidden from public directory',
    !(dir0.j?.worlds || []).some((w) => w.id === worldId), '');

  const pub = await api('POST', `/api/worlds/${worldId}/publish`, null, jar);
  check('POST publish draft->published', pub.status === 200 && pub.j?.is_published === true,
    `status=${pub.status} body=${pub.text?.slice(0, 120)}`);

  await sleep(80);
  const dir1 = await api('GET', '/api/worlds', null, null);
  const card = (dir1.j?.worlds || []).find((w) => w.id === worldId);
  check('published world in /api/worlds with owner+headcount+gravity',
    !!card && card.is_published === true && card.owner?.name === 'S1Core' &&
    typeof card.headcount === 'number' && card.gravity && typeof card.gravity.gravity === 'number',
    `card=${JSON.stringify(card)}`);

  // ============ CHECK 2: gravity ordering determinism ============
  const orderIds = (d) => (d.j?.worlds || []).map((w) => w.id);
  const first = orderIds(dir1);
  await sleep(60);
  const dir2 = await api('GET', '/api/worlds', null, null);
  const second = orderIds(dir2);
  check('gravity ordering deterministic across reads',
    first.length === second.length && first.every((id, i) => id === second[i]),
    `first=${first} second=${second}`);

  // create a second world WITHOUT activity — the active one must rank above
  const quiet = await api('POST', '/api/worlds', { name: 'Quiet ' + (Date.now() % 100000) }, jar);
  const quietId = quiet.j?.id;
  await api('POST', `/api/worlds/${quietId}/publish`, null, jar);
  // visit the first world so its gravity rises (join = reach + momentum)
  const wsc = await openWS({ name: 'GravTest', guest: true, deviceKey: dk + '-g', space: worldId });
  await wsc.waitFor('welcome');
  wsc.ws.close();
  await sleep(120);
  const dir3 = await api('GET', '/api/worlds', null, null);
  const worlds3 = dir3.j?.worlds || [];
  const idxActive = worlds3.findIndex((w) => w.id === worldId);
  const idxQuiet = worlds3.findIndex((w) => w.id === quietId);
  check('active world ranks above quiet world', idxActive >= 0 && idxQuiet >= 0 && idxActive < idxQuiet,
    `active=${idxActive} quiet=${idxQuiet}`);

  // ============ CHECK 3: portal round-trip (town-square -> hearth -> garden) ====
  const wsp = await openWS({ name: 'PortalT', guest: true, deviceKey: dk + '-p' }); // NO spaceId -> universal spawn
  const wel = await wsp.waitFor('welcome');
  check('universal spawn: no spaceId -> town-square', wel.spaceId === 'town-square',
    `spaceId=${wel.spaceId}`);
  const townPortals = (wel.world?.portals || []).map((p) => p.id);
  const target = townPortals.find((id) => id.startsWith('hearth')) || townPortals[0];
  check('town-square has hearth portal', !!target, `portals=${townPortals}`);

  wsp.send('move', { x: 8, y: 16, dir: 'right' });
  await sleep(60);
  wsp.send('portal', { portalId: target });
  const hop1 = await wsp.waitFor('portal');
  check('portal hop 1 -> hearth by world id', hop1.spaceId === 'hearth', `spaceId=${hop1.spaceId}`);

  // in hearth we land at (4,16) — exactly on the garden-east portal
  wsp.send('portal', { portalId: 'garden-east' });
  const hop2 = await wsp.waitFor('portal');
  check('portal hop 2 -> garden (round-trip via world ids)', hop2.spaceId === 'garden',
    `spaceId=${hop2.spaceId}`);
  wsp.ws.close();

  // ============ CHECK 4: audit row on publish ============
  const act = await api('GET', `/api/worlds/${worldId}/activity?limit=20`, null, null);
  const events = act.j?.events || [];
  const pubRow = events.find((e) => e.kind === 'world' && e.action === 'publish' && e.worldId === worldId);
  check('audit row on publish (append-only Emit)',
    !!pubRow && typeof pubRow.actor === 'string' && pubRow.actor.length > 0 &&
    pubRow.role === 'member' && !!pubRow.diff && !!pubRow.ts && !!pubRow.ip,
    `events=${JSON.stringify(events.slice(0, 3))}`);

  console.log(`\nRESULT: ${pass} pass, ${fail} fail`);
  process.exit(fail === 0 ? 0 : 1);
})().catch((e) => {
  fail++;
  console.log('FAIL uncaught', e.message);
  console.log(`\nRESULT: ${pass} pass, ${fail} fail`);
  process.exit(1);
});
