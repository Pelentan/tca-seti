// ---------------------------------------------------------------------------
// TCA Router — stdlib only, zero external dependencies.
//
// Drop-in replacement for express routing in signal-clearance.
// Provides the same (req, res) handler interface as express so route handler
// bodies are unchanged. Only the app setup changes.
//
// Covers exactly what signal-clearance uses:
//   - Method + path routing (GET, POST, PUT, DELETE)
//   - URL parameter extraction (:param syntax)
//   - JSON body parsing
//   - Cookie parsing and setting
//   - Sub-router mounting (Router / app.use())
//
// Copy-per-Job discipline applies. Copy this file into any TCA Job that
// needs HTTP routing. Do not import from another Job.
// ---------------------------------------------------------------------------

import type { IncomingMessage, ServerResponse } from 'http';

// ---------------------------------------------------------------------------
// Request / Response types
// ---------------------------------------------------------------------------

export interface TCARequest {
  method:  string;
  url:     string;
  headers: Record<string, string | string[] | undefined>;
  params:  Record<string, string>;
  query:   Record<string, string>;
  cookies: Record<string, string>;
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  body:    any;
  raw:     IncomingMessage;
}

export interface TCAResponse {
  raw:        ServerResponse;
  statusCode: number;
  status(code: number): TCAResponse;
  json(data: unknown): void;
  send(body: string, contentType?: string): void;
  cookie(name: string, value: string, options?: CookieOptions): TCAResponse;
  clearCookie(name: string, options?: Pick<CookieOptions, 'path'>): TCAResponse;
  redirect(url: string, code?: number): void;
  setHeader(name: string, value: string): TCAResponse;
}

export interface CookieOptions {
  httpOnly?:  boolean;
  secure?:    boolean;
  sameSite?:  'strict' | 'lax' | 'none';
  maxAge?:    number;   // milliseconds (express convention)
  path?:      string;
  domain?:    string;
}

export type Handler = (req: TCARequest, res: TCAResponse) => void | Promise<void>;

// ---------------------------------------------------------------------------
// Route matching
// ---------------------------------------------------------------------------

interface Route {
  method:  string;             // uppercase: GET, POST, etc. — '*' for use()
  pattern: RegExp;
  params:  string[];           // param names in order
  handler: Handler;
  prefix?: boolean;            // true for app.use() prefix matches
}

function compilePath(path: string, prefix = false): { pattern: RegExp; params: string[] } {
  const params: string[] = [];
  // Escape special regex chars except /:*
  const escaped = path
    .replace(/[.+?^${}()|[\]\\]/g, '\\$&')
    .replace(/:([a-zA-Z_][a-zA-Z0-9_]*)/g, (_m, name) => {
      params.push(name);
      return '([^/]+)';
    });
  const pattern = prefix
    ? new RegExp(`^${escaped}(?:/|$)`)
    : new RegExp(`^${escaped}$`);
  return { pattern, params };
}

// ---------------------------------------------------------------------------
// Router class
// ---------------------------------------------------------------------------

export class Router {
  protected routes: Route[] = [];

  get(path: string, handler: Handler): this {
    const { pattern, params } = compilePath(path);
    this.routes.push({ method: 'GET', pattern, params, handler });
    return this;
  }

  post(path: string, handler: Handler): this {
    const { pattern, params } = compilePath(path);
    this.routes.push({ method: 'POST', pattern, params, handler });
    return this;
  }

  put(path: string, handler: Handler): this {
    const { pattern, params } = compilePath(path);
    this.routes.push({ method: 'PUT', pattern, params, handler });
    return this;
  }

  delete(path: string, handler: Handler): this {
    const { pattern, params } = compilePath(path);
    this.routes.push({ method: 'DELETE', pattern, params, handler });
    return this;
  }

