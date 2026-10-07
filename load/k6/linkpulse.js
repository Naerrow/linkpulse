// linkpulse k6 부하 테스트 — 단일 클라이언트(노트북 한 대)에서 프로덕션에 건다.
//
// 앱은 IP·태스크별 인메모리 레이트리밋이 있다(app/internal/httpapi/ratelimit.go: 읽기 300/분·버스트 100).
// 그래서 한 IP로는 "최대 처리량"을 잴 수 없다. 대신 세 가지를 확인한다.
//   1) steady — 한도 안(8 rps, 태스크 2개에 나뉘어 각 약 240/분)에서 리다이렉트 지연·오류율
//   2) burst  — 한도 초과(30 rps)에서 429로 막히고 5xx가 0인가
//   3) 정합성 — 302 응답 수와 DB에 쌓인 클릭 수가 같은가(동시 증가에서 유실 없음)
import http from 'k6/http';
import { check } from 'k6';
import { Counter } from 'k6/metrics';

const BASE = __ENV.BASE_URL || 'https://lpulse.live';
const LINKS = 5; // 쓰기 한도(버스트 10) 안에서 만든다

const redirectsOk = new Counter('redirects_302');
const status429 = new Counter('status_429');
const status5xx = new Counter('status_5xx');

export const options = {
  // 리다이렉트를 따라가면 대상 사이트(example.com)를 부하 테스트하게 된다.
  maxRedirects: 0,
  scenarios: {
    steady: {
      executor: 'constant-arrival-rate',
      exec: 'redirect',
      rate: 8,
      timeUnit: '1s',
      duration: '3m',
      preAllocatedVUs: 10,
      maxVUs: 30,
    },
    // steady가 끝나고 30초 쉰다 — 토큰 버킷(초당 5개 리필)이 다시 차도록.
    burst: {
      executor: 'constant-arrival-rate',
      exec: 'redirect',
      rate: 30,
      timeUnit: '1s',
      duration: '30s',
      startTime: '3m30s',
      preAllocatedVUs: 30,
      maxVUs: 60,
    },
  },
  thresholds: {
    'http_req_duration{scenario:steady}': ['p(95)<500'],
    'http_req_failed{scenario:steady}': ['rate<0.01'],
    'status_429{scenario:steady}': ['count==0'],
    'status_429{scenario:burst}': ['count>0'],
    status_5xx: ['count==0'],
  },
};

export function setup() {
  const codes = [];
  for (let i = 0; i < LINKS; i++) {
    const res = http.post(
      `${BASE}/api/links`,
      JSON.stringify({ url: `https://example.com/k6/${Date.now()}-${i}` }),
      { headers: { 'Content-Type': 'application/json' }, tags: { name: 'POST /api/links' } },
    );
    if (res.status !== 201) {
      throw new Error(`링크 생성 실패: ${res.status} ${res.body}`);
    }
    codes.push(res.json('code'));
  }
  return { codes };
}

export function redirect(data) {
  const code = data.codes[Math.floor(Math.random() * data.codes.length)];
  const res = http.get(`${BASE}/${code}`, { tags: { name: 'GET /{code}' } });
  if (res.status === 302) redirectsOk.add(1);
  if (res.status === 429) status429.add(1);
  if (res.status >= 500) status5xx.add(1);
  check(res, { '302 또는 429': (r) => r.status === 302 || r.status === 429 });
}

// DB에 쌓인 클릭 수 합계를 찍는다. 요약의 redirects_302 count와 같아야 한다.
export function teardown(data) {
  let total = 0;
  for (const code of data.codes) {
    const res = http.get(`${BASE}/api/links/${code}`, { tags: { name: 'GET /api/links/{code}' } });
    total += res.status === 200 ? res.json('clicks') : NaN;
  }
  console.log(`DB 클릭 합계 = ${total} (요약의 redirects_302 count와 같아야 한다)`);
}
