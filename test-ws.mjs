// Hearth WS end-to-end test — runs on the box with Node >= 21 (global WebSocket).
// Two clients: join -> move -> state round-trip -> chat (space + dm) -> rate limit -> portal.
const WS_URL = process.env.WS_URL || 'ws://127.0.0.1:8090/ws';

function connect(deviceKey, name) {
  return new Promise((resolve, reject) => {
    const ws = new WebSocket(WS_URL);
    const inbox = [];
    const waiters = [];
    const api = {
      ws,
      send: (m) => ws.send(JSON.stringify(m)),
      next: (type, timeout = 8000) =>
        new Promise((res, rej) => {
          const t = setTimeout(() => rej(new Error('timeout waiting for ' + type)), timeout);
          const idx = inbox.findIndex((m) => m.type === type);
          if (idx >= 0) { clearTimeout(t); res(inbox.splice(idx, 1)[0]); return; }
          waiters.push({ type, res: (m) => { clearTimeout(t); res(m); }, rej });
        }),
      drain: (type, timeout = 3000) =>
        new Promise((res) => {
          const t = setTimeout(() => res(0), timeout);
          const count = { n: 0 };
          waiters.push({
            type,
            res: (m) => { count.n++; if (count.n >= 1) { clearTimeout(t); res(count.n); } },
            rej: () => {},
          });
        }),
    };
    ws.onopen = () => resolve(api);
    ws.onmessage = (ev) => {
      const m = JSON.parse(ev.data);
      const i = waiters.findIndex((w) => w.type === m.type);
      if (i >= 0) { const w = waiters.splice(i, 1)[0]; w.res(m); } else { inbox.push(m); }
    };
    ws.onerror = (e) => reject(new Error('ws error'));
    ws.onclose = (e) => { api.closed = e.code; };
  });
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

(async () => {
  const results = {};
  const a = await connect('dev-a', 'Alice');
  a.send({ type: 'join', deviceKey: 'dev-a', name: 'Alice', spaceId: 'hearth' });
  const wa = await a.next('welcome');
  results.alice = { entityId: wa.entityId, sessionId: wa.sessionId, x: wa.x, y: wa.y, worldTiles: wa.world.tiles.length, portals: wa.world.portals.length };

  const b = await connect('dev-b', 'Bob');
  b.send({ type: 'join', deviceKey: 'dev-b', name: 'Bob', spaceId: 'hearth', x: 16, y: 18 });
  const wb = await b.next('welcome');
  results.bob = { entityId: wb.entityId, x: wb.x, y: wb.y };

  // move: Alice walks to Bob's tile; Bob's next state must include her at (16,17)
  a.send({ type: 'move', x: 16, y: 17, dir: 'down' });
  const st = await b.next('state');
  const aliceInState = (st.entities || []).find((e) => e.id === wa.entityId);
  results.move = { bobSawAlice: aliceInState ? { x: aliceInState.x, y: aliceInState.y } : null };

  // chat: space channel Alice -> Bob
  a.send({ type: 'chat', channel: 'space', text: 'hello from Alice' });
  const cm = await b.next('chat');
  results.spaceChat = { from: cm.from, text: cm.text, channel: cm.channel };

  // chat: dm Bob -> Alice
  b.send({ type: 'chat', channel: 'dm', to: wa.entityId, text: 'psst Alice' });
  const dm = await a.next('chat');
  results.dm = { from: dm.from, text: dm.text, channel: dm.channel };

  // rate limit: fire 6 chats instantly, expect at least one rate_limited error
  let errs = 0;
  const errWaiter = a.next('error', 6000).catch(() => null);
  for (let i = 0; i < 6; i++) a.send({ type: 'chat', channel: 'space', text: 'spam' + i });
  const err = await errWaiter;
  if (err && err.code === 'rate_limited') errs++;
  results.rateLimit = { gotRateLimited: errs > 0, code: err ? err.code : null };

  // signal relay A -> B and back
  a.send({ type: 'signal', to: wb.entityId, data: { sdp: 'offer-abc' }, dataType: 'offer' });
  const sig = await b.next('signal');
  results.signal = { fromIsAlice: sig.from === wa.entityId, hasData: !!sig.data };

  // media relay (pass-through)
  a.send({ type: 'media', to: wb.entityId, action: 'mute', data: { track: 'audio' } });
  const med = await b.next('media');
  results.media = { fromIsAlice: med.from === wa.entityId, action: med.action };

  // portal: Alice walks to garden-east portal (4,16) and steps through
  a.send({ type: 'move', x: 4, y: 16, dir: 'right' });
  await sleep(400);
  a.send({ type: 'portal', portalId: 'garden-east' });
  const pt = await a.next('portal');
  results.portal = { spaceId: pt.spaceId, x: pt.x, y: pt.y };

  // world fetch sanity via REST is done by curl; here just confirm post-portal state works
  const st2 = await a.next('state');
  results.postPortalState = { gotState: !!st2, spaceId: st2.spaceId };

  console.log('WS_TEST_RESULT ' + JSON.stringify(results, null, 2));
  a.ws.close(); b.ws.close();
  process.exit(0);
})().catch((e) => { console.error('WS_TEST_FAIL ' + e.message); process.exit(1); });
