// app.js —— 管理后台的全部前端逻辑（原生 JS，无框架、无构建步骤）。
//
// 认证模型（与 /v1 完全分离）：
//   * 管理面只认**管理密码**：服务每次启动随机生成、只存服务器内存、打印在容器日志里；
//   * 本页面把密码存在 localStorage['wp.adminPassword']，之后每个请求带
//     `Authorization: Bearer <密码>`；没有用户名、没有会话 Cookie（因此也没有 CSRF 面）；
//   * 任何 401（密码错/服务重启换了密码）→ **清掉本地存储并跳回登录页**，
//     绝不留在空白面板上让人误以为"数据没了"。
//
// 其它约定：
//   * 服务端返回的错误原文直接展示（排障就看那句话），这里不"翻译"；
//   * 渲染一律用 textContent 拼节点，不用 innerHTML 拼数据（昵称/描述都是外部输入）；
//   * 不做业务判定：状态分类、能否签到、是否入池全部由服务端字段决定。

'use strict';

const PW_STORE = 'wp.adminPassword';
const THEME_STORE = 'wp.theme';

const state = {
  password: localStorage.getItem(PW_STORE) || '',
  view: '',
  models: [],
  accounts: [],
  authPoll: null, // {state, realm, timer}
  aborter: null,
};

// ── 基础工具 ────────────────────────────────────────────────
const $ = (sel, root = document) => root.querySelector(sel);
const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel));

function el(tag, opts = {}, children = []) {
  const node = document.createElement(tag);
  if (opts.class) node.className = opts.class;
  if (opts.text !== undefined) node.textContent = String(opts.text);
  if (opts.title) node.title = opts.title;
  if (opts.attrs) for (const [k, v] of Object.entries(opts.attrs)) node.setAttribute(k, v);
  for (const c of children) if (c) node.appendChild(c);
  return node;
}

function fmtNum(n) {
  if (n === null || n === undefined || Number.isNaN(Number(n))) return '–';
  return Number(n).toLocaleString();
}

// fmtDuration 把秒渲染成人读的剩余时长（服务端给的是绝对秒数差）。
function fmtDuration(sec) {
  if (!sec || sec <= 0) return '–';
  const d = Math.floor(sec / 86400);
  const h = Math.floor((sec % 86400) / 3600);
  if (d > 0) return d + ' 天 ' + h + ' 小时';
  const m = Math.floor((sec % 3600) / 60);
  if (h > 0) return h + ' 小时 ' + m + ' 分';
  return m + ' 分';
}

function toast(msg, kind = 'secondary') {
  const box = $('#toasts');
  const t = el('div', { class: 'toast align-items-center border-0 text-bg-' + kind, attrs: { role: 'alert' } }, [
    el('div', { class: 'd-flex' }, [
      el('div', { class: 'toast-body', text: msg }),
      el('button', {
        class: 'btn-close btn-close-white me-2 m-auto',
        attrs: { type: 'button', 'data-bs-dismiss': 'toast', 'aria-label': '关闭' },
      }),
    ]),
  ]);
  box.appendChild(t);
  const inst = new bootstrap.Toast(t, { delay: kind === 'danger' ? 8000 : 3500 });
  inst.show();
  t.addEventListener('hidden.bs.toast', () => t.remove());
}

function alertBox(msg, kind = 'warning') {
  $('#alertSlot').replaceChildren(el('div', { class: 'alert alert-' + kind + ' py-2 small', text: msg }));
}

function clearAlert() {
  $('#alertSlot').replaceChildren();
}

// ── 登录态 ──────────────────────────────────────────────────
function showLogin(message) {
  $('#app').hidden = true;
  $('#loginWrap').hidden = false;
  const box = $('#loginError');
  if (message) {
    box.textContent = message;
    box.classList.remove('d-none');
  } else {
    box.classList.add('d-none');
    box.textContent = '';
  }
  const input = $('#loginPassword');
  input.value = '';
  input.focus();
}

function showApp() {
  $('#loginWrap').hidden = true;
  $('#app').hidden = false;
}

// clearSession 清掉本地密码与内存副本（"凭据为空/失效"时统一走这里）。
function clearSession() {
  state.password = '';
  localStorage.removeItem(PW_STORE);
}

