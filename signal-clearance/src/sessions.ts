// ---------------------------------------------------------------------------
// Session management — refresh tokens backed by Redis.
// Uses the TCA stdlib Redis client (redis.ts) — zero external dependencies.
// ---------------------------------------------------------------------------

import { RedisClient } from './redis.js';

const REFRESH_TTL_SECONDS = 7 * 24 * 60 * 60; // 7 days
const REDIS_URL = process.env.REDIS_URL || 'redis:6379';
const REFRESH_PREFIX = 'seti:refresh:';

let redisClient: RedisClient;
let connected = false;

export async function connectRedis(): Promise<void> {
  redisClient = new RedisClient(REDIS_URL);

  for (let i = 0; i < 10; i++) {
    try {
      const ok = await redisClient.ping();
      if (ok) {
        connected = true;
        console.log('[signal-clearance] Connected to Redis');
        return;
      }
    } catch (err) {
      // not ready yet
    }
    console.log(`[signal-clearance] Redis not ready (attempt ${i + 1}/10)`);
    await new Promise(r => setTimeout(r, 2000));
  }
  throw new Error('Could not connect to Redis after 10 attempts');
}

export async function issueRefreshToken(wranglerId: string): Promise<{
  token: string;
  expires_at: string;
}> {
  const token = crypto.randomUUID();
  const key = `${REFRESH_PREFIX}${token}`;
  const expiresAt = new Date(Date.now() + REFRESH_TTL_SECONDS * 1000).toISOString();

  await redisClient.set(key, JSON.stringify({
    wrangler_id: wranglerId,
    issued_at: new Date().toISOString(),
  }), REFRESH_TTL_SECONDS);

  return { token, expires_at: expiresAt };
}

export async function consumeRefreshToken(token: string): Promise<string | null> {
  const key = `${REFRESH_PREFIX}${token}`;
  const value = await redisClient.get(key);
  if (!value) return null;

  // Single-use — delete immediately after read
  await redisClient.del(key);

  const data = JSON.parse(value);
  return data.wrangler_id;
}

export async function revokeAllSessions(wranglerId: string): Promise<number> {
  let count = 0;
  let cursor = 0;

  do {
    const [nextCursor, keys] = await redisClient.scan(cursor, `${REFRESH_PREFIX}*`, 100);
    cursor = nextCursor;

    for (const key of keys) {
      const value = await redisClient.get(key);
      if (value) {
        const data = JSON.parse(value);
        if (data.wrangler_id === wranglerId) {
          await redisClient.del(key);
          count++;
        }
      }
    }
  } while (cursor !== 0);

  return count;
}

export async function getActiveSessionCount(): Promise<number> {
  let count = 0;
  let cursor = 0;
  do {
    const [nextCursor, keys] = await redisClient.scan(cursor, `${REFRESH_PREFIX}*`, 100);
    cursor = nextCursor;
    count += keys.length;
  } while (cursor !== 0);
  return count;
}

export function isRedisConnected(): boolean {
  return connected;
}

export async function disconnectRedis(): Promise<void> {
  await redisClient?.disconnect();
  connected = false;
}
