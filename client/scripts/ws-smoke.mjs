// Smoke-test the mock server's WS protocol from a raw node client.
// Connects, joins, expects welcome + state broadcasts, sends chat, expects echo.
import crypto from 'node:crypto';
import net from 'node:net';

const HOST = 'localhost';
const PORT = 8090;
const WS_MAGIC = '258EAFA5-E914-47DA-95CA-C5AB0DC85B11';

function encodeClientFrame(str) {
  const payload = Buffer.from(str, 'utf8');
  const mask = crypto.randomBytes(4);
  const masked = Buffer.from(payload);
  for (let i = 0; i < masked.length; i++) masked[i] ^= mask[i & 3];
  let header;
  if (payload.length < 126) header = Buffer.from([0x81, 0x80 | payload.length]);
  else {
    header = Buffer.alloc(4);
    header[0] = 0x81;
    header[1] = 0x80 | 126;
    header.writeUInt16BE(payload.length, 2);
  }
  return Buffer.concat([header, mask, masked]);
}

function decodeServerFrame(buf) {
  if (buf.length < 2) return null;
  const opcode = buf[0] & 0x0f;
  let len = buf[1] & 0x7f;
  let off = 2;
  if (len === 126) { len = buf.readUInt16BE(2); off = 4; }
  else if (len === 127) { len = Number(buf.readBigUInt64BE(2)); off = 10; }
  if (buf.length < off + len) return null;
  return { opcode, payload: buf.subarray(off, off + len).toString('utf8'), rest: buf.subarray(off + len) };
}

const sock = net.connect(PORT, HOST, () => {
  const key = crypto.randomBytes(16).toString('base64');
  sock.write(
    `GET /ws HTTP/1.1\r\nHost: ${HOST}:${PORT}\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n` +
      `Sec-WebSocket-Key: ${key}\r\nSec-WebSocket-Version: 13\r\n\r\n`,
  );
});

let buf = Buffer.alloc(0);
let sawWelcome = false;
let sawState = false;
let sawChatEcho = false;
let joined = false;

const timeout = setTimeout(() => {
  console.log(JSON.stringify({ sawWelcome, sawState, sawChatEcho, PASS: sawWelcome && sawState && sawChatEcho }));
  process.exit(sawWelcome && sawState && sawChatEcho ? 0 : 1);
}, 6000);

sock.on('data', (chunk) => {
  buf = Buffer.concat([buf, chunk]);
  if (!joined) {
    const idx = buf.indexOf('\r\n\r\n');
    if (idx === -1) return;
    const head = buf.subarray(0, idx).toString();
    if (!head.includes('101')) {
      console.log('HANDSHAKE FAILED:', head.split('\r\n')[0]);
      process.exit(1);
    }
    buf = buf.subarray(idx + 4);
    joined = true;
    sock.write(encodeClientFrame(JSON.stringify({ v: 1, t: 'join', id: crypto.randomUUID(), ts: Date.now(), d: { name: 'node-test', lang: 'en', space: 'town-square', guest: true } })));
    setTimeout(() => sock.write(encodeClientFrame(JSON.stringify({ v: 1, t: 'chat', id: crypto.randomUUID(), ts: Date.now(), d: { channel: 'proximity', text: 'hello from node', nonce: 'n1' } }))), 300);
  }
  for (;;) {
    const f = decodeServerFrame(buf);
    if (!f) break;
    buf = f.rest;
    if (f.opcode === 0x8) { console.log('server closed'); process.exit(1); }
    if (f.opcode !== 0x1) continue;
    const m = JSON.parse(f.payload);
    if (m.t === 'welcome') {
      sawWelcome = true;
      console.log('WELCOME selfId=' + m.d.selfId, 'roster=' + (m.d.roster || []).map((r) => r.id).join(','));
    } else if (m.t === 'state') {
      sawState = true;
    } else if (m.t === 'chat') {
      if (m.d.text === 'hello from node' && typeof m.d.from === 'string') sawChatEcho = true;
      console.log('CHAT echo from=' + m.d.from + ' text=' + m.d.text);
    }
  }
});