// logout 供"退出登录"按钮使用。
function logout() {
  clearSession();
  stopAuthPoll();
  showLogin('已退出登录');
}

// login 用密码换取"可以进后台"的确认；成功后把密码存本地并进入主界面。
async function login(password) {
  const resp = await fetch('/admin/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ password }),
  });
  if (resp.status === 404) {
    throw new Error('管理面未开启：在 config.json 里设 "admin": {"enabled": true} 并重启服务');
  }
  if (resp.status === 429) {
    throw new Error('密码错误次数过多，请 5 分钟后再试');
  }
  if (resp.status === 401) {
    const body = await resp.text();
    throw new Error('密码错误（' + body.slice(0, 200) + '）');
  }
  if (!resp.ok) {
    throw new Error('HTTP ' + resp.status + '：' + (await resp.text()).slice(0, 300));
  }
  state.password = password;
  localStorage.setItem(PW_STORE, password);
  return resp.json();
}

// ── 请求封装 ────────────────────────────────────────────────
async function api(path, opts = {}) {
  const { method = 'GET', body = null } = opts;
  const headers = {};
  if (state.password) headers['Authorization'] = 'Bearer ' + state.password;
  if (body !== null) headers['Content-Type'] = 'application/json';

  const resp = await fetch(path, {
    method,
    headers,
    body: body === null ? null : JSON.stringify(body),
  });

  if (resp.status === 401) {
    // 密码错 / 服务重启换了密码：清本地存储并回登录页（需求：凭据无效时不得留在面板里）。
    clearSession();
    stopAuthPoll();
    showLogin('管理密码已失效或为空（服务重启会更换密码，请用日志里的新密码登录）');
    throw new Error('401 未授权：请重新登录');
  }
  if (resp.status === 503) {
    const text = await resp.text();
    throw new Error('管理密码未初始化（服务启动异常）：' + text.slice(0, 200));
  }
  if (resp.status === 404 && path.startsWith('/admin')) {
    throw new Error('管理面未开启：在 config.json 里设 "admin": {"enabled": true} 并重启服务');
  }
  if (!resp.ok) {
    const err = new Error('HTTP ' + resp.status + '：' + (await resp.text()).slice(0, 400));
    err.status = resp.status; // 调用方按状态码分支（如 410 = 二维码已过期）
    throw err;
  }
  return resp.json();
}

// ── 视图切换（hash 路由，零框架）─────────────────────────────
const VIEWS = ['overview', 'accounts', 'models', 'playground', 'add'];

