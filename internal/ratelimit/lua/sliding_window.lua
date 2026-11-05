-- KEYS[1]: hash {s = window_start_ms, c = current_count, p = previous_count}
-- ARGV: now_ms, limit, window_ms, cost
local now = tonumber(ARGV[1])
local limit = tonumber(ARGV[2])
local window = tonumber(ARGV[3])
local cost = tonumber(ARGV[4])

local state = redis.call('HMGET', KEYS[1], 's', 'c', 'p')
local start = tonumber(state[1]) or 0
local curr = tonumber(state[2]) or 0
local prev = tonumber(state[3]) or 0

local current = now - now % window
if current <= start then
  -- same window (or the clock went backwards)
elseif current == start + window then
  prev = curr
  curr = 0
  start = current
else
  prev = 0
  curr = 0
  start = current
end

local function ceil_div(a, b)
  return math.floor((a + b - 1) / b)
end

local elapsed = math.min(math.max(now - start, 0), window)
local used = prev * (window - elapsed) + curr * window

local allowed = 0
local retry = 0
if used + cost * window <= limit * window then
  curr = curr + cost
  used = used + cost * window
  allowed = 1
else
  local spare = limit - curr - cost
  if spare >= 0 then
    retry = start + ceil_div(window * (prev - spare), prev) - now
  else
    retry = start + window + ceil_div(window * (curr - (limit - cost)), curr) - now
  end
end

local reset = 0
if curr > 0 then
  reset = start + 2 * window - now
elseif prev > 0 then
  reset = start + window - now
end

if allowed == 1 then
  redis.call('HSET', KEYS[1], 's', start, 'c', curr, 'p', prev)
  redis.call('PEXPIRE', KEYS[1], start + 2 * window - now)
end

return {allowed, math.floor((limit * window - used) / window), reset, retry}
