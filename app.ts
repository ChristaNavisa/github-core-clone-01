// The server: one Forge, one HTTP server. Routing, auth and the request-id
// header live here; the four surfaces live in git.ts, rest.ts, graphql.ts and
// mcp.ts. Every reply carries X-GitHub-Request-Id, including 401s and 404s.

import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { Forge } from './store.ts';
import { handleGit } from './git.ts';
import { handleRest } from './rest.ts';
import { handleGraphql } from './graphql.ts';
import { handleMcp } from './mcp.ts';
import { errorResult, jsonResult, nextRequestId, type HandlerResult } from './web.ts';

const MAX_BODY_BYTES = 64 * 1024 * 1024;

/** Answer one request; the request-id header goes on every reply. */
function sendResult(res: ServerResponse, result: HandlerResult): void {
  const headers: Record<string, string> = {
    'content-type': result.contentType,
    'content-length': String(result.body.length),
    'x-github-request-id': nextRequestId(),
    ...(result.headers ?? {}),
  };
  res.writeHead(result.status, headers);
  res.end(result.body);
}

/** The bearer token this entry accepts, or null when no token is configured. Read once: the token is fixed before the server starts and never changes during a trial. */
let cachedExpectedToken: string | null | undefined;
function expectedToken(): string | null {
  if (cachedExpectedToken === undefined) {
    const token = process.env.MAKEFUN_SHOP_TOKEN;
    cachedExpectedToken = token === undefined || token === '' ? null : token;
  }
  return cachedExpectedToken;
}

function authorized(req: IncomingMessage): boolean {
  const auth = req.headers.authorization;
  if (typeof auth !== 'string' || !auth.startsWith('Bearer ')) return false;
  const token = auth.slice('Bearer '.length);
  if (token === '') return false;
  const expected = expectedToken();
  if (expected === null) return true;
  return token === expected;
}

function readBody(req: IncomingMessage): Promise<Buffer> {
  return new Promise<Buffer>((resolve, reject) => {
    const chunks: Buffer[] = [];
    let size = 0;
    req.on('data', (chunk: Buffer) => {
      size += chunk.length;
      if (size > MAX_BODY_BYTES) {
        reject(new Error('request body is too large'));
        req.destroy();
        return;
      }
      chunks.push(chunk);
    });
    req.on('end', () => resolve(Buffer.concat(chunks)));
    req.on('error', reject);
  });
}

function splitPath(pathname: string): string[] {
  return pathname.split('/').filter((s) => s !== '').map((s) => {
    if (!s.includes('%')) return s;
    try {
      return decodeURIComponent(s);
    } catch {
      return s;
    }
  });
}

/** Build the server. It binds nothing until the caller listens; the entry listens on an ephemeral port. */
export function createApp(): Server {
  const forge = new Forge();
  return createServer((req: IncomingMessage, res: ServerResponse) => {
    void (async () => {
      let url: URL;
      try {
        url = new URL(req.url ?? '/', 'http://entry');
      } catch {
        sendResult(res, errorResult(400, 'Bad Request'));
        return;
      }
      const method = req.method ?? 'GET';
      const segments = splitPath(url.pathname);
      const host = req.headers.host ?? '127.0.0.1';

      // The env's boot check calls this twice: 200 {"ok":true}. No auth here.
      if (method === 'GET' && url.pathname === '/healthz') {
        sendResult(res, jsonResult(200, { ok: true }));
        return;
      }

      // Everything else needs a bearer token (any non-empty one when the env
      // did not set MAKEFUN_SHOP_TOKEN, the exact one when it did).
      if (!authorized(req)) {
        sendResult(res, jsonResult(401, { message: 'Bad credentials' }));
        return;
      }

      let body = Buffer.alloc(0);
      if (method === 'POST' || method === 'PUT' || method === 'PATCH' || method === 'DELETE') {
        try {
          body = await readBody(req);
        } catch {
          sendResult(res, errorResult(413, 'Request Entity Too Large'));
          return;
        }
      }

      const headers: Record<string, string | undefined> = {};
      for (const [key, value] of Object.entries(req.headers)) {
        headers[key.toLowerCase()] = Array.isArray(value) ? value[0] : value;
      }

      // MCP: a body that is not JSON is a JSON-RPC parse error.
      if (segments.length === 1 && segments[0] === 'mcp') {
        let json: unknown = null;
        if (body.length > 0) {
          try {
            json = JSON.parse(body.toString('utf8')) as unknown;
          } catch {
            sendResult(res, jsonResult(400, { jsonrpc: '2.0', id: null, error: { code: -32700, message: 'Parse error: request body is not valid JSON' } }));
            return;
          }
        }
        sendResult(res, handleMcp(forge, { method, segments, query: url.searchParams, headers, json }));
        return;
      }

      // GraphQL v4.
      if (segments.length === 2 && segments[0] === 'api' && segments[1] === 'graphql') {
        let json: unknown = null;
        if (body.length > 0) {
          try {
            json = JSON.parse(body.toString('utf8')) as unknown;
          } catch {
            sendResult(res, jsonResult(400, { errors: [{ message: 'Problems parsing JSON' }] }));
            return;
          }
        }
        sendResult(res, handleGraphql(forge, { method, segments, query: url.searchParams, headers, json }));
        return;
      }

      // REST v3.
      if (segments.length >= 2 && segments[0] === 'api' && segments[1] === 'v3') {
        let json: unknown = null;
        if (body.length > 0) {
          try {
            json = JSON.parse(body.toString('utf8')) as unknown;
          } catch {
            sendResult(res, errorResult(400, 'Problems parsing JSON'));
            return;
          }
        }
        sendResult(res, handleRest(forge, { method, segments: segments.slice(2), query: url.searchParams, headers, json, host }));
        return;
      }

      // git smart HTTP: /{owner}/{repo}.git/...
      if (segments.length >= 2 && (segments[1] as string).endsWith('.git')) {
        sendResult(res, handleGit(forge, { method, segments, query: url.searchParams, headers, body }));
        return;
      }

      sendResult(res, jsonResult(404, { message: 'Not Found' }));
    })().catch(() => {
      if (!res.headersSent) {
        try {
          sendResult(res, errorResult(500, 'Internal Server Error'));
        } catch {
          res.destroy();
        }
      }
    });
  });
}
