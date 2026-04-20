package main

// ---------------------------------------------------------------------------
// TCA Redis Client — stdlib only, zero external dependencies.
//
// Contract: contracts/lib/redis-client.yaml
//
// Implements the full redis-client lib contract against the Redis RESP2
// wire protocol using stdlib net only. No external packages. No reflection.
// No interface{} gymnastics.
//
// Design decisions:
//   - One connection per client. AC's Redis usage is single-goroutine
//     dominant (subscribe loops own their connection, everything else
//     serialises through a mutex-protected single conn). Connection pooling
//     adds complexity with no observable benefit at AC's scale.
//   - Subscriber connections are separate from command connections.
//     Redis does not allow interleaving pub/sub and regular commands on
//     the same connection.
//   - Pipeline batches commands in a single write, reads responses in
//     order. No MULTI/EXEC — pipelining is batching, not transaction.
//   - Reconnect on every command failure. Simple and correct for AC's
//     workload pattern.
// ---------------------------------------------------------------------------

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

// RedisClient is the main client. Safe for concurrent use.
type RedisClient struct {
	addr string
	mu   sync.Mutex
	conn net.Conn
	rw   *bufio.ReadWriter
}

// StreamEntry is a single entry read from a Redis Stream.
type StreamEntry struct {
	ID     string
	Fields map[string]string
}

// Pipeliner batches commands for a single round-trip.
// Not safe for concurrent use — one per goroutine.
type Pipeliner struct {
	client *RedisClient
	cmds   [][]string // each inner slice is one command's args
}

// ExecResult is the result of a single command within a pipeline Exec.
type ExecResult struct {
	Err error
}

// ---------------------------------------------------------------------------
// Client construction
// ---------------------------------------------------------------------------

// NewRedisClient creates a client. Does not connect until first use.
func NewRedisClient(addr string) *RedisClient {
	return &RedisClient{addr: addr}
}

// ---------------------------------------------------------------------------
// Connection management (internal)
// ---------------------------------------------------------------------------

func (c *RedisClient) ensureConn() error {
	if c.conn != nil {
		return nil
	}
	conn, err := net.DialTimeout("tcp", c.addr, 5*time.Second)
	if err != nil {
		return fmt.Errorf("redis connect %s: %w", c.addr, err)
	}
	c.conn = conn
	c.rw = bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn))
	return nil
}

func (c *RedisClient) reset() {
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
		c.rw = nil
	}
}

// do sends a command and reads one reply, holding the mutex.
func (c *RedisClient) do(ctx context.Context, args ...string) (interface{}, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.ensureConn(); err != nil {
		return nil, err
	}

	// Set deadline from context
	if dl, ok := ctx.Deadline(); ok {
		c.conn.SetDeadline(dl)
		defer c.conn.SetDeadline(time.Time{})
	}

	if err := writeCommand(c.rw.Writer, args...); err != nil {
		c.reset()
		return nil, err
	}
	if err := c.rw.Flush(); err != nil {
		c.reset()
		return nil, err
	}

	reply, err := readReply(c.rw.Reader)
	if err != nil {
		c.reset()
		return nil, err
	}
	return reply, nil
}

// ---------------------------------------------------------------------------
// RESP2 writer
// ---------------------------------------------------------------------------

