// Hearth T1 world-ownership e2e: template creation (no random walls), the
// edit ACL (owner edits, guest denied), and the functional-object round-trip
// (place -> persist -> reload -> identical, then remove -> gone). Additive
// envelopes only ({op:'object'} + canEdit in welcome/spaces doc); PROTOCOL.md
// frozen contract untouched.
//
// Run against any live server: HEARTH_URL=http://host:8090 node world-ownership-test.cjs
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
  let body = null;
  try { body = await r.json(); } catch { /* non-JSON */ }
  return { code: r.status, body };
};

// Join a space over WS; resolves with { ws, canEdit, world } from the welcome.
const wsJoin = (deviceKey, name, space) =>
  new Promise((resolve, reject) => {
    const ws = new WebSocket(`${wsBase}/ws?deviceKey=${deviceKey}`);
    ws.on('open', () => ws.send(env('join', { name, lang: 'en', space, guest: true })));
    ws.on('message', (m) => {
      const j = JSON.parse(m);
      if (j.t === 'welcome') resolve({ ws, canEdit: j.d?.canEdit, world: j.d?.world });
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
      if (j.t === 'error') { clearTimeout(timer); ws.off('message', onMsg); reject(new Error('server error: ' + JSON.stringify(j.d))); return; }
      if (predicate(j)) { clearTimeout(timer); ws.off('message', onMsg); resolve(j); }
    };
    ws.on('message', onMsg);
  });

// Resolves on a matching server-error envelope (for negative ACL checks).
const waitForErr = (ws, code, what, ms = 8000) =>
  new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      ws.off('message', onMsg);
      reject(new Error('timeout waiting for ' + what));
    }, ms);
    const onMsg = (m) => {
      const j = JSON.parse(m);
      if (j.t === 'error' && j.d?.code === code) { clearTimeout(timer); ws.off('message', onMsg); resolve(j); }
    };
    ws.on('message', onMsg);
  });

(async () => {
  const tag = Date.now();
  const owner = await guest('wo-owner-' + tag, 'WOOwner-' + tag);
  const stranger = await guest('wo-stranger-' + tag, 'WOStranger-' + tag);
  check('owner guest auth', !!owner.sessionId && !!owner.cookie);
  check('stranger guest auth', !!stranger.sessionId && !!stranger.cookie);

  // ---------------------------------------------------------- 1. templates
  // empty_lot must be a blank, wall-free canvas (previously random walls).
  const created = await rest(owner.cookie, '/api/worlds', {
    method: 'POST',
    body: JSON.stringify({ name: 'WO-' + tag, template: 'empty_lot' }),
  });
  check('template create (empty_lot) -> 201', created.code === 201 && !!created.body.id,
    created.code + ' ' + JSON.stringify(created.body));
  const worldId = created.body.id;

  const doc0 = await rest(owner.cookie, '/api/spaces/' + worldId);
  const walls0 = (doc0.body.tiles || []).filter((t) => t.tileId === 1).length;
  check('empty_lot has 0 walls (no random walls)', walls0 === 0, 'walls=' + walls0);

  // ---------------------------------------------------------- 2. ownership
  // The spaces doc + welcome envelope both carry the caller's edit permission.
  check('owner canEdit=true in spaces doc', doc0.body.canEdit === true, JSON.stringify(doc0.body.canEdit));
  const docStr = await rest(stranger.cookie, '/api/spaces/' + worldId);
  check('stranger canEdit=false in spaces doc', docStr.body.canEdit === false, JSON.stringify(docStr.body.canEdit));

  let ownerWS;
  try { ownerWS = await wsJoin('wo-owner-' + tag, 'WOOwner-' + tag, worldId); } catch (e) { check('owner WS join', false, e.message); }
  check('owner WS join', !!ownerWS);
  check('owner welcome canEdit=true', ownerWS && ownerWS.canEdit === true, ownerWS && JSON.stringify(ownerWS.canEdit));

  // owner paints -> applied ack (broadcast echo carries by + applied)
  const paintAckP = waitFor(ownerWS.ws, (j) => j.t === 'edit' && j.d?.op === 'paint' && j.d?.x === 5 && j.d?.y === 5 && j.d?.applied, 'owner paint ack');
  ownerWS.ws.send(env('edit', { op: 'paint', x: 5, y: 5, tileId: 3 }));
  const paintAck = await paintAckP;
  check('owner paint applied', paintAck.d.applied === true && paintAck.d.tileId === 3, JSON.stringify(paintAck.d));

  // stranger joins the same world -> read-only
  let strangerWS;
  try { strangerWS = await wsJoin('wo-stranger-' + tag, 'WOStranger-' + tag, worldId); } catch (e) { check('stranger WS join', false, e.message); }
  check('stranger WS join', !!strangerWS);
  check('stranger welcome canEdit=false', strangerWS && strangerWS.canEdit === false, strangerWS && JSON.stringify(strangerWS.canEdit));

  // stranger paint -> edit_forbidden error (server-arbitrated ACL)
  const forbidP = waitForErr(strangerWS.ws, 'edit_forbidden', 'guest edit rejection');
  strangerWS.ws.send(env('edit', { op: 'paint', x: 6, y: 6, tileId: 3 }));
  const forbid = await forbidP;
  check('guest paint rejected (edit_forbidden)', forbid.d.code === 'edit_forbidden', JSON.stringify(forbid.d));

  // ---------------------------------------------------------- 3. objects
  // owner places a functional object (door) -> applied ack -> persisted on reload
  const objId = 'obj-test-' + String(tag).slice(-6);
  const obj = { id: objId, kind: 'door', x: 8, y: 8 };
  const objAckP = waitFor(ownerWS.ws, (j) => j.t === 'edit' && j.d?.op === 'object' && j.d?.object?.id === objId, 'object place ack');
  ownerWS.ws.send(env('edit', { op: 'object', object: obj }));
  const objAck = await objAckP;
  check('object place ack applied', objAck.d.applied === true && objAck.d.object?.kind === 'door', JSON.stringify(objAck.d));

  const doc1 = await rest(owner.cookie, '/api/spaces/' + worldId);
  const placed = (doc1.body.objects || []).find((o) => o.id === objId);
  check('object persisted on reload (identical)', !!placed && placed.kind === 'door' && placed.x === 8 && placed.y === 8,
    JSON.stringify(doc1.body.objects));

  // remove by objectId (the client undo path) -> gone after reload
  const delAckP = waitFor(ownerWS.ws, (j) => j.t === 'edit' && j.d?.op === 'object' && j.d?.objectId === objId, 'object remove ack');
  ownerWS.ws.send(env('edit', { op: 'object', objectId: objId }));
  const delAck = await delAckP;
  check('object remove ack applied', delAck.d.applied === true, JSON.stringify(delAck.d));

  const doc2 = await rest(owner.cookie, '/api/spaces/' + worldId);
  const stillThere = (doc2.body.objects || []).some((o) => o.id === objId);
  check('object gone after remove', !stillThere, JSON.stringify(doc2.body.objects));

  ownerWS.ws.close();
  strangerWS.ws.close();
  console.log(`\nRESULT: ${pass} pass, ${fail} fail`);
  process.exit(fail === 0 ? 0 : 1);
})().catch((e) => {
  console.error('FATAL', e.message);
  console.log(`\nRESULT: ${pass} pass, ${fail} fail`);
  process.exit(1);
});
