// Hearth T2 editor-v2 e2e: freeform stroke round-trip (edit → save → reload →
// identical), custom asset upload → place → broadcast to a second client →
// remove, and animated-tile persistence (server-authoritative anims table).
// Additive envelopes only; PROTOCOL.md frozen contract untouched.
//
// Run against any live server: HEARTH_URL=http://host:8090 node editor-test.cjs
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
  // JSON bodies only — FormData (asset upload) must keep fetch's multipart header
  if (init.body && typeof init.body === 'string') headers['Content-Type'] = 'application/json';
  const r = await fetch(base + path, { ...init, headers });
  const body = await r.json().catch(() => ({}));
  return { code: r.status, body };
};

const wsJoin = (deviceKey, name, space) =>
  new Promise((resolve, reject) => {
    const ws = new WebSocket(`${wsBase}/ws?deviceKey=${deviceKey}`);
    ws.on('open', () => ws.send(env('join', { name, lang: 'en', space, guest: true })));
    ws.on('message', (m) => {
      const j = JSON.parse(m);
      if (j.t === 'welcome') { ws.welcome = j.d; resolve(ws); }
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

const sendEdit = (ws, d) => ws.send(env('edit', d));
const waitEdit = (ws, pred, what) => waitFor(ws, (j) => j.t === 'edit' && pred(j.d), what);

// Minimal valid 1x1 PNG (red) — verified against the PNG magic below before
// upload, so the server's magic-byte mime check is exercised for real.
const PNG_1x1 = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==',
  'base64',
);

(async () => {
  const tag = Date.now();
  const aKey = 't2ed-a-' + tag;
  const a = await guest(aKey, 'T2EdA-' + tag);
  check('guest auth A', !!a.sessionId && !!a.cookie);
  check('png fixture is a png (magic bytes)', PNG_1x1.length > 8 &&
    PNG_1x1[0] === 0x89 && PNG_1x1[1] === 0x50 && PNG_1x1[2] === 0x4e && PNG_1x1[3] === 0x47);

  // fresh world owned by A
  const cr = await rest(a.cookie, '/api/worlds?deviceKey=' + encodeURIComponent(aKey), {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ name: 'T2 Editor ' + tag, width: 24, height: 24 }),
  });
  const worldId = cr.body && cr.body.id;
  check('world created', cr.code === 201 && !!worldId, cr.code + ' ' + JSON.stringify(cr.body));

  let aWS;
  try { aWS = await wsJoin(aKey, 'T2EdA-' + tag, worldId); } catch (e) { check('owner WS join', false, e.message); }
  check('owner WS join', !!aWS);
  check('owner canEdit true', aWS && aWS.welcome.canEdit === true, JSON.stringify(aWS && aWS.welcome).slice(0, 120));

  // ---------------------------------------------------------------- stroke
  // build two different prior tiles the stroke will overwrite
  sendEdit(aWS, { op: 'paint', x: 5, y: 5, tileId: 1 });
  await waitEdit(aWS, (d) => d.x === 5 && d.y === 5 && d.applied, 'paint wall 5,5');
  sendEdit(aWS, { op: 'paint', x: 6, y: 5, tileId: 3 });
  await waitEdit(aWS, (d) => d.x === 6 && d.y === 5 && d.applied, 'paint grass 6,5');

  // freeform stroke: one batch op over 3 cells, op-level tileId fallback
  sendEdit(aWS, {
    op: 'paint', tileId: 2,
    cells: [{ x: 5, y: 5 }, { x: 6, y: 5 }, { x: 7, y: 5 }],
  });
  const strokeAck = await waitEdit(aWS, (d) => Array.isArray(d.cells) && d.applied, 'stroke batch ack');
  const scells = (strokeAck.d && strokeAck.d.cells) || [];
  const prior = {};
  for (const c of scells) prior[c.x] = c.priorTileId;
  check('stroke ack: 3 cells, tileId 2, priors 1/3/0',
    scells.length === 3 &&
    scells.every((c) => c.tileId === 2) &&
    prior[5] === 1 && prior[6] === 3 && prior[7] === 0,
    JSON.stringify(scells));

  const worldDoc = async () => {
    const r = await rest(null, '/api/spaces/' + worldId);
    return r.body || {};
  };
  const tileAt = (doc, x, y) => (doc.tiles || []).find((t) => t.x === x && t.y === y);

  let doc = await worldDoc();
  check('stroke persisted on reload (identical)',
    tileAt(doc, 5, 5)?.tileId === 2 && tileAt(doc, 6, 5)?.tileId === 2 && tileAt(doc, 7, 5)?.tileId === 2,
    JSON.stringify((doc.tiles || []).filter((t) => t.y === 5)));

  // erase stroke: batch erase of one cell → gone after reload
  // (server acks single-cell changes as x/y/priorTileId, not a cells array)
  sendEdit(aWS, { op: 'erase', cells: [{ x: 6, y: 5 }] });
  await waitEdit(aWS, (d) => d.op === 'erase' && d.applied && d.x === 6 && d.y === 5, 'erase stroke ack');
  doc = await worldDoc();
  check('erase stroke persisted', tileAt(doc, 6, 5) === undefined, JSON.stringify((doc.tiles || []).filter((t) => t.y === 5)));

  // freeform undo: single batch inverse op with PER-CELL prior tiles
  sendEdit(aWS, {
    op: 'paint',
    cells: [{ x: 5, y: 5, tileId: prior[5] }, { x: 6, y: 5, tileId: prior[6] }, { x: 7, y: 5, tileId: prior[7] }],
  });
  await waitEdit(aWS, (d) => Array.isArray(d.cells) && d.cells.length === 3 && d.applied, 'undo batch ack');
  doc = await worldDoc();
  check('per-cell undo restored priors',
    tileAt(doc, 5, 5)?.tileId === 1 && tileAt(doc, 6, 5)?.tileId === 3 && tileAt(doc, 7, 5) === undefined,
    JSON.stringify((doc.tiles || []).filter((t) => t.y === 5)));

  // ---------------------------------------------------------------- assets
  // upload (multipart, owner session cookie)
  const fd = new FormData();
  fd.append('file', new Blob([PNG_1x1], { type: 'image/png' }), 'ember.png');
  fd.append('name', 'ember');
  const up = await rest(a.cookie, '/api/worlds/' + worldId + '/assets', { method: 'POST', body: fd });
  const asset = up.body && up.body.asset;
  check('asset upload accepted', up.code === 201 && !!asset && asset.id && asset.mime === 'image/png' && !!asset.url,
    up.code + ' ' + JSON.stringify(up.body));

  const reg = await rest(a.cookie, '/api/worlds/' + worldId + '/assets');
  check('registry lists upload', (reg.body.assets || []).some((x) => x.id === asset.id), JSON.stringify(reg.body));

  // second client joins the same world (guest — receives broadcasts)
  let bWS;
  try { bWS = await wsJoin('t2ed-b-' + tag, 'T2EdB-' + tag, worldId); } catch (e) { check('second client WS join', false, e.message); }
  check('second client WS join', !!bWS);
  check('second client is guest (canEdit false)', bWS && bWS.welcome.canEdit === false, JSON.stringify(bWS && bWS.welcome).slice(0, 120));

  // A places the asset → B must receive the broadcast with name+url
  const bPlaceP = waitEdit(bWS, (d) => d.op === 'asset' && d.asset && d.asset.assetId === asset.id && d.asset.x === 9 && d.asset.y === 5, 'asset place broadcast to B');
  sendEdit(aWS, { op: 'asset', asset: { assetId: asset.id, x: 9, y: 5 } });
  const aPlaceAck = await waitEdit(aWS, (d) => d.op === 'asset' && d.applied && d.asset && d.asset.x === 9, 'asset place ack');
  check('asset place ack applied', !!aPlaceAck);
  const bPlace = await bPlaceP;
  check('asset placement broadcast to second client (name+url)',
    bPlace.d.asset.name === 'ember' && !!bPlace.d.asset.url && bPlace.d.asset.remove !== true,
    JSON.stringify(bPlace.d.asset));

  doc = await worldDoc();
  check('asset placement persisted on reload',
    (doc.assets || []).some((p) => p.assetId === asset.id && p.x === 9 && p.y === 5 && !!p.url),
    JSON.stringify((doc.assets || []).filter((p) => p.x === 9)));

  // A removes the placement → B receives the remove broadcast → gone after reload
  const bRemoveP = waitEdit(bWS, (d) => d.op === 'asset' && d.asset && d.asset.remove === true && d.asset.x === 9, 'asset remove broadcast to B');
  sendEdit(aWS, { op: 'asset', asset: { assetId: asset.id, x: 9, y: 5, remove: true } });
  await waitEdit(aWS, (d) => d.op === 'asset' && d.applied && d.asset && d.asset.remove === true, 'asset remove ack');
  const bRemove = await bRemoveP;
  check('asset removal broadcast to second client', bRemove.d.asset.remove === true && bRemove.d.asset.x === 9,
    JSON.stringify(bRemove.d.asset));
  doc = await worldDoc();
  check('asset removal persisted on reload', !(doc.assets || []).some((p) => p.assetId === asset.id && p.x === 9 && p.y === 5),
    JSON.stringify((doc.assets || []).filter((p) => p.x === 9)));

  // ------------------------------------------------------------- animation
  // server-authoritative anims table rides every world doc
  const anims = (doc.anims || []).reduce((m, a) => { m[a.tileId] = a; return m; }, {});
  check('anims table present (torch 4f@7fps, glow 6f@5fps)',
    !!anims[20] && anims[20].frames === 4 && anims[20].fps === 7 &&
    !!anims[21] && anims[21].frames === 6 && anims[21].fps === 5 &&
    !!anims[2] && !!anims[8],
    JSON.stringify(doc.anims));

  // paint an animated tile (torch) → persists on reload
  // (single-cell batch acks come back in the single-cell x/y shape)
  sendEdit(aWS, { op: 'paint', tileId: 20, cells: [{ x: 10, y: 5 }] });
  await waitEdit(aWS, (d) => d.applied && ((d.x === 10 && d.y === 5) || (Array.isArray(d.cells) && d.cells.some((c) => c.x === 10 && c.y === 5))), 'torch stroke ack');
  doc = await worldDoc();
  check('animated tile (torch) persists on reload',
    tileAt(doc, 10, 5)?.tileId === 20 && tileAt(doc, 10, 5)?.t === 'torch',
    JSON.stringify(tileAt(doc, 10, 5)));

  aWS.close(); if (bWS) bWS.close();
  console.log(`\nRESULT: ${pass} pass, ${fail} fail`);
  process.exit(fail === 0 ? 0 : 1);
})().catch((e) => {
  console.error('FATAL', e.message);
  console.log(`\nRESULT: ${pass} pass, ${fail} fail`);
  process.exit(1);
});
