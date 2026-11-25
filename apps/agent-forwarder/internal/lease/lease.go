package lease

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/tsxuehu/agent-proxy/shared/keys"
)

const ttl = 30 * time.Second

var acquire = redis.NewScript(`
local time = redis.call('TIME')
local now = time[1] * 1000 + math.floor(time[2] / 1000)
for i, key in ipairs(KEYS) do
    redis.call('ZREMRANGEBYSCORE', key, '-inf', now)
    if redis.call('ZCARD', key) >= tonumber(ARGV[i+2]) then return 0 end
end
for _, key in ipairs(KEYS) do
    redis.call('ZADD', key, now + tonumber(ARGV[2]), ARGV[1])
    redis.call('PEXPIRE', key, tonumber(ARGV[2]) * 2)
end
return 1
`)

var renew = redis.NewScript(`
local time = redis.call('TIME')
local now = time[1] * 1000 + math.floor(time[2] / 1000)
for _, key in ipairs(KEYS) do
    local expiry = redis.call('ZSCORE', key, ARGV[1])
    if not expiry or tonumber(expiry) <= now then return 0 end
end
for _, key in ipairs(KEYS) do
    redis.call('ZADD', key, 'XX', now + tonumber(ARGV[2]), ARGV[1])
    redis.call('PEXPIRE', key, tonumber(ARGV[2]) * 2)
end
return 1
`)

type Limit struct {
	Key string
	Max int
}

// Acquire returns a context canceled before any lease that cannot be renewed expires.
func Acquire(ctx context.Context, client *redis.Client, limits ...Limit) (context.Context, func(), error) {
	member := keys.New("")
	names := make([]string, 0, len(limits))
	args := []any{member, ttl.Milliseconds()}
	for _, limit := range limits {
		names = append(names, "agent:lease:"+limit.Key)
		args = append(args, limit.Max)
	}
	ok, err := acquire.Run(ctx, client, names, args...).Int()
	if err != nil || ok != 1 {
		return nil, nil, err
	}
	active, cancel := context.WithCancel(ctx)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-active.Done():
				return
			case <-ticker.C:
				check, stop := context.WithTimeout(active, 4*time.Second)
				ok, err := renew.Run(check, client, names, member, ttl.Milliseconds()).Int()
				stop()
				if err != nil || ok != 1 {
					cancel()
					return
				}
			}
		}
	}()
	return active, func() {
		cancel()
		<-stopped
		release, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		pipeline := client.Pipeline()
		for _, name := range names {
			pipeline.ZRem(release, name, member)
		}
		_, _ = pipeline.Exec(release)
	}, nil
}