  // Mount a sub-router under a prefix
  use(prefixOrRouter: string | Router, subRouter?: Router): this {
    const prefix = typeof prefixOrRouter === 'string' ? prefixOrRouter : '';
    const router = typeof prefixOrRouter === 'string' ? subRouter! : prefixOrRouter;

    for (const route of router.routes) {
      if (!prefix) {
        // No prefix — mount routes directly with their original patterns
        this.routes.push({ ...route });
      } else {
        // Prefix mount — combine prefix pattern with route pattern
        const { pattern: prefixPattern } = compilePath(prefix, true);
        const combined = new RegExp(
          prefixPattern.source + route.pattern.source.replace(/^\^/, '')
        );
        this.routes.push({
          method:  route.method,
          pattern: combined,
          params:  route.params,
          handler: route.handler,
        });
      }
    }
    return this;
  }

  // Internal: try to match a method + path against registered routes
  match(method: string, path: string): { handler: Handler; params: Record<string, string> } | null {
    for (const route of this.routes) {
      if (route.method !== method && route.method !== '*') continue;
      const m = path.match(route.pattern);
      if (!m) continue;
      const params: Record<string, string> = {};
      route.params.forEach((name, i) => { params[name] = decodeURIComponent(m[i + 1] ?? ''); });
      return { handler: route.handler, params };
    }
    return null;
  }
}

// ---------------------------------------------------------------------------
// App — top-level router that also dispatches raw Node requests
// ---------------------------------------------------------------------------

export class App extends Router {

  // Dispatch a raw Node.js IncomingMessage / ServerResponse pair
  async dispatch(raw: IncomingMessage, rawRes: ServerResponse): Promise<void> {
    const method = (raw.method ?? 'GET').toUpperCase();
    const fullUrl = raw.url ?? '/';
    const [pathname, queryString] = fullUrl.split('?');

    // Parse body
    const body = await readBody(raw);

    // Parse cookies
    const cookies = parseCookies(raw.headers['cookie']);

    // Parse query string
    const query = parseQuery(queryString);

    // Build the match
    const matched = this.match(method, pathname);

    if (!matched) {
      rawRes.writeHead(404, { 'Content-Type': 'application/json' });
      rawRes.end(JSON.stringify({ code: 'NOT_FOUND', message: `${method} ${pathname} not found` }));
      return;
    }

    const req = buildRequest(raw, matched.params, body, cookies, query);
    const res = buildResponse(rawRes);

    try {
      await matched.handler(req, res);
    } catch (err) {
      console.error('[router] Unhandled handler error:', err);
      if (!rawRes.headersSent) {
        rawRes.writeHead(500, { 'Content-Type': 'application/json' });
        rawRes.end(JSON.stringify({ code: 'INTERNAL_ERROR', message: 'An internal error occurred' }));
      }
    }
  }
}

// ---------------------------------------------------------------------------
// Body parsing — accumulate chunks, parse JSON
// ---------------------------------------------------------------------------

// eslint-disable-next-line @typescript-eslint/no-explicit-any
function readBody(raw: IncomingMessage): Promise<any> {
  return new Promise((resolve) => {
    const contentType = raw.headers['content-type'] ?? '';
    if (!contentType.includes('application/json')) {
      resolve({});
      return;
    }

    let data = '';
    raw.on('data', (chunk: Buffer) => { data += chunk.toString(); });
    raw.on('end', () => {
      if (!data) { resolve({}); return; }
      try {
        resolve(JSON.parse(data) as Record<string, unknown>);
      } catch {
        resolve({});
      }
    });
    raw.on('error', () => resolve({}));
  });
}

// ---------------------------------------------------------------------------
// Cookie parsing
// ---------------------------------------------------------------------------

function parseCookies(header: string | string[] | undefined): Record<string, string> {
  // Object.create(null) — no prototype, prevents __proto__ / constructor injection.
  const cookies: Record<string, string> = Object.create(null) as Record<string, string>;
  if (!header) return cookies;
  const cookieStr = Array.isArray(header) ? header.join('; ') : header;
  for (const pair of cookieStr.split(';')) {
    const eqIdx = pair.indexOf('=');
    if (eqIdx === -1) continue;
    const name  = pair.slice(0, eqIdx).trim();
    const value = pair.slice(eqIdx + 1).trim();
    if (name) cookies[name] = decodeURIComponent(value);
  }
  return cookies;
}

