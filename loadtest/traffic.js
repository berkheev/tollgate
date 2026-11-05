// Simulates a mix of applications asking tollgate for decisions: a skewed
// population of API users (a few heavy hitters get limited), search traffic
// per IP, a credential-stuffing bot hitting login, report exports with
// variable cost and bursty webhook fan-out per tenant.
import http from 'k6/http';
import { check } from 'k6';
import { Counter } from 'k6/metrics';

const targets = (__ENV.TARGETS || 'http://localhost:8090').split(',');
const steady = __ENV.DURATION || '3m';

const allowed = new Counter('decisions_allowed');
const denied = new Counter('decisions_denied');

http.setResponseCallback(http.expectedStatuses(200, 429));

export const options = {
  discardResponseBodies: false,
  scenarios: {
    api: {
      executor: 'ramping-arrival-rate',
      exec: 'api',
      startRate: 50,
      timeUnit: '1s',
      preAllocatedVUs: 50,
      maxVUs: 300,
      stages: [
        { target: 250, duration: '30s' },
        { target: 250, duration: steady },
        { target: 600, duration: '20s' },
        { target: 250, duration: '40s' },
      ],
    },
    search: constantRate('search', 60),
    login: constantRate('login', 12),
    exports: constantRate('reportExports', 3),
    webhooks: constantRate('webhooks', 140),
  },
  thresholds: {
    http_req_failed: ['rate<0.01'],
    'http_req_duration{expected_response:true}': ['p(95)<25', 'p(99)<50'],
    checks: ['rate>0.99'],
  },
};

function constantRate(exec, rate) {
  return {
    executor: 'constant-arrival-rate',
    exec,
    rate,
    timeUnit: '1s',
    duration: totalDuration(),
    preAllocatedVUs: 20,
    maxVUs: 100,
  };
}

function totalDuration() {
  const m = /^(\d+)([smh])$/.exec(steady);
  const seconds = m ? Number(m[1]) * { s: 1, m: 60, h: 3600 }[m[2]] : 180;
  return `${seconds + 90}s`;
}

// Zipf-like pick: low indexes are drawn far more often than high ones.
function skewed(n, power = 3) {
  return Math.floor(n * Math.pow(Math.random(), power));
}

function randomIP(pool) {
  const i = skewed(pool, 2);
  return `198.51.${(i >> 8) & 255}.${i & 255}`;
}

function decide(rule, key, cost = 1) {
  const url = `${targets[Math.floor(Math.random() * targets.length)]}/v1/check`;
  const res = http.post(url, JSON.stringify({ rule, key, cost }), {
    headers: { 'Content-Type': 'application/json' },
    tags: { rule },
  });
  check(res, {
    'status is 200 or 429': (r) => r.status === 200 || r.status === 429,
    'has RateLimit-Remaining': (r) => r.headers['Ratelimit-Remaining'] !== undefined,
  });
  if (res.status === 200) {
    allowed.add(1, { rule });
  } else if (res.status === 429) {
    denied.add(1, { rule });
  }
}

export function api() {
  decide('api_per_user', `user-${skewed(400)}`);
}

export function search() {
  decide('search_per_ip', randomIP(300));
}

export function login() {
  // A third of the attempts come from a handful of IPs trying passwords.
  const ip = Math.random() < 0.33 ? `203.0.113.${skewed(4, 1)}` : randomIP(5000);
  decide('login_attempts', ip);
}

export function reportExports() {
  decide('exports_per_account', `account-${skewed(60, 2)}`, 1 + Math.floor(Math.random() * 3));
}

export function webhooks() {
  decide('webhooks_per_tenant', `tenant-${skewed(6, 2)}`);
}