function currentView() {
  const raw = (location.hash || '').replace(/^#\/?/, '');
  return VIEWS.includes(raw) ? raw : 'overview';
}

function render() {
  if (!state.password) {
    showLogin(null);
    return;
  }
  state.view = currentView();
  for (const v of VIEWS) {
    $('#view-' + v).hidden = v !== state.view;
  }
  $$('.nav-link[data-view]').forEach((b) => {
    b.classList.toggle('active', b.dataset.view === state.view);
  });
  clearAlert();

  switch (state.view) {
    case 'overview': loadStatus(); break;
    case 'accounts': loadAccounts(); break;
    case 'models': loadModels(); break;
    case 'playground': loadModels(true); break;
    case 'add': break;
  }
}

// ── 概览 ────────────────────────────────────────────────────
async function loadStatus() {
  try {
    // 走 /admin/status（管理凭据）；/status 是数据面接口，留给下游用 api_key 调。
    const s = await api('/admin/status');
    $('#kpiTotal').textContent = fmtNum(s.total);
    $('#kpiHealthy').textContent = fmtNum(s.healthy);
    $('#kpiCooling').textContent = fmtNum(s.cooling);
    $('#kpiDisabled').textContent = fmtNum(s.disabled);
    $('#kpiInFlight').textContent = fmtNum(s.in_flight_full);
    $('#kpiSticky').textContent = fmtNum(s.sticky_sessions);
    $('#statusRaw').textContent = JSON.stringify(s, null, 2);
    loadCredits();
  } catch (e) {
    $('#statusRaw').textContent = String(e.message || e);
  }
}

// ── 账号 ────────────────────────────────────────────────────
// ── 积分日账本（概览卡片）──────────────────────────────────────────────
// 数据源 GET /admin/credit-daily：totals/accounts 是**当日内存热值**，
// histories 是落盘的每日汇总（保留 30 天）；全局总量由后端按账号加总。
async function loadCredits() {
  const tbody = $('#creditBody');
  if (!tbody) return;
  try {
    const d = await api('/admin/credit-daily?days=30');
    const t = d.totals || {};
    $('#creditToday').textContent =
      d.today + '：赚 ' + fmtNum(t.earned || 0) + ' / 花 ' + fmtNum(t.spent || 0) +
      ' / 调整 ' + fmtNum(t.adjust || 0) + ' / 净 ' + fmtNum(t.net || 0) +
      '　（账号 ' + Object.keys(d.accounts || {}).length + ' 个）';
    const rows = (d.histories || []).slice().reverse().map((h) => {
      const ht = h.totals || {};
      return el('tr', {}, [
        el('td', { class: 'font-monospace small', text: h.date }),
        el('td', { class: 'text-end text-success', text: fmtNum(ht.earned || 0) }),
        el('td', { class: 'text-end text-danger', text: fmtNum(ht.spent || 0) }),
        el('td', { class: 'text-end text-body-secondary', text: fmtNum(ht.adjust || 0) }),
        el('td', { class: 'text-end fw-semibold', text: fmtNum(ht.net || 0) }),
        el('td', { class: 'text-end text-body-secondary', text: fmtNum((h.accounts || []).length) }),
      ]);
    });
    if (rows.length === 0) {
      rows.push(el('tr', {}, [el('td', {
        class: 'text-center text-body-secondary', attrs: { colspan: '6' },
        text: '还没有记录（今天还没有签到或请求）',
      })]));
    }
    tbody.replaceChildren(...rows);
  } catch (e) {
    $('#creditToday').textContent = String(e.message || e);
  }
}

function statusBadge(a) {
  // 顺序与「能否接单」的判断一致：先看人为动作，再看系统状态，最后看是否入池。
  if (a.manual_disabled) return el('span', { class: 'badge text-bg-secondary status-badge', text: '手动停用', title: a.manual_reason || '' });
  if (a.disabled) return el('span', { class: 'badge text-bg-danger status-badge', text: '系统禁用', title: a.disabled_reason || '' });
  if (a.cooling) return el('span', { class: 'badge text-bg-warning status-badge', text: '冷却 ' + fmtDuration(a.cool_remaining_sec) });
  if (a.resting) return el('span', { class: 'badge text-bg-info status-badge', text: '休养中' });
  if (!a.in_pool) return el('span', { class: 'badge text-bg-secondary status-badge', text: '未入池' });
  return el('span', { class: 'badge text-bg-success status-badge', text: '在线' });
}

function credentialText(a) {
  if (!a.has_credential) return '无 accessToken';
  if (!a.can_refresh) return '仅 accessToken';
  if (a.remaining_sec > 0) {
    const d = Math.floor(a.remaining_sec / 86400);
    return d > 60 ? '长期有效' : fmtDuration(a.remaining_sec) + ' 后过期';
  }
  return '已过期（可刷新）';
}

function actionBtn(label, kind, uid, action, disabled) {
  const b = el('button', {
    class: 'btn btn-sm btn-outline-' + kind + ' me-1',
    text: label,
    attrs: { type: 'button', 'data-uid': uid, 'data-action': action },
  });
  if (disabled) b.disabled = true;
  return b;
}

async function loadAccounts() {
  const body = $('#accountsBody');
  body.replaceChildren(el('tr', {}, [el('td', { class: 'text-center text-body-secondary py-4', attrs: { colspan: '9' }, text: '加载中…' })]));
  try {
    const data = await api('/admin/accounts');
    state.accounts = data.accounts || [];
    const counts = data.counts || {};
    $('#accountsHint').textContent =
      '共 ' + state.accounts.length + ' 个 · 健康 ' + (counts.healthy || 0) +
      ' · 冷却 ' + (counts.cooling || 0) + ' · 禁用 ' + (counts.disabled || 0) +
      ' · 目录 ' + (data.auth_dir || '–');

    if (state.accounts.length === 0) {
      body.replaceChildren(el('tr', {}, [el('td', {
        class: 'text-center text-body-secondary py-4', attrs: { colspan: '9' },
        text: '还没有账号：到「扫码加号」添加，或把 auth 文件放进 auths/ 目录',
      })]));
      return;
    }

    const rows = state.accounts.map((a) => {
      const ops = el('td', { class: 'text-end text-nowrap' }, [
        a.manual_disabled
          ? actionBtn('启用', 'success', a.uid, 'enable')
          : actionBtn('停用', 'secondary', a.uid, 'disable', a.disabled || !a.in_pool),
        a.resting
          ? actionBtn('恢复', 'outline-success', a.uid, 'unrest')
          : actionBtn('休养', 'outline-secondary', a.uid, 'rest', a.disabled || !a.in_pool),

      ]);
      return el('tr', {}, [
        el('td', { class: 'font-monospace small', text: a.uid }),
        el('td', { text: a.nickname || '–' }),
        el('td', { text: a.realm || '–' }),
        el('td', {}, [
          statusBadge(a),
          a.checked_in_today ? el('span', {
            class: 'badge text-bg-success-subtle text-success-emphasis ms-1',
            text: '今日已签',
            title: a.last_checkin_status ? '最近一次：' + a.last_checkin_status : '',
          }) : null,
        ].filter(Boolean)),
        el('td', { class: 'text-end', text: fmtNum(a.credits) }),
        el('td', { class: 'text-end', text: fmtNum(a.in_flight) }),
        el('td', { class: 'small text-body-secondary', text: credentialText(a) }),
        el('td', { class: 'small text-body-secondary', text: (a.proxy || '直连') + (a.weight ? ' / x' + a.weight : '') }),
        ops,
      ]);
    });
    body.replaceChildren(...rows);
  } catch (e) {
    body.replaceChildren(el('tr', {}, [el('td', { class: 'text-center text-danger py-4', attrs: { colspan: '9' }, text: String(e.message || e) })]));
  }
}

async function doAccountAction(uid, action) {
  try {
    if (action === 'rest') {
      await api('/admin/accounts/' + encodeURIComponent(uid) + '/rest', { method: 'POST', body: {} });
      toast(uid + ' 已进入休养：不接下游流量，签到/上报/旅行/保活照跑', 'success');
    } else if (action === 'unrest') {
      await api('/admin/accounts/' + encodeURIComponent(uid) + '/unrest', { method: 'POST', body: {} });
      toast(uid + ' 已恢复接流', 'success');
    } else if (action === 'disable') {
      const reason = prompt('停用原因（只摘对话流量，签到与保活照常）：', '手动停用');
      if (reason === null) return;
      await api('/admin/accounts/' + encodeURIComponent(uid) + '/disable', { method: 'POST', body: { reason } });
      toast(uid + ' 已停用', 'secondary');
    } else if (action === 'enable') {
      await api('/admin/accounts/' + encodeURIComponent(uid) + '/enable', { method: 'POST', body: {} });
      toast(uid + ' 已启用', 'success');
    }
    loadAccounts();
  } catch (e) {
    toast(String(e.message || e), 'danger');
  }
}

async function checkinAll() {
  const btn = $('#btnCheckinAll');
  btn.disabled = true;
  btn.textContent = '签到中…';
  try {
    // 服务端会跳过"今天已签到成功"的账号（状态持久化在 state.json），重复点不会白打上游。
    const r = await api('/admin/accounts/checkin', { method: 'POST', body: {} });
    toast('一键签到完成：成功 ' + (r.ok || 0) + ' / 今日已签跳过 ' + (r.already || 0) +
      ' / 失败 ' + (r.fail || 0) + ' / 其它跳过 ' + (r.skipped || 0), r.fail ? 'warning' : 'success');
    loadAccounts();
  } catch (e) {
    toast(String(e.message || e), 'danger');
  } finally {
    btn.disabled = false;
    btn.textContent = '一键签到';
  }
}

// ── 模型 ────────────────────────────────────────────────────
async function loadModels(fillSelect = false) {
  try {
    const data = await api('/admin/models');
    state.models = data.data || [];
    $('#modelsHint').textContent = '共 ' + (data.count || 0) + ' 个（来源 ' + (data.source || '–') + '）';
    if (fillSelect) fillModelSelect();
    renderModelRows();
  } catch (e) {
    $('#modelsBody').replaceChildren(el('tr', {}, [el('td', { class: 'text-center text-danger py-4', attrs: { colspan: '7' }, text: String(e.message || e) })]));
    $('#modelsHint').textContent = '';
  }
}

function modelCapabilities(m) {
  const parts = [];
  if (m.supports_images) parts.push('图像');
  if (m.supports_tool_call) parts.push('工具');
  if (m.supports_reasoning) parts.push('推理');
  if (m.only_reasoning) parts.push('仅推理');
  return parts.join(' · ') || '–';
}

function renderModelRows() {
  const kw = ($('#modelFilter')?.value || '').trim().toLowerCase();
  const rows = state.models
    .filter((m) => !kw || (m.id || '').toLowerCase().includes(kw) || (m.description || '').toLowerCase().includes(kw))
    .map((m) => el('tr', {}, [
      el('td', { class: 'font-monospace small', text: m.id || '–' }),
      el('td', { class: 'small', text: m.description || '–' }),
      el('td', { class: 'text-end', text: fmtNum(m.context_length) }),
      el('td', { class: 'text-end', text: fmtNum(m.max_output_tokens) }),
      el('td', { class: 'small', text: (m.reasoning_supported_efforts || []).join(' / ') || '–' }),
      el('td', { class: 'small', text: m.credits || '–' }),
      el('td', { class: 'small text-body-secondary', text: modelCapabilities(m) }),
    ]));
  if (rows.length === 0) {
    rows.push(el('tr', {}, [el('td', { class: 'text-center text-body-secondary py-4', attrs: { colspan: '7' }, text: '没有匹配的模型（或尚未从上游拉到清单）' })]));
  }
  $('#modelsBody').replaceChildren(...rows);
}

function fillModelSelect() {
  const sel = $('#pgModel');
  const prev = sel.value;
  sel.replaceChildren(...state.models.map((m) => el('option', { text: m.id, attrs: { value: m.id } })));
  if (prev && state.models.some((m) => m.id === prev)) sel.value = prev;
}

// ── 测试台 ──────────────────────────────────────────────────
async function sendPlayground() {
  const model = $('#pgModel').value;
  const prompt = $('#pgPrompt').value.trim();
  if (!model) return toast('先选一个模型（先到「模型」页刷新清单）', 'warning');
  if (!prompt) return toast('提示词不能为空', 'warning');

  const stream = $('#pgStream').checked;
  const body = { model, messages: [{ role: 'user', content: prompt }], stream };
  const effort = $('#pgEffort').value;
  if (effort) body.reasoning_effort = effort;

  const out = $('#pgOut');
  out.textContent = '';
  $('#pgMeta').textContent = '请求中…';
  const started = performance.now();
  state.aborter = new AbortController();
  $('#btnSend').disabled = true;
  $('#btnAbort').disabled = false;

  try {
    const headers = { 'Content-Type': 'application/json' };
    if (state.password) headers['Authorization'] = 'Bearer ' + state.password;
    const resp = await fetch('/admin/playground/chat', {
      method: 'POST', headers, body: JSON.stringify(body), signal: state.aborter.signal,
    });
    if (resp.status === 401) {
      clearSession();
      showLogin('管理密码已失效（服务重启会更换密码）');
      throw new Error('401 未授权');
    }
    if (!resp.ok) {
      throw new Error('HTTP ' + resp.status + '：' + (await resp.text()).slice(0, 600));
    }

    if (!stream) {
      const data = await resp.json();
      const msg = (data.choices && data.choices[0] && data.choices[0].message) || {};
      const reasoning = msg.reasoning_content ? '【思考】\n' + msg.reasoning_content + '\n\n' : '';
      out.textContent = reasoning + (msg.content || JSON.stringify(data, null, 2));
    } else {
      // 逐行读 SSE：上游事件是 `data: {json}`，帧内含 choices[0].delta.content。
      const reader = resp.body.getReader();
      const decoder = new TextDecoder();
      let buf = '';
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        buf += decoder.decode(value, { stream: true });
        let idx;
        while ((idx = buf.indexOf('\n')) >= 0) {
          const line = buf.slice(0, idx).replace(/\r$/, '');
          buf = buf.slice(idx + 1);
          if (!line.startsWith('data: ')) continue;
          const payload = line.slice(6).trim();
          if (!payload || payload === '[DONE]') continue;
          try {
            const obj = JSON.parse(payload);
            const delta = (obj.choices && obj.choices[0] && obj.choices[0].delta) || {};
            if (delta.reasoning_content) out.textContent += delta.reasoning_content;
            if (delta.content) out.textContent += delta.content;
            if (obj.usage) out.textContent += '\n\n【用量】' + JSON.stringify(obj.usage);
          } catch (_) { /* 上游偶发非 JSON 帧：跳过，不影响已收内容 */ }
        }
      }
      if (!out.textContent) out.textContent = '（流结束但没有正文，可能是该模型在思考或上游返回空流）';
    }
    $('#pgMeta').textContent = '完成，' + Math.round(performance.now() - started) + ' ms';
  } catch (e) {
    if (e.name === 'AbortError') {
      $('#pgMeta').textContent = '已中断';
      out.textContent += '\n\n（请求被中断）';
    } else {
      out.textContent = String(e.message || e);
      $('#pgMeta').textContent = '失败';
    }
  } finally {
    state.aborter = null;
    $('#btnSend').disabled = false;
    $('#btnAbort').disabled = true;
  }
}