func writeCommand(w *bufio.Writer, args ...string) error {
	_, err := fmt.Fprintf(w, "*%d\r\n", len(args))
	if err != nil {
		return err
	}
	for _, arg := range args {
		_, err = fmt.Fprintf(w, "$%d\r\n%s\r\n", len(arg), arg)
		if err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// RESP2 reader
// ---------------------------------------------------------------------------

func readReply(r *bufio.Reader) (interface{}, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("redis read: %w", err)
	}
	line = strings.TrimRight(line, "\r\n")
	if len(line) == 0 {
		return nil, fmt.Errorf("redis: empty reply")
	}

	switch line[0] {
	case '+': // Simple string
		return line[1:], nil

	case '-': // Error
		return nil, fmt.Errorf("redis error: %s", line[1:])

	case ':': // Integer
		n, err := strconv.ParseInt(line[1:], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("redis int parse: %w", err)
		}
		return n, nil

	case '$': // Bulk string
		n, err := strconv.ParseInt(line[1:], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("redis bulk length: %w", err)
		}
		if n == -1 {
			return nil, nil // Redis Nil
		}
		buf := make([]byte, n+2) // +2 for \r\n
		if _, err := readFull(r, buf); err != nil {
			return nil, fmt.Errorf("redis bulk read: %w", err)
		}
		return string(buf[:n]), nil

	case '*': // Array
		n, err := strconv.ParseInt(line[1:], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("redis array length: %w", err)
		}
		if n == -1 {
			return nil, nil // Nil array
		}
		arr := make([]interface{}, n)
		for i := int64(0); i < n; i++ {
			arr[i], err = readReply(r)
			if err != nil {
				return nil, err
			}
		}
		return arr, nil

	default:
		return nil, fmt.Errorf("redis: unknown reply type %q", line[0])
	}
}

func readFull(r *bufio.Reader, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := r.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// ---------------------------------------------------------------------------
// Helper converters
// ---------------------------------------------------------------------------

func toString(v interface{}) (string, bool) {
	if v == nil {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func toInt64(v interface{}) int64 {
	if n, ok := v.(int64); ok {
		return n
	}
	return 0
}

func toArray(v interface{}) []interface{} {
	if v == nil {
		return nil
	}
	arr, _ := v.([]interface{})
	return arr
}

// isNilErr returns true if err is a Redis Nil reply (key not found).
// In our implementation, Nil bulk strings return (nil, nil) — callers
// check the bool from Get rather than an error.
func isNilReply(v interface{}) bool {
	return v == nil
}

// ---------------------------------------------------------------------------
// Key operations
// ---------------------------------------------------------------------------

// Ping verifies Redis is reachable.
func (c *RedisClient) Ping(ctx context.Context) error {
	v, err := c.do(ctx, "PING")
	if err != nil {
		return err
	}
	s, _ := toString(v)
	if s != "PONG" {
		return fmt.Errorf("redis: unexpected PING response %q", s)
	}
	return nil
}

// Get returns the value for key. bool is false if key does not exist.
func (c *RedisClient) Get(ctx context.Context, key string) (string, bool, error) {
	v, err := c.do(ctx, "GET", key)
	if err != nil {
		return "", false, err
	}
	if isNilReply(v) {
		return "", false, nil
	}
	s, ok := toString(v)
	if !ok {
		return "", false, fmt.Errorf("redis GET: unexpected reply type")
	}
	return s, true, nil
}

// Set sets a key. ttl of 0 means no expiry.
func (c *RedisClient) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	var args []string
	if ttl > 0 {
		ms := ttl.Milliseconds()
		args = []string{"SET", key, value, "PX", strconv.FormatInt(ms, 10)}
	} else {
		args = []string{"SET", key, value}
	}
	_, err := c.do(ctx, args...)
	return err
}

// Expire sets a TTL on an existing key.
func (c *RedisClient) Expire(ctx context.Context, key string, ttl time.Duration) error {
	ms := ttl.Milliseconds()
	_, err := c.do(ctx, "PEXPIRE", key, strconv.FormatInt(ms, 10))
	return err
}

// Del deletes one or more keys.
func (c *RedisClient) Del(ctx context.Context, keys ...string) error {
	args := append([]string{"DEL"}, keys...)
	_, err := c.do(ctx, args...)
	return err
}

// Incr atomically increments a key by 1 and returns the new value.
func (c *RedisClient) Incr(ctx context.Context, key string) (int64, error) {
	v, err := c.do(ctx, "INCR", key)
	if err != nil {
		return 0, err
	}
	return toInt64(v), nil
}

// Keys returns all keys matching a glob-style pattern.
func (c *RedisClient) Keys(ctx context.Context, pattern string) ([]string, error) {
	v, err := c.do(ctx, "KEYS", pattern)
	if err != nil {
		return nil, err
	}
	arr := toArray(v)
	result := make([]string, 0, len(arr))
	for _, item := range arr {
		if s, ok := toString(item); ok {
			result = append(result, s)
		}
	}
	return result, nil
}

// ---------------------------------------------------------------------------
// Pub/Sub
// ---------------------------------------------------------------------------

// Publish publishes a message to a channel.
func (c *RedisClient) Publish(ctx context.Context, channel, message string) error {
	_, err := c.do(ctx, "PUBLISH", channel, message)
	return err
}

// Subscribe subscribes to a channel on a dedicated connection.
// Returns a channel that receives messages. Closed when ctx is cancelled
// or the connection is lost.
func (c *RedisClient) Subscribe(ctx context.Context, channel string) (<-chan string, error) {
	conn, err := net.DialTimeout("tcp", c.addr, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("redis subscribe connect: %w", err)
	}

	rw := bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn))

	if err := writeCommand(rw.Writer, "SUBSCRIBE", channel); err != nil {
		conn.Close()
		return nil, err
	}
	if err := rw.Flush(); err != nil {
		conn.Close()
		return nil, err
	}

	// Read the SUBSCRIBE confirmation
	if _, err := readReply(rw.Reader); err != nil {
		conn.Close()
		return nil, fmt.Errorf("redis subscribe confirm: %w", err)
	}

	ch := make(chan string, 32)

	go func() {
		defer close(ch)
		defer conn.Close()
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
			reply, err := readReply(rw.Reader)
			if err != nil {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					continue // deadline — check ctx and retry
				}
				return // real error — close channel
			}

			arr := toArray(reply)
			if len(arr) == 3 {
				kind, _ := toString(arr[0])
				if kind == "message" {
					if msg, ok := toString(arr[2]); ok {
						select {
						case ch <- msg:
						case <-ctx.Done():
							return
						}
					}
				}
			}
		}
	}()

	return ch, nil
}

