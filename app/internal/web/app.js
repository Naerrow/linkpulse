// linkpulse 화면 스크립트(plan 0011). 빌드 없이 그대로 서빙된다.
//
// 화면은 주소의 # 뒤로 셋으로 나뉜다.
//   (없음)    방문자 기본 화면 — 링크 요청, 클릭 수 보기
//   #r=<id>   요청 확인 페이지 — 그 요청 하나의 결과. 주소 자체가 열쇠다
//   #admin    운영자 화면 — 방문자에게는 링크를 노출하지 않는다
//
// ⚠️ 보안 규칙: 서버에서 온 문자열(특히 방문자가 쓴 URL·메모)은 textContent/value로만 넣는다.
// innerHTML을 쓰지 않는다 — 운영자 토큰이 localStorage에 있으므로 XSS 한 번이 곧 토큰 탈취다.
'use strict';

const TOKEN_KEY = 'linkpulse.adminToken';

const $ = (id) => document.getElementById(id);

function show(el, on) {
  el.hidden = !on;
}

function setError(id, message) {
  const el = $(id);
  el.textContent = message || '';
  show(el, Boolean(message));
}

// api는 JSON API를 부르고 {ok, status, data}를 돌려준다. 네트워크 오류도 같은 모양으로 감싼다.
async function api(method, path, body, token) {
  const headers = {};
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  if (token) headers.Authorization = 'Bearer ' + token;
  try {
    const res = await fetch(path, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    let data = null;
    try {
      data = await res.json();
    } catch (_) {
      data = null;
    }
    return { ok: res.ok, status: res.status, data };
  } catch (_) {
    return { ok: false, status: 0, data: null };
  }
}

// errorMessage는 서버의 일관된 에러 형식({"error":{"message"}})에서 문구를 꺼낸다.
function errorMessage(res, fallback) {
  if (res.status === 0) return '서버에 연결할 수 없습니다. 잠시 후 다시 시도해 주세요.';
  if (res.status === 429 && !(res.data && res.data.error)) return '요청이 너무 많습니다. 잠시 후 다시 시도해 주세요.';
  return (res.data && res.data.error && res.data.error.message) || fallback;
}

function getToken() {
  try {
    return localStorage.getItem(TOKEN_KEY) || '';
  } catch (_) {
    return '';
  }
}

function setToken(token) {
  try {
    if (token) localStorage.setItem(TOKEN_KEY, token);
    else localStorage.removeItem(TOKEN_KEY);
  } catch (_) {
    // 저장소를 못 쓰는 브라우저(사생활 보호 모드 등)는 이번 페이지에서만 로그인 상태가 유지된다.
  }
}

let sessionToken = getToken();

function formatTime(iso) {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? '' : d.toLocaleString('ko-KR');
}

// ── 복사 ─────────────────────────────────────────────────────
async function copyFrom(input, btn) {
  try {
    await navigator.clipboard.writeText(input.value);
  } catch (_) {
    input.select();
    document.execCommand('copy');
  }
  const label = btn.textContent;
  btn.textContent = '복사됨';
  setTimeout(() => { btn.textContent = label; }, 1500);
}

document.addEventListener('click', (e) => {
  const btn = e.target.closest('[data-copy]');
  if (btn) copyFrom($(btn.dataset.copy), btn);
});

// ── 화면 전환(# 뒤) ──────────────────────────────────────────
function requestIdFromHash() {
  const m = /^#r=([A-Za-z0-9_-]{22})$/.exec(location.hash);
  return m ? m[1] : '';
}

// justSubmitted는 방금 요청해서 확인 페이지로 넘어온 경우에만 true다(안내 문구를 한 번 보여 준다).
let justSubmitted = false;

function route() {
  const id = requestIdFromHash();
  const admin = location.hash === '#admin';
  show($('view-home'), !id && !admin);
  show($('view-status'), Boolean(id));
  show($('view-admin'), admin);
  if (id) loadStatus(id);
  if (admin && sessionToken) {
    setLoggedIn(true);
    loadAdmin();
  }
  window.scrollTo(0, 0);
}

window.addEventListener('hashchange', route);

// ── 요청 확인 페이지 ─────────────────────────────────────────
const STATUS_TEXT = {
  pending: '운영자가 아직 확인하지 않았습니다. 이 페이지 주소를 저장해 두고 나중에 다시 열어 보세요.',
  approved: '단축 링크가 만들어졌습니다. 아래 링크를 복사해서 쓰시면 됩니다.',
  rejected: '운영자가 이 요청을 처리하지 않기로 했습니다.',
};

async function loadStatus(id) {
  show($('status-submitted'), justSubmitted);
  justSubmitted = false;
  $('status-page-url').value = location.href;
  $('status-text').textContent = '불러오는 중…';

  const res = await api('GET', '/api/requests/' + encodeURIComponent(id));
  if (!res.ok) {
    $('status-text').textContent = res.status === 404
      ? '요청을 찾을 수 없습니다. 주소가 정확한지 확인해 주세요.'
      : errorMessage(res, '상태를 불러오지 못했습니다.');
    show($('status-detail'), false);
    show($('status-result'), false);
    return;
  }
  const req = res.data;
  $('status-text').textContent = STATUS_TEXT[req.status] || req.status;
  $('status-url').textContent = req.url;
  $('status-note').textContent = req.note || '—';
  show($('status-detail'), true);
  show($('status-result'), Boolean(req.short_url));
  $('status-short-url').value = req.short_url || '';
}

$('status-refresh').addEventListener('click', () => {
  const id = requestIdFromHash();
  if (id) loadStatus(id);
});

// ── 링크 요청 ────────────────────────────────────────────────
$('request-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  setError('request-error', '');

  const res = await api('POST', '/api/requests', {
    url: $('request-url').value.trim(),
    note: $('request-note').value.trim(),
  });
  if (!res.ok) {
    setError('request-error', errorMessage(res, '요청하지 못했습니다.'));
    return;
  }
  e.target.reset();
  justSubmitted = true;
  location.hash = '#r=' + res.data.id; // hashchange → route()가 확인 페이지를 연다
});