// ── 扫码加号 ────────────────────────────────────────────────
// ── 扫码授权的有效期倒计时 ──────────────────────────────────────────────
// 二维码里的 state 有 TTL（config 的 admin.login_ttl，默认 15 分钟）：过期后上游不再
// 接受这次授权，必须重新生成。所以这里把"还剩多久"显式画出来，并在到点时：
//   · 链接加删除线（点了也没意义）
//   · 二维码盖一层"已失效"遮罩
//   · 停止轮询（再问也不会成功）
let authCountdownTimer = null;
let authDeadline = 0;
let authTTLSeconds = 0;

function stopAuthCountdown() {
  if (authCountdownTimer) {
    clearInterval(authCountdownTimer);
    authCountdownTimer = null;
  }
}

function renderAuthCountdown() {
  const el = $('#addCountdown');
  if (!el || !authDeadline) return;
  const leftMs = authDeadline - Date.now();
  if (leftMs <= 0) {
    expireAuth();
    return;
  }
  const total = Math.ceil(leftMs / 1000);
  const mm = String(Math.floor(total / 60)).padStart(2, '0');
  const ss = String(total % 60).padStart(2, '0');
  el.textContent = '有效期剩余 ' + mm + ':' + ss;
  // 最后 60 秒转黄提醒，避免用户扫到一半失效。
  el.className = 'badge flex-shrink-0 ' + (total > 60 ? 'text-bg-secondary' : 'text-bg-warning');
}