// ---------------------------------------------------------------------------
// Streams
// ---------------------------------------------------------------------------

// XAdd appends an entry to a Redis Stream.
func (c *RedisClient) XAdd(ctx context.Context, stream string, maxLen int64, fields map[string]string) (string, error) {
	args := []string{"XADD", stream}
	if maxLen > 0 {
		args = append(args, "MAXLEN", "~", strconv.FormatInt(maxLen, 10))
	}
	args = append(args, "*") // auto-generate ID
	for k, v := range fields {
		args = append(args, k, v)
	}
	reply, err := c.do(ctx, args...)
	if err != nil {
		return "", err
	}
	id, _ := toString(reply)
	return id, nil
}

// XRange returns entries from start to stop (inclusive).
func (c *RedisClient) XRange(ctx context.Context, stream, start, stop string) ([]StreamEntry, error) {
	reply, err := c.do(ctx, "XRANGE", stream, start, stop)
	if err != nil {
		return nil, err
	}
	return parseStreamEntries(reply), nil
}

// XRevRangeN returns up to count entries in reverse order.
func (c *RedisClient) XRevRangeN(ctx context.Context, stream, start, stop string, count int64) ([]StreamEntry, error) {
	reply, err := c.do(ctx, "XREVRANGE", stream, start, stop, "COUNT", strconv.FormatInt(count, 10))
	if err != nil {
		return nil, err
	}
	return parseStreamEntries(reply), nil
}

// XLen returns the number of entries in a stream.
func (c *RedisClient) XLen(ctx context.Context, stream string) (int64, error) {
	reply, err := c.do(ctx, "XLEN", stream)
	if err != nil {
		return 0, err
	}
	return toInt64(reply), nil
}

// XTrim trims a stream to maxLen entries using exact trimming.
func (c *RedisClient) XTrim(ctx context.Context, stream string, maxLen int64) error {
	_, err := c.do(ctx, "XTRIM", stream, "MAXLEN", strconv.FormatInt(maxLen, 10))
	return err
}

// parseStreamEntries converts a raw RESP array reply into []StreamEntry.
func parseStreamEntries(reply interface{}) []StreamEntry {
	arr := toArray(reply)
	if len(arr) == 0 {
		return []StreamEntry{}
	}
	entries := make([]StreamEntry, 0, len(arr))
	for _, raw := range arr {
		entry := toArray(raw)
		if len(entry) != 2 {
			continue
		}
		id, ok := toString(entry[0])
		if !ok {
			continue
		}
		fieldArr := toArray(entry[1])
		fields := make(map[string]string, len(fieldArr)/2)
		for i := 0; i+1 < len(fieldArr); i += 2 {
			k, ok1 := toString(fieldArr[i])
			v, ok2 := toString(fieldArr[i+1])
			if ok1 && ok2 {
				fields[k] = v
			}
		}
		entries = append(entries, StreamEntry{ID: id, Fields: fields})
	}
	return entries
}

// ---------------------------------------------------------------------------
// Pipeline
// ---------------------------------------------------------------------------

