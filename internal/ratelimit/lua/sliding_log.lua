-- KEYS[1]: sorted set, one member per consumed unit, scored by timestamp
-- ARGV: now_ms, limit, window_ms, cost, nonce
local now = tonumber(ARGV[1])
local limit = tonumber(ARGV[2])
local window = tonumber(ARGV[3])
local cost = tonumber(ARGV[4])
local nonce = ARGV[5]

redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', now - window)
local count = redis.call('ZCARD', KEYS[1])

local allowed = 0
local retry = 0
if count + cost <= limit then
  local args = {}
  for i = 1, cost do
    args[#args + 1] = now
    args[#args + 1] = nonce .. ':' .. i
    if #args >= 1000 or i == cost then
      redis.call('ZADD', KEYS[1], unpack(args))
      args = {}
    end
  end
  count = count + cost
  allowed = 1
else
  local idx = count + cost - limit - 1
  local oldest = redis.call('ZRANGE', KEYS[1], idx, idx, 'WITHSCORES')
  retry = tonumber(oldest[2]) + window - now
end

local reset = 0
if count > 0 then
  local newest = redis.call('ZRANGE', KEYS[1], -1, -1, 'WITHSCORES')
  reset = tonumber(newest[2]) + window - now
end

if allowed == 1 then
  redis.call('PEXPIRE', KEYS[1], math.max(reset, 1))
end

return {allowed, limit - count, reset, retry}