function startAuthCountdown(seconds) {
  stopAuthCountdown();
  const el = $('#addCountdown');
  if (!seconds || seconds <= 0) {
    authDeadline = 0;
    if (el) el.textContent = '';
    return;
  }
  authTTLSeconds = seconds;
  authDeadline = Date.now() + seconds * 1000;
  renderAuthCountdown();
  authCountdownTimer = setInterval(renderAuthCountdown, 1000);
}

// expireAuth 把界面切到"已失效"态（倒计时到点或 poll 返回 410 都走这里）。
function expireAuth() {
  stopAuthCountdown();
  stopAuthPoll();
  authDeadline = 0;
  const el = $('#addCountdown');
  if (el) {
    el.textContent = '已失效';
    el.className = 'badge flex-shrink-0 text-bg-danger';
  }
  $('#qrOverlay').classList.remove('d-none');
  $('#qrImg').classList.add('qr-stale');
  $('#addUrl').classList.add('auth-expired');
  const ttlText = authTTLSeconds >= 60
    ? Math.round(authTTLSeconds / 60) + ' 分钟'
    : authTTLSeconds + ' 秒';
  $('#addHint').textContent =
    '二维码已过期（有效期 ' + ttlText + '）。点上方「获取二维码链接」重新生成。';
  $('#addMeta').textContent = '';
}

