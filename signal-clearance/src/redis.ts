// ---------------------------------------------------------------------------
// TCA Redis Client — stdlib only, zero external dependencies.
//
// Implements the RESP2 wire protocol over Node.js net.Socket.
// Covers only the commands used by signal-clearance session management:
//   SET key value EX ttl
//   GET key
//   DEL key
//   SCAN cursor MATCH pattern COUNT n
//
// Each command opens a fresh TCP connection, sends the command, reads the
// full response, and closes. No connection pooling — session operations are
// low-frequency and correctness is more important than throughput here.
// ---------------------------------------------------------------------------

import net from 'net';

const CRLF = '\r\n';

// ---------------------------------------------------------------------------
// RESP2 encoder
// ---------------------------------------------------------------------------

function encodeCommand(...args: string[]): string {
  let out = `*${args.length}${CRLF}`;
  for (const arg of args) {
    out += `$${Buffer.byteLength(arg)}${CRLF}${arg}${CRLF}`;
  }
  return out;
}

// ---------------------------------------------------------------------------
// RESP2 decoder
// ---------------------------------------------------------------------------

type RespValue = string | number | null | RespValue[];

function parseResp(data: string): { value: RespValue; remaining: string } {
  const type = data[0];
  const idx = data.indexOf(CRLF);
  if (idx === -1) throw new Error('Incomplete RESP response');
  const line = data.slice(1, idx);
  const rest = data.slice(idx + 2);

  switch (type) {
    case '+': // Simple string
      return { value: line, remaining: rest };

    case '-': // Error
      throw new Error(`Redis error: ${line}`);

    case ':': // Integer
      return { value: parseInt(line, 10), remaining: rest };

    case '$': { // Bulk string
      const len = parseInt(line, 10);
      if (len === -1) return { value: null, remaining: rest };
      const value = rest.slice(0, len);
      return { value, remaining: rest.slice(len + 2) };
    }

    case '*': { // Array
      const count = parseInt(line, 10);
      if (count === -1) return { value: null, remaining: rest };
      const arr: RespValue[] = [];
      let remaining = rest;
      for (let i = 0; i < count; i++) {
        const parsed = parseResp(remaining);
        arr.push(parsed.value);
        remaining = parsed.remaining;
      }
      return { value: arr, remaining };
    }

    default:
      throw new Error(`Unknown RESP type: ${type}`);
  }
}

// ---------------------------------------------------------------------------
// TCP transport — send command, read full response
// ---------------------------------------------------------------------------

function sendCommand(host: string, port: number, command: string): Promise<RespValue> {
  return new Promise((resolve, reject) => {
    const socket = new net.Socket();
    let buffer = '';
    let resolved = false;

    const done = (err?: Error) => {
      if (resolved) return;
      resolved = true;
      socket.destroy();
      if (err) reject(err);
    };

    socket.setTimeout(5000);
    socket.on('timeout', () => done(new Error('Redis command timed out')));
    socket.on('error', (err) => done(err));

    socket.on('data', (chunk) => {
      buffer += chunk.toString();
      try {
        const { value } = parseResp(buffer);
        resolved = true;
        socket.destroy();
        resolve(value);
      } catch {
        // Incomplete response — wait for more data
      }
    });

    socket.connect(port, host, () => {
      socket.write(command);
    });
  });
}

// ---------------------------------------------------------------------------
// RedisClient
// ---------------------------------------------------------------------------

export class RedisClient {
  private host: string;
  private port: number;

  constructor(redisUrl: string) {
    // Accept "host:port" or "redis://host:port"
    const clean = redisUrl.replace(/^redis:\/\//, '');
    const parts = clean.split(':');
    this.host = parts[0] || 'redis';
    this.port = parseInt(parts[1] || '6379', 10);
  }

  async ping(): Promise<boolean> {
    try {
      const reply = await sendCommand(this.host, this.port, encodeCommand('PING'));
      return reply === 'PONG';
    } catch {
      return false;
    }
  }

  async set(key: string, value: string, ttlSeconds: number): Promise<void> {
    await sendCommand(
      this.host, this.port,
      encodeCommand('SET', key, value, 'EX', String(ttlSeconds))
    );
  }

  async get(key: string): Promise<string | null> {
    const reply = await sendCommand(this.host, this.port, encodeCommand('GET', key));
    return reply as string | null;
  }

  async del(key: string): Promise<void> {
    await sendCommand(this.host, this.port, encodeCommand('DEL', key));
  }

  // scan returns [nextCursor, keys]
  async scan(cursor: number, match: string, count: number): Promise<[number, string[]]> {
    const reply = await sendCommand(
      this.host, this.port,
      encodeCommand('SCAN', String(cursor), 'MATCH', match, 'COUNT', String(count))
    ) as RespValue[];

    const nextCursor = parseInt(reply[0] as string, 10);
    const keys = (reply[1] as RespValue[]).map(k => k as string);
    return [nextCursor, keys];
  }

  async disconnect(): Promise<void> {
    // No persistent connection — nothing to close
  }

  get isReady(): boolean {
    return true; // Connection-per-command — always "ready"
  }
}
