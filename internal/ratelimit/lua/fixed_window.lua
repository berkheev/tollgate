-- KEYS[1]: window hash {s = window_start_ms, c = count}
-- ARGV: now_ms, limit, window_ms, cost
local now = tonumber(ARGV[1])
local limit = tonumber(ARGV[2])
local window = tonumber(ARGV[3])
local cost = tonumber(ARGV[4])

local state = redis.call('HMGET', KEYS[1], 's', 'c')
local start = tonumber(state[1]) or 0
local count = tonumber(state[2]) or 0

local current = now - now % window
if current > start then
  start = current
  count = 0
end

local reset = start + window - now
local allowed = 0
local retry = 0
if count + cost <= limit then
  count = count + cost
  allowed = 1
  redis.call('HSET', KEYS[1], 's', start, 'c', count)
  redis.call('PEXPIRE', KEYS[1], reset)
else
  retry = reset
end

return {allowed, limit - count, reset, retry}