// ── 클릭 수 보기 ─────────────────────────────────────────────
$('stats-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const out = $('stats-result');
  // 단축 링크 전체를 붙여 넣어도 마지막 경로 조각을 코드로 쓴다.
  const raw = $('stats-code').value.trim().replace(/\/+$/, '');
  const code = raw.split('/').pop();
  if (!/^[0-9A-Za-z]+$/.test(code)) {
    out.textContent = '단축 링크 형식이 아닙니다.';
    show(out, true);
    return;
  }
  const res = await api('GET', '/api/links/' + encodeURIComponent(code));
  out.textContent = res.ok
    ? res.data.short_url + ' → 클릭 ' + res.data.clicks.toLocaleString('ko-KR') + '회' +
      (res.data.disabled ? ' (운영자가 중단한 링크)' : '')
    : (res.status === 404 ? '없는 링크입니다.' : errorMessage(res, '조회하지 못했습니다.'));
  show(out, true);
});

// ── 운영자 ───────────────────────────────────────────────────
function setLoggedIn(on) {
  show($('login-form'), !on);
  show($('admin-panel'), on);
}

async function loadPending() {
  setError('pending-error', '');
  const res = await api('GET', '/api/admin/requests', undefined, sessionToken);
  if (res.status === 401) {
    logout('토큰이 맞지 않거나 운영자 기능이 꺼져 있습니다.');
    return false;
  }
  if (!res.ok) {
    setError('pending-error', errorMessage(res, '목록을 불러오지 못했습니다.'));
    return true;
  }
  renderPending(res.data.requests);
  return true;
}

// loadAdmin은 운영자 화면의 두 목록을 함께 불러온다. 토큰이 틀리면 loadPending이 로그아웃시킨다.
async function loadAdmin() {
  if (await loadPending()) await loadLinks();
}

async function loadLinks() {
  setError('links-error', '');
  const res = await api('GET', '/api/admin/links', undefined, sessionToken);
  if (res.status === 401) {
    logout('토큰이 맞지 않습니다. 다시 로그인해 주세요.');
    return;
  }
  if (!res.ok) {
    setError('links-error', errorMessage(res, '링크 목록을 불러오지 못했습니다.'));
    return;
  }
  const list = $('links-list');
  list.replaceChildren();
  const tpl = $('link-item');
  for (const link of res.data.links) {
    const li = tpl.content.firstElementChild.cloneNode(true);
    li.querySelector('.link-short').textContent = link.short_url;
    li.querySelector('.link-url').textContent = '→ ' + link.url;
    renderLinkState(li, link);
    li.querySelector('.toggle').addEventListener('click', () => toggleLink(li, link.code));
    list.append(li);
  }
}

