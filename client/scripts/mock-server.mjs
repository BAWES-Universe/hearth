// Hearth mock server for client smoke tests — NOT the real Go server.
// Zero deps: raw RFC6455 WebSocket + static dist serving on :8090.
// Serves: GET / (client dist), GET /api/spaces/:id, WS /ws per PROTOCOL.md.
// Protocol basics: welcome (world + roster with 2 moving bots), state
// broadcasts ~4Hz, chat echo to all, edit broadcast.

import http from 'node:http';
import { readFile } from 'node:fs/promises';
import { createHash, randomUUID } from 'node:crypto';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const DIST = path.resolve(__dirname, '..', 'dist');
const PORT = Number(process.env.PORT || 8090);

const MIME = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.json': 'application/json',
  '.png': 'image/png',
  '.svg': 'image/svg+xml',
  '.ico': 'image/x-icon',
  '.woff2': 'font/woff2',
  '.webmanifest': 'application/manifest+json',
};

// ---------------------------------------------------------------- world gen
function mulberry32(seed) {
  let a = seed >>> 0;
  return () => {
    a |= 0;
    a = (a + 0x6d2b79f5) | 0;
    let t = Math.imul(a ^ (a >>> 15), 1 | a);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

function genWorld(w = 32, h = 32) {
  const rnd = mulberry32(42);
  const g = Array.from({ length: h }, () => new Array(w).fill(0));
  for (let x = 0; x < w; x++) {
    g[0][x] = 1;
    g[h - 1][x] = 1;
  }
  for (let y = 0; y < h; y++) {
    g[y][0] = 1;
    g[y][w - 1] = 1;
  }
  const rooms = [
    [5, 5, 9, 9],
    [16, 4, 22, 10],
    [6, 14, 13, 20],
    [19, 16, 26, 26],
  ];
  for (const [x1, y1, x2, y2] of rooms)
    for (let y = y1; y <= y2; y++) for (let x = x1; x <= x2; x++) if (g[y][x] !== 1) g[y][x] = 0;
  for (let i = 0; i < 14; i++) {
    const x = 3 + Math.floor(rnd() * (w - 6));
    const y = 3 + Math.floor(rnd() * (h - 6));
    const len = 2 + Math.floor(rnd() * 4);
    const horiz = rnd() > 0.5;
    for (let k = 0; k < len; k++) {
      const xx = horiz ? x + k : x;
      const yy = horiz ? y : y + k;
      if (xx > 0 && xx < w - 1 && yy > 0 && yy < h - 1 && g[yy][xx] === 0) g[yy][xx] = 1;
    }
  }
  const px = Math.floor(w / 2) + 1;
  const py = Math.floor(h / 2) + 1;
  for (let dy = -2; dy <= 2; dy++)
    for (let dx = -2; dx <= 2; dx++)
      if (Math.abs(dx) + Math.abs(dy) <= 2) {
        const xx = px + dx;
        const yy = py + dy;
        if (g[yy][xx] !== 1) g[yy][xx] = 2;
      }
  const tiles = [];
  for (let y = 0; y < h; y++) for (let x = 0; x < w; x++) tiles.push({ x, y, tileId: g[y][x] });
  return { id: 'town-square', name: 'town-square', width: w, height: h, tiles };
}

const WORLD = genWorld();

// ------------------------------------------------------------------ clients
const clients = new Map(); // socket -> { id, name, x, y, dir }
let uid = 0;
let chatSeq = 0;

const bots = [
  { id: 'bot-ember', name: 'Ember', cx: 8.5, cy: 9.5, r: 3.2, phase: 0.5, x: 8.5, y: 9.5, dir: 'down' },
  { id: 'bot-coal', name: 'Coal', cx: 21.5, cy: 20.5, r: 2.8, phase: 1.3, x: 21.5, y: 20.5, dir: 'left' },
];

// ----------------------------------------------------------------- WS frames
const WS_MAGIC = '258EAFA5-E914-47DA-95CA-C5AB0DC85B11';

function frame(opcode, payload) {
  const buf = Buffer.from(payload);
  let header;
  if (buf.length < 126) header = Buffer.from([0x80 | opcode, buf.length]);
  else if (buf.length < 65536) {
    header = Buffer.alloc(4);
    header[0] = 0x80 | opcode;
    header[1] = 126;
    header.writeUInt16BE(buf.length, 2);
  } else {
    header = Buffer.alloc(10);
    header[0] = 0x80 | opcode;
    header[1] = 127;
    header.writeBigUInt64BE(BigInt(buf.length), 2);
  }
  return Buffer.concat([header, buf]);
}

function send(ws, t, d) {
  if (ws.destroyed) return;
  ws.write(frame(0x1, JSON.stringify({ v: 1, t, id: randomUUID(), ts: Date.now(), d })));
}

function handleSocket(socket, req) {
  const key = req.headers['sec-websocket-key'];
  const accept = createHash('sha1').update(key + WS_MAGIC).digest('base64');
  socket.write(
    'HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n' +
      `Sec-WebSocket-Accept: ${accept}\r\n\r\n`,
  );

  const client = { socket, id: null, name: '', x: 16.5, y: 16.5, dir: 'down' };
  let buf = Buffer.alloc(0);

  const cleanup = () => {
    clients.delete(socket);
  };
  socket.on('error', cleanup);
  socket.on('close', cleanup);

  socket.on('data', (chunk) => {
    buf = Buffer.concat([buf, chunk]);
    for (;;) {
      if (buf.length < 2) return;
      const b0 = buf[0];
      const b1 = buf[1];
      const opcode = b0 & 0x0f;
      let len = b1 & 0x7f;
      let off = 2;
      if (len === 126) {
        if (buf.length < 4) return;
        len = buf.readUInt16BE(2);
        off = 4;
      } else if (len === 127) {
        if (buf.length < 10) return;
        len = Number(buf.readBigUInt64BE(2));
        off = 10;
      }
      const masked = (b1 & 0x80) !== 0;
      let mask = null;
      if (masked) {
        if (buf.length < off + 4) return;
        mask = buf.subarray(off, off + 4);
        off += 4;
      }
      if (buf.length < off + len) return;
      let payload = buf.subarray(off, off + len);
      if (masked) {
        payload = Buffer.from(payload);
        for (let i = 0; i < payload.length; i++) payload[i] ^= mask[i & 3];
      }
      buf = buf.subarray(off + len);

      if (opcode === 0x8) {
        socket.end();
        return;
      }
      if (opcode === 0x9) {
        socket.write(frame(0xa, payload));
        continue;
      }
      if (opcode === 0xa) continue;
      if (opcode === 0x1) {
        try {
          onMessage(client, JSON.parse(payload.toString('utf8')));
        } catch {
          /* bad json — ignore */
        }
      }
    }
  });
}

function onMessage(c, m) {
  if (!m || typeof m !== 'object') return;
  switch (m.t) {
    case 'join': {
      c.id = `u${++uid}`;
      c.name = String(m.d?.name || 'guest').slice(0, 24);
      clients.set(c.socket, c);
      send(c.socket, 'welcome', {
        selfId: c.id,
        space: String(m.d?.space || 'town-square'),
        world: WORLD,
        roster: [
          { id: 'bot-ember', name: 'Ember', x: bots[0].x, y: bots[0].y, dir: 'down' },
          { id: 'bot-coal', name: 'Coal', x: bots[1].x, y: bots[1].y, dir: 'left' },
          ...[...clients.values()]
            .filter((o) => o !== c && o.id)
            .map((o) => ({ id: o.id, name: o.name, x: o.x, y: o.y, dir: o.dir })),
        ],
      });
      break;
    }
    case 'move': {
      if (typeof m.d?.x === 'number') c.x = m.d.x;
      if (typeof m.d?.y === 'number') c.y = m.d.y;
      if (typeof m.d?.dir === 'string') c.dir = m.d.dir;
      break;
    }
    case 'chat': {
      const d = {
        channel: m.d?.channel === 'space' ? 'space' : 'proximity',
        from: c.id,
        text: String(m.d?.text || '').slice(0, 2000),
        seq: ++chatSeq,
      };
      for (const o of clients.values()) send(o.socket, 'chat', d);
      break;
    }
    case 'edit': {
      const d = { op: m.d?.op, x: m.d?.x, y: m.d?.y, tileId: m.d?.tileId, by: c.id };
      for (const o of clients.values()) send(o.socket, 'edit', d);
      break;
    }
    case 'signal':
    case 'media':
    case 'portal':
    case 'bot_msg':
      break; // ack silently — not needed for client smoke tests
    default:
      send(c.socket, 'error', { code: 'UNKNOWN', msg: `unknown type: ${m.t}` });
  }
}

// ----------------------------------------------------------- state broadcast
setInterval(() => {
  const t = Date.now() / 1000;
  for (const b of bots) {
    b.x = b.cx + Math.cos(t * 0.5 + b.phase * 6) * b.r;
    b.y = b.cy + Math.sin(t * 0.7 + b.phase * 6) * b.r;
    b.dir = Math.sin(t * 0.9 + b.phase) > 0 ? 'right' : 'left';
  }
  const states = [
    ...bots.map((b) => ({ id: b.id, x: +b.x.toFixed(2), y: +b.y.toFixed(2), dir: b.dir })),
    ...[...clients.values()].filter((c) => c.id).map((c) => ({ id: c.id, x: c.x, y: c.y, dir: c.dir })),
  ];
  for (const c of clients.values()) send(c.socket, 'state', states);
}, 250);

// ----------------------------------------------------------------- HTTP
const server = http.createServer(async (req, res) => {
  const u = new URL(req.url, 'http://localhost');
  res.setHeader('Access-Control-Allow-Origin', '*');
  if (req.method === 'GET' && u.pathname === '/health') {
    res.writeHead(200, { 'content-type': 'application/json' });
    res.end(JSON.stringify({ ok: true }));
    return;
  }
  if (req.method === 'GET' && u.pathname.startsWith('/api/spaces/')) {
    const id = decodeURIComponent(u.pathname.split('/').pop() || '');
    res.writeHead(200, { 'content-type': 'application/json' });
    res.end(JSON.stringify({ ...WORLD, id }));
    return;
  }
  if (req.method === 'GET') {
    const rel = u.pathname === '/' ? '/index.html' : u.pathname;
    const fp = path.join(DIST, rel);
    try {
      const data = await readFile(fp);
      res.writeHead(200, { 'content-type': MIME[path.extname(fp)] || 'application/octet-stream' });
      res.end(data);
      return;
    } catch {
      /* try SPA fallback */
    }
    try {
      const data = await readFile(path.join(DIST, 'index.html'));
      res.writeHead(200, { 'content-type': 'text/html; charset=utf-8' });
      res.end(data);
      return;
    } catch {
      res.writeHead(404, { 'content-type': 'text/plain' });
      res.end('mock: no dist/ build found — run npm run build first');
      return;
    }
  }
  res.writeHead(405);
  res.end();
});

server.on('upgrade', (req, socket) => handleSocket(socket, req));

server.listen(PORT, () => {
  console.log(`[hearth-mock] listening on :${PORT} — static: ${DIST}`);
});
