import { createClient } from 'redis';
import { v4 as uuidv4 } from 'uuid';

const REFRESH_TTL_SECONDS = 7 * 24 * 60 * 60; // 7 days
const REDIS_URL = process.env.REDIS_URL || 'redis:6379';
const REFRESH_PREFIX = 'seti:refresh:';

let redisClient: ReturnType<typeof createClient>;

export async function connectRedis(): Promise<void> {
  redisClient = createClient({ url: `redis://${REDIS_URL}` });

  redisClient.on('error', (err) => {
    console.error(`[signal-clearance] Redis error: ${err.message}`);
  });

  for (let i = 0; i < 10; i++) {
    try {
      await redisClient.connect();
      console.log('[signal-clearance] Connected to Redis');
      return;
    } catch (err) {
      console.log(`[signal-clearance] Redis not ready (attempt ${i + 1}/10)`);
      await new Promise(r => setTimeout(r, 2000));
    }
  }
  throw new Error('Could not connect to Redis after 10 attempts');
}

export async function issueRefreshToken(wranglerId: string): Promise<{
  token: string;
  expires_at: string;
}> {
  const token = uuidv4();
  const key = `${REFRESH_PREFIX}${token}`;
  const expiresAt = new Date(Date.now() + REFRESH_TTL_SECONDS * 1000).toISOString();

  await redisClient.setEx(key, REFRESH_TTL_SECONDS, JSON.stringify({
    wrangler_id: wranglerId,
    issued_at: new Date().toISOString(),
  }));

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
  // Scan for all refresh tokens belonging to this wrangler
  // In production this would use a secondary index — acceptable for Phase 1
  let count = 0;
  let cursor = 0;

  do {
    const result = await redisClient.scan(cursor, {
      MATCH: `${REFRESH_PREFIX}*`,
      COUNT: 100,
    });
    cursor = result.cursor;

    for (const key of result.keys) {
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
    const result = await redisClient.scan(cursor, {
      MATCH: `${REFRESH_PREFIX}*`,
      COUNT: 100,
    });
    cursor = result.cursor;
    count += result.keys.length;
  } while (cursor !== 0);
  return count;
}

export function isRedisConnected(): boolean {
  return redisClient?.isReady ?? false;
}