// renderLinkState는 링크 한 줄의 상태 문구와 버튼을 현재 값에 맞춘다.
function renderLinkState(li, link) {
  li.dataset.disabled = link.disabled ? '1' : '';
  li.querySelector('.pending-meta').textContent =
    '클릭 ' + link.clicks.toLocaleString('ko-KR') + '회 · ' + (link.disabled ? '중단됨' : '사용 중');
  li.querySelector('.toggle').textContent = link.disabled ? '다시 사용' : '중단';
}

async function toggleLink(li, code) {
  const btn = li.querySelector('.toggle');
  btn.disabled = true;
  const action = li.dataset.disabled ? 'enable' : 'disable';
  const res = await api('POST', '/api/admin/links/' + encodeURIComponent(code) + '/' + action, undefined, sessionToken);
  btn.disabled = false;
  if (res.status === 401) {
    logout('토큰이 맞지 않습니다. 다시 로그인해 주세요.');
    return;
  }
  if (!res.ok) {
    setError('links-error', errorMessage(res, '바꾸지 못했습니다.'));
    return;
  }
  renderLinkState(li, res.data);
}

function renderPending(requests) {
  const list = $('pending-list');
  list.replaceChildren();
  $('pending-count').textContent = '(' + requests.length + ')';
  show($('pending-empty'), requests.length === 0);
  const tpl = $('pending-item');
  for (const req of requests) {
    const li = tpl.content.firstElementChild.cloneNode(true);
    li.querySelector('.pending-url').textContent = req.url;
    li.querySelector('.pending-note').textContent = req.note ? '메모: ' + req.note : '';
    li.querySelector('.pending-meta').textContent = '요청 시각: ' + formatTime(req.created_at);
    li.querySelector('.approve').addEventListener('click', () => decide(li, req.id, 'approve'));
    li.querySelector('.reject').addEventListener('click', () => decide(li, req.id, 'reject'));
    const copyBtn = li.querySelector('.pending-copy');
    copyBtn.addEventListener('click', () => copyFrom(li.querySelector('.pending-short-url'), copyBtn));
    list.append(li);
  }
}

async function decide(li, id, action) {
  const actions = li.querySelector('.pending-actions');
  for (const b of actions.querySelectorAll('button')) b.disabled = true;
  const res = await api('POST', '/api/admin/requests/' + encodeURIComponent(id) + '/' + action, undefined, sessionToken);
  if (res.status === 401) {
    logout('토큰이 맞지 않습니다. 다시 로그인해 주세요.');
    return;
  }
  const message = li.querySelector('.pending-message');
  if (!res.ok) {
    message.textContent = errorMessage(res, '처리하지 못했습니다.');
    for (const b of actions.querySelectorAll('button')) b.disabled = false;
  } else if (action === 'approve') {
    message.textContent = '승인했습니다. 단축 링크:';
    li.querySelector('.pending-short-url').value = res.data.link.short_url;
    show(li.querySelector('.pending-result'), true);
    actions.remove();
    loadLinks();
  } else {
    message.textContent = '거절했습니다.';
    actions.remove();
  }
  show(message, true);
}

function logout(message) {
  sessionToken = '';
  setToken('');
  setLoggedIn(false);
  setError('login-error', message || '');
}

$('login-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  setError('login-error', '');
  sessionToken = $('admin-token').value.trim();
  e.target.reset();
  if (await loadPending()) {
    setToken(sessionToken);
    setLoggedIn(true);
    loadLinks();
  }
});

$('logout').addEventListener('click', () => logout());
$('pending-refresh').addEventListener('click', loadPending);
$('links-refresh').addEventListener('click', loadLinks);

$('create-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  setError('create-error', '');
  show($('create-done'), false);
  const res = await api('POST', '/api/links', { url: $('create-url').value.trim() }, sessionToken);
  if (res.status === 401) {
    logout('토큰이 맞지 않습니다. 다시 로그인해 주세요.');
    return;
  }
  if (!res.ok) {
    setError('create-error', errorMessage(res, '만들지 못했습니다.'));
    return;
  }
  $('create-short-url').value = res.data.short_url;
  show($('create-done'), true);
  e.target.reset();
  loadLinks();
});

// ── 시작 ─────────────────────────────────────────────────────
route();
