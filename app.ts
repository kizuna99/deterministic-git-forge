// The forge's HTTP server: one process answering git smart HTTP, REST v3, GraphQL and MCP from a
// single in-memory store. createApp() builds the server; main.ts owns the harness lifecycle.

import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { JSON_PARSE_ERROR, type Ctx } from './ctx.ts';
import { store } from './store.ts';
import { handleRest } from './rest.ts';
import { handleGraphQL } from './graphql.ts';
import { handleMcp } from './mcp.ts';
import { infoRefs, receivePack, uploadPack } from './gitserver.ts';

const TOKEN = process.env.MAKEFUN_SHOP_TOKEN ?? '';
let requestCounter = 0;

function readBody(req: IncomingMessage): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    req.on('data', (chunk: Buffer) => chunks.push(chunk));
    req.on('end', () => resolve(Buffer.concat(chunks)));
    req.on('error', reject);
  });
}

export function createApp(): Server {
  const server = createServer((req: IncomingMessage, res: ServerResponse) => {
    void handle(req, res).catch(() => {
      if (!res.headersSent) {
        res.writeHead(500, { 'content-type': 'application/json; charset=utf-8' });
      }
      res.end(JSON.stringify({ message: 'Internal error' }));
    });
  });
  server.keepAliveTimeout = 30_000;
  server.headersTimeout = 35_000;
  return server;
}

async function handle(req: IncomingMessage, res: ServerResponse): Promise<void> {
  requestCounter += 1;
  const requestId = `forge-${requestCounter.toString(16).padStart(8, '0')}`;
  const method = (req.method ?? 'GET').toUpperCase();
  const rawUrl = req.url ?? '/';
  const queryStart = rawUrl.indexOf('?');
  const path = queryStart >= 0 ? rawUrl.slice(0, queryStart) : rawUrl;
  const searchParams = new URLSearchParams(queryStart >= 0 ? rawUrl.slice(queryStart + 1) : '');
  const host = req.headers.host ?? 'localhost';
  const origin = `http://${host}`;

  const send = (status: number, contentType: string, body: Buffer | string, headers: Record<string, string> = {}): void => {
    const payload = typeof body === 'string' ? Buffer.from(body, 'utf8') : body;
    res.writeHead(status, {
      'content-type': contentType,
      'content-length': String(payload.length),
      'x-github-request-id': requestId,
      ...headers,
    });
    res.end(payload);
  };
  const sendJson = (status: number, value: unknown, headers: Record<string, string> = {}): void => {
    send(status, 'application/json; charset=utf-8', JSON.stringify(value) + '\n', headers);
  };

  if (path === '/healthz') {
    send(200, 'text/plain', 'ok\n');
    return;
  }

  const authorized = TOKEN !== '' && req.headers.authorization === `Bearer ${TOKEN}`;

  const body = method === 'GET' || method === 'HEAD' ? Buffer.alloc(0) : await readBody(req);

  const segments = path
    .split('/')
    .filter((segment) => segment.length > 0)
    .map((segment) => {
      if (!segment.includes('%')) return segment;
      try {
        return decodeURIComponent(segment);
      } catch {
        return segment;
      }
    });

  // JSON body parsing is shared: a malformed body is a 400 on REST/GraphQL, a -32700 on MCP.
  let json: unknown;
  const contentType = req.headers['content-type'] ?? '';
  if (body.length > 0 && (contentType.includes('json') || path.startsWith('/api/') || path === '/mcp')) {
    try {
      json = JSON.parse(body.toString('utf8'));
    } catch {
      json = JSON_PARSE_ERROR;
    }
  }

  const ctx: Ctx = {
    method,
    path,
    segments,
    query: searchParams,
    body,
    json,
    origin,
    send,
    sendJson,
  };

  // MCP answers its own parse error before auth, so it gets the raw context first.
  if (path === '/mcp') {
    if (method === 'POST') {
      handleMcp(ctx, authorized);
    } else {
      sendJson(404, { message: 'Not Found' });
    }
    return;
  }

  // Everything else requires the bearer token.
  if (!authorized) {
    if (path.startsWith('/api/')) {
      sendJson(401, { message: 'Bad credentials' });
    } else {
      send(401, 'text/plain', 'unauthorized\n');
    }
    return;
  }

  if (path === '/api/graphql') {
    if (method === 'POST') handleGraphQL(ctx);
    else sendJson(404, { message: 'Not Found' });
    return;
  }

  if (path.startsWith('/api/v3')) {
    const handled = handleRest(ctx);
    if (!handled) sendJson(404, { message: 'Not Found' });
    return;
  }

  // Git smart HTTP: /{owner}/{repo}.git/...
  const gitMatch = /^\/([^/]+)\/([^/]+)\.git\/(info\/refs|git-upload-pack|git-receive-pack)$/.exec(path);
  if (gitMatch) {
    const owner = decodeURIComponent(gitMatch[1]);
    const name = decodeURIComponent(gitMatch[2]);
    const action = gitMatch[3];
    const repo = owner === 'adaiversen' ? store.getRepo(owner, name) : undefined;
    const gitSend = (status: number, type: string, payload: Buffer | string): void => send(status, type, payload);
    if (action === 'info/refs' && method === 'GET') {
      const service = searchParams.get('service') ?? '';
      const protocolV2 = (req.headers['git-protocol'] ?? '') === 'version=2';
      infoRefs(repo, service, protocolV2, gitSend);
      return;
    }
    if (action === 'git-upload-pack' && method === 'POST') {
      uploadPack(repo, body, gitSend);
      return;
    }
    if (action === 'git-receive-pack' && method === 'POST') {
      receivePack(repo, body, gitSend);
      return;
    }
    send(404, 'text/plain', 'not found\n');
    return;
  }

  if (path.startsWith('/api/')) {
    sendJson(404, { message: 'Not Found' });
    return;
  }
  send(404, 'text/plain', 'not found\n');
}
