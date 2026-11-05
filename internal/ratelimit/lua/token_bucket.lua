-- KEYS[1]: bucket hash {t = tokens, u = updated_ms}
-- ARGV: now_ms, capacity, limit, window_ms, cost
local now = tonumber(ARGV[1])
local capacity = tonumber(ARGV[2])
local limit = tonumber(ARGV[3])
local window = tonumber(ARGV[4])
local cost = tonumber(ARGV[5])

local state = redis.call('HMGET', KEYS[1], 't', 'u')
local tokens = tonumber(state[1])
local updated = tonumber(state[2])
if tokens == nil or updated == nil then
  tokens = capacity
  updated = now
end

local elapsed = now - updated
if elapsed > 0 then
  tokens = math.min(capacity, tokens + elapsed * limit / window)
end

local allowed = 0
local retry = 0
if tokens >= cost then
  tokens = tokens - cost
  allowed = 1
else
  retry = math.ceil((cost - tokens) * window / limit)
end

local reset = math.ceil((capacity - tokens) * window / limit)
if allowed == 1 then
  redis.call('HSET', KEYS[1], 't', string.format('%.17g', tokens), 'u', math.max(now, updated))
  redis.call('PEXPIRE', KEYS[1], math.max(reset, 1))
end

return {allowed, math.floor(tokens), reset, retry}