// resetAuthView 在每次新生成二维码前复位界面（清掉上一次的失效标记）。
function resetAuthView() {
  stopAuthCountdown();
  authDeadline = 0;
  $('#qrOverlay').classList.add('d-none');
  $('#qrImg').classList.remove('qr-stale');
  $('#addUrl').classList.remove('auth-expired');
  const el = $('#addCountdown');
  el.textContent = '有效期 --:--';
  el.className = 'badge flex-shrink-0 text-bg-secondary';
  $('#addHint').textContent = '已开始轮询授权结果，成功后账号会自动落盘并进入账号池。';
}

// renderQR 把文本（auth_url）渲成二维码图片。
//
// 为什么要绕一层 fetch：`<img src="/admin/qr?...">` 无法携带 Authorization 头，
// 而 /admin/qr 和其它管理端点一样要管理凭据。所以先带头发请求，再转成 data URL 挂上。
//
// 为什么用 data URL 而不是 createObjectURL(blob)：页面 CSP 是
// `default-src 'self'; img-src 'self' data:` —— **blob: 不在白名单里**，用它挂图
// 会被浏览器直接拦掉（表现就是"二维码不显示"）。data: 已在白名单内，且二维码只有几 KB。
async function renderQR(text) {
  const img = $('#qrImg');
  if (!text) {
    img.classList.add('d-none');
    return;
  }
  try {
    const resp = await fetch('/admin/qr?size=280&text=' + encodeURIComponent(text), {
      headers: state.password ? { Authorization: 'Bearer ' + state.password } : {},
    });
    if (!resp.ok) throw new Error('HTTP ' + resp.status);
    const blob = await resp.blob();
    const dataURL = await new Promise((resolve, reject) => {
      const fr = new FileReader();
      fr.onload = () => resolve(fr.result);
      fr.onerror = () => reject(fr.error || new Error('读取二维码失败'));
      fr.readAsDataURL(blob);
    });
    img.src = dataURL;
    img.classList.remove('d-none');
  } catch (e) {
    img.classList.add('d-none');
    toast('二维码生成失败：' + String(e.message || e), 'warning');
  }
}

