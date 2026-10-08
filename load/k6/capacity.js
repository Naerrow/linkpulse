// linkpulse 한계 측정(plan 0012) — 단계 부하로 처리 한계와 첫 병목을 잰다.
//
// 측정 클라이언트 IP만 레이트리밋 예외로 둔 상태에서 실행한다(RATE_LIMIT_EXEMPT_IPS, README 참고).
// 시나리오는 SCENARIO env로 하나씩 따로 실행한다.
//   spread — 링크 100개에 고르게 GET /{code}            : 실제에 가까운 상한
//   hot    — 링크 1개에 전부 GET /{code}                 : 인기 링크의 행 잠금 경합
//   read   — 링크 100개에 GET /api/links/{code}(쓰기 없음) : 대조군. 쓰기를 뺀 상한
//
// 단계: 50 → 100 → 200 → 400 → 800 → 1600 rps, 단계마다 30초 올리고 2분 30초 유지(= 3분).
// setup이 끝나면 다음 정각(:00초)까지 기다렸다가 시작한다. 그래서 단계마다 유지 구간이
// CloudWatch의 온전한 1분 두 개를 덮는다(collect-metrics.sh가 이 두 분만 단계 값으로 쓴다).
import http from 'k6/http';
import { sleep } from 'k6';
import { Counter } from 'k6/metrics';

const BASE = __ENV.BASE_URL || 'https://lpulse.live';
const SCENARIO = __ENV.SCENARIO;
// 단계를 이 값 이하로 자른다(로컬 검증은 100). collect-metrics.sh에도 같은 값을 넘긴다.
const MAX_RPS = Number(__ENV.MAX_RPS || 1600);
// 로컬 전용. 로컬에는 ALB가 없어 XFF 맨 오른쪽 값을 클라이언트가 정한다.
// 운영에서는 ALB가 진짜 IP를 맨 뒤에 붙이므로 이 헤더가 실수로 붙어도 무시된다.
const XFF = __ENV.LOCAL_XFF ? { 'X-Forwarded-For': __ENV.LOCAL_XFF } : {};

// collect-metrics.sh의 단계표와 같아야 한다.
const ALL_RATES = [50, 100, 200, 400, 800, 1600];

if (!['spread', 'hot', 'read'].includes(SCENARIO)) {
  throw new Error('SCENARIO는 spread|hot|read 중 하나여야 한다');
}
const rates = ALL_RATES.filter((r) => r <= MAX_RPS);
if (rates.length === 0) {
  throw new Error(`MAX_RPS=${MAX_RPS} 이하 단계가 없다`);
}

const redirectsOk = new Counter('redirects_302');
const status429 = new Counter('status_429');
const status5xx = new Counter('status_5xx');

export const options = {
  // 리다이렉트를 따라가면 대상 사이트(example.com)를 부하 테스트하게 된다.
  maxRedirects: 0,
  // 1600 rps에서 발생기 메모리·CPU를 아낀다. 본문이 필요한 setup·teardown만 responseType으로 받는다.
  discardResponseBodies: true,
  // 링크 생성 + 정각 맞춤(최대 1분).
  setupTimeout: '3m',
  scenarios: {
    [SCENARIO]: {
      // 열린 모델: 서버가 느려져도 요청률이 유지돼야 한계가 드러난다.
      executor: 'ramping-arrival-rate',
      exec: SCENARIO === 'read' ? 'stats' : 'redirect',
      startRate: 0,
      timeUnit: '1s',
      // 1600 rps에서 평균 지연 약 300ms까지 버틴다. 그보다 느리면 이미 p95 기준에서 불합격이다.
      preAllocatedVUs: 100,
      maxVUs: 500,
      stages: rates.flatMap((r) => [
        { target: r, duration: '30s' },
        { target: r, duration: '2m30s' },
      ]),
    },
  },
  // 안전 정지일 뿐이다. 임계값은 실행 시작부터의 누적값이라 단계 판정은 collect-metrics.sh의 분 단위 자료로 한다.
  thresholds: {
    [`http_req_failed{scenario:${SCENARIO}}`]: [{ threshold: 'rate<0.05', abortOnFail: true, delayAbortEval: '10s' }],
    [`http_req_duration{scenario:${SCENARIO}}`]: [{ threshold: 'p(95)<1000', abortOnFail: true, delayAbortEval: '10s' }],
    // 예외 IP가 안 맞으면 바로 멈춰 원인이 드러나게 한다.
    status_429: [{ threshold: 'count<1', abortOnFail: true }],
  },
};

export function setup() {
  // 링크 생성은 관리자만 한다(plan 0011). 토큰은 실행할 때 env로만 넘긴다 — 파일에 두지 않는다.
  const token = __ENV.ADMIN_TOKEN;
  if (!token) {
    throw new Error('ADMIN_TOKEN이 필요하다(README의 read -rs 참고)');
  }
  const n = SCENARIO === 'hot' ? 1 : 100;
  const codes = [];
  for (let i = 0; i < n; i++) {
    const res = http.post(
      `${BASE}/api/links`,
      JSON.stringify({ url: `https://example.com/k6/${Date.now()}-${i}` }),
      {
        headers: Object.assign({ 'Content-Type': 'application/json', Authorization: `Bearer ${token}` }, XFF),
        responseType: 'text',
        tags: { name: 'POST /api/links' },
      },
    );
    if (res.status !== 201) {
      // 429면 예외 IP가 안 맞은 것이다(운영: -var 값, 로컬: LOCAL_XFF).
      throw new Error(`링크 생성 실패: ${res.status} ${res.body}`);
    }
    codes.push(res.json('code'));
  }

  // 다음 정각까지 기다린다. 시나리오는 setup이 돌아오는 즉시 시작한다.
  const now = Date.now();
  const start = Math.ceil(now / 60000) * 60000;
  sleep((start - now) / 1000);
  console.log(`시나리오 시작 = ${new Date(start).toISOString()} epoch=${start / 1000} — collect-metrics.sh 첫 인자`);
  return { codes };
}

function pick(codes) {
  return codes[Math.floor(Math.random() * codes.length)];
}

function count(res) {
  if (res.status === 302) redirectsOk.add(1);
  if (res.status === 429) status429.add(1);
  if (res.status >= 500) status5xx.add(1);
}

// spread·hot: 리다이렉트 1회 = SELECT 1번 + 같은 행 UPDATE 1번. hot은 링크가 1개라 늘 같은 행이다.
export function redirect(data) {
  count(http.get(`${BASE}/${pick(data.codes)}`, { headers: XFF, tags: { name: 'GET /{code}' } }));
}

// read: SELECT만 한다(클릭을 세지 않는다).
export function stats(data) {
  count(http.get(`${BASE}/api/links/${pick(data.codes)}`, { headers: XFF, tags: { name: 'GET /api/links/{code}' } }));
}

// DB에 쌓인 클릭 합계를 찍는다. 스크립트는 메트릭 값을 읽을 수 없어서 사람이 요약의 redirects_302와 비교한다.
// "302 수 > DB 합계"일 때만 유실이다 — abort로 끝나면 진행 중 요청이 끊겨 DB 합계가 더 클 수 있다. read는 0이 정상이다.
export function teardown(data) {
  let total = 0;
  for (const code of data.codes) {
    const res = http.get(`${BASE}/api/links/${code}`, {
      headers: XFF,
      responseType: 'text',
      tags: { name: 'GET /api/links/{code}' },
    });
    total += res.status === 200 ? res.json('clicks') : NaN;
  }
  console.log(`DB 클릭 합계 = ${total} (요약의 redirects_302 count와 비교한다)`);
}
