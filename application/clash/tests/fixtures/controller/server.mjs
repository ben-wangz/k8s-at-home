import { createServer } from 'node:http';
import { readFile } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';
import { resolve, basename } from 'node:path';
import { createRequire } from 'node:module';

const require = createRequire(new URL('../../browser/package.json', import.meta.url));
const { WebSocketServer, WebSocket } = require('ws');
const assets = fileURLToPath(new URL('../../../chart/files/console/', import.meta.url));
const sockets = new Set();
const levels = { debug: 0, info: 1, warning: 2, error: 3 };
let state;

function reset() {
  for (const socket of sockets) socket.close();
  state = { selected: { PROXY: 'Node A', 'Group / one': 'Node A' }, secret: '', outage: false, writes: 0, missingMetadata: false, emptyConnections: false, slow: false };
}

function inventory() {
  const proxies = {
    'Node A': { name: 'Node A', type: 'Http', alive: true, history: [{ delay: 12 }] },
    'Node / B': { name: 'Node / B', type: 'Http', history: [] },
    Blocked: { name: 'Blocked', type: 'Http' },
    DIRECT: { name: 'DIRECT', type: 'Direct' },
    Automatic: { name: 'Automatic', type: 'URLTest', all: ['Node A', 'Node / B'], now: 'Node A' },
  };
  for (const [name, selected] of Object.entries(state.selected)) proxies[name] = { name, type: 'Selector', all: ['Node A', 'Node / B', 'Blocked', 'DIRECT'], now: selected };
  return proxies;
}

function reply(response, status, value) {
  response.writeHead(status, { 'Content-Type': 'application/json' });
  response.end(value === undefined ? undefined : JSON.stringify(value));
}

async function body(request) {
  const values = [];
  for await (const part of request) values.push(part);
  return JSON.parse(Buffer.concat(values).toString() || '{}');
}

function emit(socket, value) { if (socket.readyState === WebSocket.OPEN) socket.send(JSON.stringify(value)); }

function logEvent(socket, index = 0, level = 'info') {
  if (levels[level] >= levels[socket.level || 'info']) emit(socket, { time: '12:00:00', level, message: `fixture event ${index}` });
}

const server = createServer(async (request, response) => {
  const url = new URL(request.url, 'http://127.0.0.1:43819');
  response.setHeader('Access-Control-Allow-Origin', request.headers.origin || '*');
  response.setHeader('Access-Control-Allow-Headers', 'Authorization, Content-Type');
  response.setHeader('Access-Control-Allow-Methods', 'GET, PUT, POST, OPTIONS');
  if (request.method === 'OPTIONS') { response.writeHead(204).end(); return; }
  if (url.pathname === '/__fixture') {
    if (request.method === 'POST') {
      const changes = await body(request);
      if (changes.reset) reset();
      Object.assign(state, changes);
      if (changes.outage) for (const socket of sockets) socket.close();
      if (changes.burst) for (const socket of sockets) if (socket.path === '/logs') for (let index = 0; index < changes.burst; index++) logEvent(socket, index, changes.level || 'info');
    }
    const counts = {};
    for (const socket of sockets) if (socket.readyState === WebSocket.OPEN) counts[socket.path] = (counts[socket.path] || 0) + 1;
    reply(response, 200, { writes: state.writes, counts });
    return;
  }
  if (url.pathname.startsWith('/ui/')) {
    const name = url.pathname === '/ui/' ? 'index.html' : basename(url.pathname);
    try {
      const content = await readFile(resolve(assets, name));
      const type = name.endsWith('.js') ? 'text/javascript' : name.endsWith('.css') ? 'text/css' : 'text/html';
      response.writeHead(200, { 'Content-Type': type }); response.end(content);
    } catch { response.writeHead(404).end(); }
    return;
  }
  const path = url.pathname.replace(/^\/other(?=\/)/, '');
  if (state.outage) { reply(response, 503, { message: 'fixture outage' }); return; }
  if (state.secret && request.headers.authorization !== `Bearer ${state.secret}`) { reply(response, 401, { message: 'Unauthorized' }); return; }
  if (path === '/version') { reply(response, 200, { version: 'v1.19.30-fixture' }); return; }
  if (path === '/proxies') { reply(response, 200, { proxies: inventory() }); return; }
  if (path.startsWith('/proxies/')) {
    const name = decodeURIComponent(path.slice('/proxies/'.length));
    if (!inventory()[name]) { reply(response, 404, {}); return; }
    if (request.method === 'PUT') {
      const value = await body(request);
      state.writes++;
      if (state.slow) await new Promise(done => setTimeout(done, 400));
      if (value.name === 'Blocked' || !inventory()[name].all?.includes(value.name)) { reply(response, 400, { message: 'Selection rejected' }); return; }
      state.selected[name] = value.name;
      reply(response, 204); return;
    }
    reply(response, 200, inventory()[name]); return;
  }
  reply(response, 404, {});
});

const websockets = new WebSocketServer({ noServer: true });
server.on('upgrade', (request, socket, head) => {
  const url = new URL(request.url, 'http://127.0.0.1:43819');
  if (state.outage || (state.secret && url.searchParams.get('token') !== state.secret)) { socket.write('HTTP/1.1 401 Unauthorized\r\n\r\n'); socket.destroy(); return; }
  websockets.handleUpgrade(request, socket, head, client => {
    client.path = url.pathname.replace(/^\/other(?=\/)/, '');
    client.level = url.searchParams.get('level');
    sockets.add(client);
    const tick = () => {
      if (client.path === '/traffic') emit(client, { up: 1024, down: 2048, upTotal: 1048576, downTotal: 2097152 });
      if (client.path === '/connections') emit(client, { connections: state.emptyConnections ? null : [{ id: 'fixture-connection', metadata: state.missingMetadata ? undefined : { host: 'traffic.fixture', destinationPort: '8080', sourceIP: '127.0.0.1', sourcePort: '1234', network: 'tcp', type: 'HTTP' }, chains: ['Node A', 'PROXY'], rule: 'Match', upload: 1024, download: 2048, start: '2026-01-01T12:00:00Z' }], uploadTotal: 1024, downloadTotal: 2048 });
    };
    tick();
    if (client.path === '/logs') logEvent(client);
    const timer = setInterval(tick, 150);
    client.on('close', () => { clearInterval(timer); sockets.delete(client); });
  });
});

reset();
server.listen(43819, '127.0.0.1');
for (const signal of ['SIGTERM', 'SIGINT']) process.on(signal, () => { for (const socket of sockets) socket.terminate(); server.close(() => process.exit(0)); });