async function startAuth() {
  const realm = $('#addRealm').value;
  const btn = $('#btnAuthStart');
  btn.disabled = true;
  try {
    const r = await api('/admin/auth/start', { method: 'POST', body: { realm } });
    $('#addUrl').textContent = r.auth_url || '';
    $('#addUrl').href = r.auth_url || '#';
    resetAuthView();
    $('#addBox').classList.remove('d-none');
    $('#addMeta').textContent = '等待授权…（' + (r.realm || realm) + '）';
    renderQR(r.auth_url || '');
    startAuthCountdown(r.expires_in || 0);
    pollAuth(r.state, r.realm || realm);
  } catch (e) {
    $('#addMeta').textContent = '';
    toast(String(e.message || e), 'danger');
  } finally {
    btn.disabled = false;
  }
}

function pollAuth(stateToken, realm) {
  stopAuthPoll();
  const timer = setInterval(async () => {
    try {
      const r = await api('/admin/auth/poll?state=' + encodeURIComponent(stateToken) + '&realm=' + encodeURIComponent(realm));
      if (r.status === 'saved') {
        stopAuthPoll();
        stopAuthCountdown();
        const el = $('#addCountdown');
        el.textContent = '已授权';
        el.className = 'badge flex-shrink-0 text-bg-success';
        $('#addHint').textContent = '授权完成：账号已落盘，几秒内自动进入账号池。';
        $('#addMeta').textContent = '已添加号（' + (r.nickname || '') + '，' + r.uid + '）';
        toast('账号已落盘：' + r.uid + '，几秒内进入账号池', 'success');
        loadAccounts();
      }
    } catch (e) {
      // 410 = state 过期/不存在：切成"已失效"态并提示重新获取，而不是笼统报失败。
      if (e.status === 410) {
        expireAuth();
        toast('二维码已失效，请重新获取', 'warning');
        return;
      }
      stopAuthPoll();
      stopAuthCountdown();
      $('#addMeta').textContent = '';
      toast('轮询失败：' + String(e.message || e), 'danger');
    }
  }, 2000);
  state.authPoll = { stateToken, realm, timer };
}

function stopAuthPoll() {
  if (state.authPoll) {
    clearInterval(state.authPoll.timer);
    state.authPoll = null;
  }
}

// ── 主题 ────────────────────────────────────────────────────
function applyTheme(theme) {
  document.documentElement.dataset.bsTheme = theme;
  localStorage.setItem(THEME_STORE, theme);
}

