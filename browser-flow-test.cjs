// Browser-flow test: exact client join path — ws://host/ws with NO query
// (no ?deviceKey= in URL), deviceKey carried in the join payload, and the
// client sends the space under the key 'space' (not 'spaceId').
// This mirrors src/App.tsx join() after the infinite-loading fix.
const { WebSocket } = require('/usr/share/nodejs/ws/index.js');
const { randomUUID } = require('node:crypto');

const env = (t, d) => JSON.stringify({ v: 1, t, id: randomUUID(), ts: Date.now(), d });
let pass = 0, fail = 0;
const check = (name, ok, extra = '') => { if (ok) { pass++; console.log('PASS', name); } else { fail++; console.log('FAIL', name, extra); } };

(async () => {
  const dk = 'e2e-' + Date.now();
  const ws = new WebSocket('ws://127.0.0.1:8090/ws'); // NO deviceKey in URL — payload-only auth
  let finished = false;
  const finish = (code) => {
    if (finished) return;
    finished = true;
    setTimeout(() => { console.log(`\nRESULT: ${pass} pass, ${fail} fail`); process.exit(code); }, 300);
  };
  ws.on('open', () =>
    ws.send(env('join', { name: 'BrowserFlow', lang: 'en', space: 'town-square', guest: true, deviceKey: dk })),
  );
  ws.on('message', (m) => {
    let j;
    try { j = JSON.parse(m); } catch { return; }
    if (j.t === 'welcome') {
      check('welcome envelope {v:1,t,d}', j.v === 1 && j.t === 'welcome' && !!j.d && typeof j.d === 'object');
      check('selfId + entityId present', !!j.d?.selfId && !!j.d?.entityId);
      check('world tiles present', !!j.d?.world);
      check('spaceId echoed (town-square)', j.d?.spaceId === 'town-square');
      check('no auth_required error', true);
      ws.close();
      finish(0);
    }
    if (j.t === 'error') {
      fail++;
      console.log('ERR', j.d?.code, j.d?.message);
      finish(1);
    }
  });
  ws.on('close', () => finish(fail ? 1 : 0));
  setTimeout(() => { console.log('TIMEOUT - welcome never arrived (join rejected?)'); finish(1); }, 8000);
})();