// ---------------------------------------------------------------------------
// Query string parsing
// ---------------------------------------------------------------------------

function parseQuery(qs: string | undefined): Record<string, string> {
  // Object.create(null) — no prototype, prevents __proto__ / constructor injection.
  const query: Record<string, string> = Object.create(null) as Record<string, string>;
  if (!qs) return query;
  for (const pair of qs.split('&')) {
    const eqIdx = pair.indexOf('=');
    if (eqIdx === -1) continue;
    const key = decodeURIComponent(pair.slice(0, eqIdx));
    const val = decodeURIComponent(pair.slice(eqIdx + 1));
    query[key] = val;
  }
  return query;
}

// ---------------------------------------------------------------------------
// Request / Response builders
// ---------------------------------------------------------------------------

function buildRequest(
  raw:     IncomingMessage,
  params:  Record<string, string>,
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  body:    any,
  cookies: Record<string, string>,
  query:   Record<string, string>,
): TCARequest {
  return {
    method:  (raw.method ?? 'GET').toUpperCase(),
    url:     raw.url ?? '/',
    headers: raw.headers as Record<string, string | string[] | undefined>,
    params,
    query,
    cookies,
    body,
    raw,
  };
}

function buildResponse(rawRes: ServerResponse): TCAResponse {
  let statusCode = 200;
  const pendingCookies: string[] = [];

  const res: TCAResponse = {
    raw:        rawRes,
    statusCode,

    status(code: number): TCAResponse {
      statusCode = code;
      return res;
    },

    json(data: unknown): void {
      const body = JSON.stringify(data);
      rawRes.writeHead(statusCode, {
        'Content-Type':   'application/json',
        'Content-Length': Buffer.byteLength(body),
        ...(pendingCookies.length ? { 'Set-Cookie': pendingCookies } : {}),
      });
      rawRes.end(body);
    },

    send(body: string, contentType = 'text/plain'): void {
      rawRes.writeHead(statusCode, {
        'Content-Type':   contentType,
        'Content-Length': Buffer.byteLength(body),
        ...(pendingCookies.length ? { 'Set-Cookie': pendingCookies } : {}),
      });
      rawRes.end(body);
    },

    cookie(name: string, value: string, options: CookieOptions = {}): TCAResponse {
      pendingCookies.push(buildSetCookieHeader(name, value, options));
      return res;
    },

    clearCookie(name: string, options: Pick<CookieOptions, 'path'> = {}): TCAResponse {
      pendingCookies.push(buildSetCookieHeader(name, '', {
        path:    options.path ?? '/',
        maxAge:  0,
        httpOnly: true,
      }));
      return res;
    },

    redirect(url: string, code = 302): void {
      rawRes.writeHead(code, { Location: url });
      rawRes.end();
    },

    setHeader(name: string, value: string): TCAResponse {
      rawRes.setHeader(name, value);
      return res;
    },
  };

  return res;
}

// ---------------------------------------------------------------------------
// Set-Cookie header builder
// ---------------------------------------------------------------------------

function buildSetCookieHeader(name: string, value: string, options: CookieOptions): string {
  let cookie = `${name}=${encodeURIComponent(value)}`;

  if (options.path)     cookie += `; Path=${options.path}`;
  if (options.domain)   cookie += `; Domain=${options.domain}`;
  if (options.maxAge !== undefined) {
    // express maxAge is milliseconds; Set-Cookie Max-Age is seconds
    const maxAgeSec = Math.floor(options.maxAge / 1000);
    cookie += `; Max-Age=${maxAgeSec}`;
  }
  if (options.httpOnly) cookie += '; HttpOnly';
  if (options.secure)   cookie += '; Secure';
  if (options.sameSite) cookie += `; SameSite=${options.sameSite.charAt(0).toUpperCase() + options.sameSite.slice(1)}`;

  return cookie;
}
