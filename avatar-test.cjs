// Hearth S6 avatar E2E: layered avatar_spec round-trip over the frozen
// envelope. Run against a live server (box: node avatar-test.cjs).
//  1. join with avatar {spec}  => welcome echoes the exact spec
//  2. state broadcast includes avatar.spec for self
//  3. reload same deviceKey (no avatar field) => welcome echoes the SAME spec
//     (server persistence per member)
//  4. NPC/bot avatars are distinct: ambient bots carry an NPC-only body 'bot'
//     (hidden from the human picker) in the roster/state stream
const { WebSocket } = require('/usr/share/nodejs/ws/index.js');
const { randomUUID } = require('node:crypto');

const env = (t, d) => JSON.stringify({ v: 1, t, id: randomUUID(), ts: Date.now(), d });
const base = 'http://127.0.0.1:8090';
let pass = 0, fail = 0;
const check = (name, ok, extra = '') => { if (ok) { pass++; console.log('PASS', name); } else { fail++; console.log('FAIL', name, extra); } };

const TEST_SPEC = {
  v: 1,
  body: 'slim',
  skin: 'olive',
  hair: 'mohawk',
  outfit: 'vest',
  accessory: 'crown',
};

const specEq = (a, b) =>
  !!a && !!b &&
  a.v === b.v && a.body === b.body && a.skin === b.skin &&
  a.hair === b.hair && a.outfit === b.outfit && a.accessory === b.accessory;

function connect(dk) {
  return new Promise((resolve, reject) => {
    const ws = new WebSocket('ws://127.0.0.1:8090/ws?deviceKey=' + dk);
    const inbox = [];
    const waiters = [];
    ws.on('open', () => resolve({ ws, inbox, waiters }));
    ws.on('error', reject);
    ws.on('message', (m) => {
      const j = JSON.parse(m);
      const i = waiters.findIndex((w) => w.pred(j));
      if (i >= 0) { waiters.splice(i, 1)[0].res(j); }
      else inbox.push(j);
    });
  });
}

const waitFor = (c, pred, ms = 6000, label = 'message') =>
  new Promise((resolve, reject) => {
    const hit = c.inbox.findIndex(pred);
    if (hit >= 0) { resolve(c.inbox.splice(hit, 1)[0]); return; }
    const t = setTimeout(() => reject(new Error('timeout waiting for ' + label)), ms);
    c.waiters.push({ pred, res: (j) => { clearTimeout(t); resolve(j); } });
  });

(async () => {
  // 0. space fetch sanity (server up)
  const sp = await fetch(base + '/api/spaces/town-square').then((r) => (r.ok ? r.json() : null));
  check('server up (town-square fetch)', !!sp, 'got 404');

  const dk = 'avatar-e2e-' + Date.now();

  // 1. join with avatar field => welcome echoes it
  const c1 = await connect(dk);
  c1.ws.send(env('join', { name: 'AvatarE2E', lang: 'en', space: 'town-square', guest: true, avatar: { spec: TEST_SPEC } }));
  const w1 = await waitFor(c1, (j) => j.t === 'welcome', 6000, 'welcome');
  const echoed = w1.d?.avatar?.spec;
  check('welcome echoes avatar_spec', specEq(echoed, TEST_SPEC), JSON.stringify(echoed));

  // 2. state broadcast includes avatar.spec for self
  const st = await waitFor(c1, (j) => j.t === 'state' && Array.isArray(j.d?.entities), 6000, 'state');
  const self = st.d.entities.find((e) => e.id === w1.d.selfId);
  check('state broadcast includes avatar', !!self && specEq(self.avatar?.spec, TEST_SPEC), JSON.stringify(self?.avatar));

  // 3. bots are NPC-distinct (body 'bot' present in state stream)
  const bot = st.d.entities.find((e) => e.bot === true && e.avatar?.spec?.body === 'bot');
  check('bots carry NPC-only body (distinct)', !!bot, 'no bot with body=bot in state');

  c1.ws.close();
  await new Promise((r) => setTimeout(r, 400));

  // 4. reload same deviceKey (no avatar field) => same spec (server persisted)
  const c2 = await connect(dk);
  c2.ws.send(env('join', { name: 'AvatarE2E', lang: 'en', space: 'town-square', guest: true }));
  const w2 = await waitFor(c2, (j) => j.t === 'welcome', 6000, 'welcome2');
  const persisted = w2.d?.avatar?.spec;
  check('reload same deviceKey => same avatar', specEq(persisted, TEST_SPEC), JSON.stringify(persisted));
  c2.ws.close();

  // 5. fresh deviceKey (no avatar) => valid deterministic default, no crash
  const c3 = await connect('avatar-e2e-fresh-' + Date.now());
  c3.ws.send(env('join', { name: 'Fresh', lang: 'en', space: 'town-square', guest: true }));
  const w3 = await waitFor(c3, (j) => j.t === 'welcome', 6000, 'welcome3');
  const def = w3.d?.avatar?.spec;
  check('fresh device gets valid default spec', !!def && def.v === 1 && !!def.body && !!def.skin && !!def.hair && !!def.outfit && !!def.accessory, JSON.stringify(def));
  c3.ws.close();

  setTimeout(() => { console.log(`\nRESULT: ${pass} pass, ${fail} fail`); process.exit(fail ? 1 : 0); }, 300);
})().catch((e) => { console.error('FATAL', e.message); process.exit(1); });