// NewPipeline returns a Pipeliner for batching commands.
func (c *RedisClient) NewPipeline() *Pipeliner {
	return &Pipeliner{client: c}
}

// Set queues a SET command.
func (p *Pipeliner) Set(key, value string, ttl time.Duration) *Pipeliner {
	if ttl > 0 {
		ms := ttl.Milliseconds()
		p.cmds = append(p.cmds, []string{"SET", key, value, "PX", strconv.FormatInt(ms, 10)})
	} else {
		p.cmds = append(p.cmds, []string{"SET", key, value})
	}
	return p
}

// Del queues a DEL command.
func (p *Pipeliner) Del(keys ...string) *Pipeliner {
	p.cmds = append(p.cmds, append([]string{"DEL"}, keys...))
	return p
}

// Publish queues a PUBLISH command.
func (p *Pipeliner) Publish(channel, message string) *Pipeliner {
	p.cmds = append(p.cmds, []string{"PUBLISH", channel, message})
	return p
}

// XAdd queues an XADD command.
func (p *Pipeliner) XAdd(stream string, maxLen int64, fields map[string]string) *Pipeliner {
	args := []string{"XADD", stream}
	if maxLen > 0 {
		args = append(args, "MAXLEN", "~", strconv.FormatInt(maxLen, 10))
	}
	args = append(args, "*")
	for k, v := range fields {
		args = append(args, k, v)
	}
	p.cmds = append(p.cmds, args)
	return p
}

// Exec sends all queued commands in a single round-trip.
func (p *Pipeliner) Exec(ctx context.Context) ([]ExecResult, error) {
	if len(p.cmds) == 0 {
		return nil, nil
	}

	c := p.client
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.ensureConn(); err != nil {
		return nil, err
	}

	if dl, ok := ctx.Deadline(); ok {
		c.conn.SetDeadline(dl)
		defer c.conn.SetDeadline(time.Time{})
	}

	// Write all commands in one pass
	for _, cmd := range p.cmds {
		if err := writeCommand(c.rw.Writer, cmd...); err != nil {
			c.reset()
			return nil, err
		}
	}
	if err := c.rw.Flush(); err != nil {
		c.reset()
		return nil, err
	}

	// Read one reply per command
	results := make([]ExecResult, len(p.cmds))
	for i := range p.cmds {
		_, err := readReply(c.rw.Reader)
		if err != nil {
			// Mark this and all remaining as failed, reset conn
			for j := i; j < len(p.cmds); j++ {
				results[j] = ExecResult{Err: err}
			}
			c.reset()
			break
		}
		results[i] = ExecResult{}
	}

	// Reset pipeline for reuse
	p.cmds = p.cmds[:0]

	return results, nil
}

// SubscribeMulti subscribes to one or more channels on a dedicated connection.
// Returns a channel that receives message payloads from any subscribed channel.
// Closed when ctx is cancelled or the connection is lost.
// For a single channel, prefer Subscribe.
func (c *RedisClient) SubscribeMulti(ctx context.Context, channels ...string) (<-chan string, error) {
	if len(channels) == 0 {
		return nil, fmt.Errorf("redis subscribe: no channels specified")
	}
	if len(channels) == 1 {
		return c.Subscribe(ctx, channels[0])
	}

	conn, err := net.DialTimeout("tcp", c.addr, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("redis subscribe connect: %w", err)
	}

	rw := bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn))

	args := append([]string{"SUBSCRIBE"}, channels...)
	if err := writeCommand(rw.Writer, args...); err != nil {
		conn.Close()
		return nil, err
	}
	if err := rw.Flush(); err != nil {
		conn.Close()
		return nil, err
	}

	// Read one confirmation reply per channel
	for range channels {
		if _, err := readReply(rw.Reader); err != nil {
			conn.Close()
			return nil, fmt.Errorf("redis subscribe confirm: %w", err)
		}
	}

	ch := make(chan string, 32)

	go func() {
		defer close(ch)
		defer conn.Close()
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
			reply, err := readReply(rw.Reader)
			if err != nil {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					continue
				}
				return
			}

			arr := toArray(reply)
			if len(arr) == 3 {
				kind, _ := toString(arr[0])
				if kind == "message" {
					if msg, ok := toString(arr[2]); ok {
						select {
						case ch <- msg:
						case <-ctx.Done():
							return
						}
					}
				}
			}
		}
	}()

	return ch, nil
}