// ── 启动 ────────────────────────────────────────────────────
function bind() {
  // 登录表单
  $('#loginForm').addEventListener('submit', async (ev) => {
    ev.preventDefault();
    const pw = ($('#loginPassword').value || '').trim();
    if (!pw) {
      showLogin('请输入管理密码');
      return;
    }
    const btn = $('#btnLogin');
    btn.disabled = true;
    btn.textContent = '登录中…';
    try {
      await login(pw);
      showApp();
      render();
    } catch (e) {
      clearSession();
      showLogin(String(e.message || e));
    } finally {
      btn.disabled = false;
      btn.textContent = '登录';
    }
  });

  if ($('#btnTravelRun')) {
    $('#btnTravelRun').addEventListener('click', async () => {
      const btn = $('#btnTravelRun');
      btn.disabled = true;
      try {
        await api('/admin/travel/run', { method: 'POST', body: {} });
        toast('已开始一轮猫猫旅行（领养/派出/领奖），稍后刷新看结果', 'success');
      } catch (e) {
        toast(String(e.message || e), 'danger');
      } finally {
        btn.disabled = false;
      }
    });
  }
  if ($('#btnRefreshBalances')) {
    $('#btnRefreshBalances').addEventListener('click', async () => {
      const btn = $('#btnRefreshBalances');
      btn.disabled = true;
      btn.textContent = '刷新中…';
      try {
        const r = await api('/admin/accounts/refresh', { method: 'POST', body: {} });
        toast('余额刷新：成功 ' + (r.ok || 0) + ' / 跳过 ' + (r.skipped || 0) +
          ' / 失败 ' + (r.fail || 0), r.fail ? 'warning' : 'success');
        loadAccounts();
      } catch (e) {
        toast(String(e.message || e), 'danger');
      } finally {
        btn.disabled = false;
        btn.textContent = '刷新余额';
      }
    });
  }
  $('#btnLogout').addEventListener('click', logout);
  if ($('#btnCreditRefresh')) $('#btnCreditRefresh').addEventListener('click', loadCredits);

  // 导航（按钮切换 hash，由 hashchange 驱动渲染）
  $$('.nav-link[data-view]').forEach((b) => {
    b.addEventListener('click', () => {
      location.hash = '#/' + b.dataset.view;
      const nav = $('#nav');
      if (nav.classList.contains('show')) bootstrap.Collapse.getOrCreateInstance(nav).hide();
    });
  });

  const toggleTheme = () => applyTheme(document.documentElement.dataset.bsTheme === 'dark' ? 'light' : 'dark');
  $('#btnTheme').addEventListener('click', toggleTheme);
  $('#btnThemeLogin').addEventListener('click', toggleTheme);

  $('#btnRefreshStatus').addEventListener('click', loadStatus);
  $('#btnRefreshAccounts').addEventListener('click', loadAccounts);
  $('#btnCheckinAll').addEventListener('click', checkinAll);
  $('#btnRefreshModels').addEventListener('click', () => loadModels(true));
  $('#modelFilter').addEventListener('input', renderModelRows);

  // 账号行操作走事件委托（行是动态生成的，逐个绑定会漏）
  $('#accountsBody').addEventListener('click', (ev) => {
    const btn = ev.target.closest('button[data-action]');
    if (!btn) return;
    doAccountAction(btn.dataset.uid, btn.dataset.action);
  });

  $('#btnSend').addEventListener('click', sendPlayground);
  $('#btnAbort').addEventListener('click', () => {
    if (state.aborter) state.aborter.abort();
  });
  $('#btnAuthStart').addEventListener('click', startAuth);

  window.addEventListener('hashchange', render);
}

// boot：先看本地有没有密码。有就先验一次（服务重启后旧密码会失效），
// 验不过就清掉并停在登录页；没有就直接显示登录页。
(async function boot() {
  // 标记：app.js 已执行并接管页面（自动化脚本据此判断可交互，见 scripts/ui-login-check.mjs）。
  document.documentElement.dataset.appBooted = '1';
  applyTheme(localStorage.getItem(THEME_STORE) || (matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light'));
  bind();

  if (!state.password) {
    showLogin(null);
    return;
  }
  try {
    await login(state.password);
    showApp();
    render();
  } catch (e) {
    clearSession();
    showLogin('上次的密码已失效（服务重启会更换密码）：' + String(e.message || e));
  }
})();
