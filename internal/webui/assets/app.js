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
  creditDaily: null, // /admin/credit-daily 最近一次响应（概览与单号曲线共用）
  totalCredits: 0,   // 整池当前积分（/status 各账号之和），概览曲线最后一个点用
  chartUID: '',      // 积分曲线弹窗当前账号
  chartDays: 7,      // 积分曲线区间（7 / 15 / 30，默认 7）
  overviewDays: 7,   // 概览「积分日账本」曲线区间（7 / 15 / 30，默认 7）

  // 设置页
  config: null,          // {values, defaults, schema, meta}
  configDirty: new Map(),// path -> 待保存值
  configErrors: new Map(),// path -> 错误文案
  configFilter: '',
  setupMode: false,      // 首次初始化密码
  restartPending: false, // 有改动等待重启
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
  state.setupMode = false;
  $('#app').hidden = true;
  $('#loginWrap').hidden = false;
  $('#loginForm').hidden = false;
  $('#setupForm').hidden = true;
  $('#loginSubtitle').textContent = '管理后台登录';
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
    throw new Error('管理面未开启：在 data/config.json 里设 "admin": {"enabled": true} 后重启容器');
  }
  if (resp.status === 428) {
    boxSetupError('管理员密码还没初始化：刷新页面，按提示设置密码');
    throw new Error('管理密码尚未初始化');
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
    // 密码错 / 改过密码：清本地存储并回登录页（需求：凭据无效时不得留在面板里）。
    clearSession();
    stopAuthPoll();
    showLogin('管理密码已失效或为空，请重新登录');
    throw new Error('401 未授权：请重新登录');
  }
  if (resp.status === 428) {
    clearSession();
    showSetup(8);
    throw new Error('管理密码尚未初始化：请按页面提示设置密码');
  }
  if (resp.status === 404 && path.startsWith('/admin')) {
    throw new Error('管理面已关闭：在 data/config.json 里把 admin.enabled 改回 true 后重启容器');
  }
  if (!resp.ok) {
    const err = new Error('HTTP ' + resp.status + '：' + (await resp.text()).slice(0, 400));
    err.status = resp.status; // 调用方按状态码分支（如 410 = 二维码已过期）
    throw err;
  }
  return resp.json();
}

// ── 视图切换（hash 路由，零框架）─────────────────────────────
const VIEWS = ['overview', 'accounts', 'models', 'playground', 'add', 'settings'];

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
    case 'settings': loadConfig(); break;
  }
  updateRestartBanner();
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
    // 当前总积分额度 = 池内各账号余额之和（/status 的 accounts[] 与账号页同源）。
    // 概览曲线最后一天（今天）也用这个实时值，而不是等日末快照。
    const sumCredits = (s.accounts || []).reduce((acc, a) => acc + Number(a.credits || 0), 0);
    state.totalCredits = sumCredits;
    $('#kpiCredits').textContent = fmtNum(sumCredits);
    $('#statusRaw').textContent = JSON.stringify(s, null, 2);
    loadCredits();
  } catch (e) {
    $('#statusRaw').textContent = String(e.message || e);
  }
}

// ── 账号 ────────────────────────────────────────────────────
// ── 积分日账本（概览曲线）──────────────────────────────────────────────
// 数据源 GET /admin/credit-daily：totals/accounts 是**当日内存热值**，
// histories 是落盘的每日汇总（保留 30 天）；整池按天汇总在 chartRows 里做。
async function loadCredits() {
  const host = $('#overviewChart');
  if (!host) return;
  try {
    const d = await api('/admin/credit-daily?days=30');
    state.creditDaily = d; // 缓存给「单账号曲线」弹窗复用，避免点开时再等一次
    const t = d.totals || {};
    $('#creditToday').textContent =
      d.today + '：赚 ' + fmtNum(t.earned || 0) + ' / 花 ' + fmtNum(t.spent || 0) +
      ' / 调整 ' + fmtNum(t.adjust || 0) + ' / 净 ' + fmtNum(t.net || 0) +
      '　（账号 ' + Object.keys(d.accounts || {}).length + ' 个）';
    // 当日三项总计卡片（与账本同一份热值，口径不会分叉）。
    $('#kpiEarned').textContent = fmtNum(t.earned || 0);
    $('#kpiSpent').textContent = fmtNum(t.spent || 0);
    $('#kpiAdjust').textContent = fmtNum(t.adjust || 0);
    renderOverviewChart();
  } catch (e) {
    $('#overviewChartHint').textContent = String(e.message || e);
  }
}

// ── 单账号积分曲线（弹窗 + 手写 SVG 折线图）──────────────────────────────
// 数据源：GET /admin/credit-daily?days=30 —— histories[] 已按天给出**每账号**
// earned/spent/adjust/credits_end，所以 7/15/30 天只是同一份数据切片，不需要额外接口。
// 为什么手写 SVG：CSP 是 `default-src 'self'`，引不了任何 CDN 图表库，而这套后台
// 刻意不设构建步骤（见 app.css 头注释），自己画四十行比塞一个库更省事。

const SVG_NS = 'http://www.w3.org/2000/svg';

// 四条固定序列（颜色/名称/读哪个轴）。颜色选 Bootstrap 语义色的实色值：
// 深浅色主题下都够对比度，且不依赖 CSS 变量（SVG 属性拿不到 --bs-* 的解析值）。
const CREDIT_SERIES = [
  { key: 'credits', name: '日终总积分', color: '#0d6efd', axis: 'left' },
  { key: 'earned', name: '当日获得', color: '#198754', axis: 'right' },
  { key: 'spent', name: '当日消耗', color: '#dc3545', axis: 'right' },
  { key: 'adjust', name: '当日调整', color: '#fd7e14', axis: 'right' },
];

function svgNode(tag, attrs = {}) {
  const n = document.createElementNS(SVG_NS, tag);
  for (const [k, v] of Object.entries(attrs)) n.setAttribute(k, String(v));
  return n;
}

// fmtAxis 坐标轴刻度用的紧凑数字（12000 → 1.2万，3400 → 3.4k）。
function fmtAxis(v) {
  const abs = Math.abs(v);
  if (abs >= 10000) return (v / 10000).toFixed(abs >= 100000 ? 0 : 1) + '万';
  if (abs >= 1000) return (v / 1000).toFixed(abs >= 10000 ? 0 : 1) + 'k';
  return String(Math.round(v));
}

// niceMax 把轴上限向上取整到 1/2/5×10^k：刻度才会是 250/500/1250 这种整数，
// 而不是 103/205/308（410 的四个等分）那种看着像随机数的值。
function niceMax(v) {
  if (!(v > 0)) return 1;
  const exp = Math.pow(10, Math.floor(Math.log10(v)));
  const n = v / exp;
  const step = n <= 1 ? 1 : n <= 2 ? 2 : n <= 5 ? 5 : 10;
  return step * exp;
}

// drawLineChart 双 Y 轴折线图：left 轴给量级大的余额，right 轴给当日增减。
// labels 与每个 series.values 等长且已按时间升序。
function drawLineChart(host, labels, series) {
  host.replaceChildren();
  if (!labels.length) {
    host.appendChild(el('div', { class: 'text-body-secondary small py-5 text-center', text: '该区间没有账本数据' }));
    return;
  }

  const W = 900, H = 320;
  const padL = 58, padR = 58, padT = 14, padB = 36;
  const plotW = W - padL - padR;
  const plotH = H - padT - padB;
  const svg = svgNode('svg', { viewBox: '0 0 ' + W + ' ' + H, class: 'credit-chart', role: 'img' });

  const leftVals = series.filter((s) => s.axis === 'left').flatMap((s) => s.values);
  const rightVals = series.filter((s) => s.axis !== 'left').flatMap((s) => s.values);
  const maxOf = (vals) => vals.reduce((m, v) => Math.max(m, Number(v) || 0), 0);
  // 全 0（例如新号、区间内没有任何记录）时给 1，避免除零。
  const maxL = niceMax(maxOf(leftVals));
  const maxR = niceMax(maxOf(rightVals));

  const xAt = (i) => (labels.length === 1 ? padL + plotW / 2 : padL + (plotW * i) / (labels.length - 1));
  const yAt = (v, max) => padT + plotH - (Math.max(0, Number(v) || 0) / max) * plotH;

  // 网格 + 双轴刻度（5 条横线：0 / ¼ / ½ / ¾ / 满）。
  for (let k = 0; k <= 4; k++) {
    const ratio = k / 4;
    const y = padT + plotH * (1 - ratio);
    svg.appendChild(svgNode('line', { class: 'grid', x1: padL, x2: padL + plotW, y1: y, y2: y }));
    const lv = svgNode('text', { class: 'axis-label', x: padL - 8, y: y + 4, 'text-anchor': 'end' });
    lv.textContent = fmtAxis(maxL * ratio);
    svg.appendChild(lv);
    const rv = svgNode('text', { class: 'axis-label', x: padL + plotW + 8, y: y + 4, 'text-anchor': 'start' });
    rv.textContent = fmtAxis(maxR * ratio);
    svg.appendChild(rv);
  }

  // X 轴：最多 8 个日期标签，首尾必显示（其余按步长抽稀，避免 30 天挤成一团）。
  // 靠近末尾的抽稀标签要跳过，否则会和"最后一天"的标签叠在一起（30 天档踩过）。
  const step = Math.max(1, Math.ceil(labels.length / 8));
  const minGap = Math.max(1, Math.round(step / 2));
  labels.forEach((lab, i) => {
    const last = labels.length - 1;
    if (i !== last && (i % step !== 0 || last - i < minGap)) return;
    const t = svgNode('text', { class: 'x-label', x: xAt(i), y: padT + plotH + 20, 'text-anchor': 'middle' });
    t.textContent = String(lab).slice(5); // 2026-11-10 → 11-10
    svg.appendChild(t);
  });

  // 曲线 + 数据点（点带 <title>，鼠标悬停即读数）。点多了缩小半径，避免连成一条白带。
  const dotR = labels.length > 15 ? 1.8 : 2.8;
  for (const s of series) {
    const max = s.axis === 'left' ? maxL : maxR;
    const pts = s.values.map((v, i) => xAt(i) + ',' + yAt(v, max));
    svg.appendChild(svgNode('polyline', {
      class: 'series-line', points: pts.join(' '), stroke: s.color,
    }));
    s.values.forEach((v, i) => {
      const dot = svgNode('circle', {
        class: 'series-dot', cx: xAt(i), cy: yAt(v, max), r: dotR, fill: s.color,
      });
      const tip = svgNode('title', {});
      tip.textContent = labels[i] + ' · ' + s.name + '：' + fmtNum(v);
      dot.appendChild(tip);
      svg.appendChild(dot);
    });
  }

  host.appendChild(svg);
  host.appendChild(el('div', {
    class: 'd-flex justify-content-between small text-body-secondary mt-1',
    // 轴标签只在图内，这里再给一句人话，免得看左轴/右轴搞混。
  }, [
    el('span', { text: '左轴：日终总积分' }),
    el('span', { text: '右轴：当日获得 / 消耗 / 调整' }),
  ]));
}

// dayLabels 生成"今天往前 days 天"的完整日期序列（升序，yyyy-mm-dd）。
// 为什么要自己铺日历：需求是「没有积分的日期按 0 处理」——只画账本里存在的日期
// 会让缺失的那天在图上凭空消失（X 轴不连续，看不出"这天啥都没干"）。
// 用 UTC 口径做日期加减，避免本地时区/DST 把日期算偏。
function dayLabels(todayISO, days) {
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(String(todayISO || ''));
  const base = m ? Date.UTC(Number(m[1]), Number(m[2]) - 1, Number(m[3])) : Date.now();
  const out = [];
  for (let i = days - 1; i >= 0; i--) {
    out.push(new Date(base - i * 86400000).toISOString().slice(0, 10));
  }
  return out;
}

// chartRows 把 credit-daily 响应整理成逐日序列，整池汇总与单账号共用同一套口径。
//
//   - uid == null → 整池：earned/spent/adjust 按天相加，日终总积分 = 当天各账号
//     credits_end 之和；
//   - uid 非空 → 单账号：取该账号当天那条记录。
//
// 缺失日期一律按 0（需求：实际只有 3 天数据却看 7 天时，空的那几天用 0 填充）——
// 四条线都不做沿用/插值，图上的空档就是 0。
// 唯一的例外是**今天**那一点：用实时值（整池=当前总积分，单号=账号当前积分），
// 因为"此刻的余额"不是缺失数据，日末快照可能还没写。
function chartRows(payload, days, uid) {
  const labels = dayLabels(payload && payload.today, days);
  const byDate = new Map(((payload && payload.histories) || []).map((h) => [h.date, h]));
  const rows = labels.map((date) => {
    const h = byDate.get(date);
    const list = h ? (h.accounts || []) : [];
    if (!uid) {
      let earned = 0, spent = 0, adjust = 0, credits = 0;
      for (const rec of list) {
        earned += Number(rec.earned || 0);
        spent += Number(rec.spent || 0);
        adjust += Number(rec.adjust || 0);
        credits += Number(rec.credits_end || 0);
      }
      return { date, earned, spent, adjust, credits };
    }
    const rec = list.find((a) => a.uid === uid);
    return {
      date,
      earned: rec ? Number(rec.earned || 0) : 0,
      spent: rec ? Number(rec.spent || 0) : 0,
      adjust: rec ? Number(rec.adjust || 0) : 0,
      credits: rec ? Number(rec.credits_end || 0) : 0,
    };
  });
  if (rows.length) {
    const live = uid
      ? Number((((state.accounts || []).find((a) => a.uid === uid)) || {}).credits || 0)
      : Number(state.totalCredits || 0);
    rows[rows.length - 1].credits = live;
  }
  return { labels, rows };
}

// renderSeriesLegend 画图例（颜色 + 名称 + 该线读哪个轴）。
function renderSeriesLegend(host) {
  if (!host) return;
  host.replaceChildren(...CREDIT_SERIES.map((s) => el('span', {}, [
    el('span', { class: 'swatch', attrs: { style: 'background:' + s.color } }),
    el('span', { text: s.name + (s.axis === 'left' ? '（左轴）' : '（右轴）') }),
  ])));
}

// chartSeriesValues 把 rows 摊成 drawLineChart 需要的 series 数组。
function chartSeriesValues(rows) {
  return CREDIT_SERIES.map((s) => ({
    name: s.name,
    color: s.color,
    axis: s.axis,
    values: rows.map((r) => r[s.key]),
  }));
}

// renderOverviewChart 概览「积分日账本」曲线：整池 4 条线，7/15/30 天可切换。
function renderOverviewChart() {
  const days = state.overviewDays || 7;
  $$('#overviewRange button').forEach((b) => {
    b.classList.toggle('active', Number(b.dataset.days) === days);
  });
  const daysLabel = $('#overviewChartDays');
  if (daysLabel) daysLabel.textContent = String(days);
  renderSeriesLegend($('#overviewChartLegend'));

  const { labels, rows } = chartRows(state.creditDaily, days, null);
  const hint = $('#overviewChartHint');
  if (hint) {
    hint.textContent = labels.length
      ? (labels[0] + ' ~ ' + labels[labels.length - 1] + '（' + labels.length + ' 天）')
      : '';
  }
  drawLineChart($('#overviewChart'), labels, chartSeriesValues(rows));
}

// renderCreditChart 用当前缓存的账本数据 + 当前区间重画弹窗内容。
function renderCreditChart() {
  const uid = state.chartUID;
  if (!uid) return;
  const days = state.chartDays || 7;
  $$('#creditModalRange button').forEach((b) => {
    b.classList.toggle('active', Number(b.dataset.days) === days);
  });
  // 图例（名称 + 该线读哪个轴）
  renderSeriesLegend($('#creditModalLegend'));

  const { labels, rows } = chartRows(state.creditDaily, days, uid);
  $('#creditModalHint').textContent = labels.length
    ? (labels[0] + ' ~ ' + labels[labels.length - 1] + '（' + labels.length + ' 天）')
    : '';
  drawLineChart($('#creditModalChart'), labels, chartSeriesValues(rows));
}

// openCreditChart 打开某账号的积分曲线弹窗（默认近 7 天）。
async function openCreditChart(uid) {
  state.chartUID = uid;
  state.chartDays = 7;
  const acc = (state.accounts || []).find((a) => a.uid === uid);
  $('#creditModalTitle').textContent = '积分曲线 · ' + uid;
  $('#creditModalMeta').textContent = acc
    ? [acc.nickname || '', acc.realm || '', '当前积分 ' + fmtNum(acc.credits)].filter(Boolean).join(' · ')
    : '';
  const host = $('#creditModalChart');
  host.replaceChildren(el('div', { class: 'text-body-secondary small py-5 text-center', text: '加载中…' }));
  bootstrap.Modal.getOrCreateInstance($('#creditChartModal')).show();
  try {
    // 每次打开都取最新（30 天覆盖三档区间）；顺手刷新概览缓存。
    state.creditDaily = await api('/admin/credit-daily?days=30');
  } catch (e) {
    host.replaceChildren(el('div', { class: 'text-danger small py-4 text-center', text: String(e.message || e) }));
    return;
  }
  renderCreditChart();
}

function statusBadge(a) {
  // 顺序与「能否接单」的判断一致：先看禁用，再看系统状态，最后看是否入池。
  if (a.disabled) {
    return a.disabled_source === 'manual'
      ? el('span', { class: 'badge text-bg-secondary status-badge', text: '手动禁用', title: a.disabled_reason || '' })
      : el('span', { class: 'badge text-bg-danger status-badge', text: '系统禁用', title: a.disabled_reason || '' });
  }
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
      const ops = el('td', { class: 'text-start text-nowrap' }, [
        a.disabled
          ? actionBtn('启用', 'success', a.uid, 'enable')
          : actionBtn('禁用', 'danger', a.uid, 'disable'),
        a.resting
          ? actionBtn('恢复', 'outline-success', a.uid, 'unrest')
          : actionBtn('休养', 'outline-secondary', a.uid, 'rest', a.disabled || !a.in_pool),

      ]);
      return el('tr', {}, [
        // 第一列（昵称）可点：打开该账号的近 7/15/30 天积分曲线弹窗。
        // 没有昵称时按钮显示「–」，仍可点（data-chart-uid 带的是真 uid）。
        el('td', {}, [el('button', {
          class: 'btn btn-link btn-sm p-0',
          text: a.nickname || '–',
          title: '点击查看近 7 / 15 / 30 天积分曲线',
          attrs: { type: 'button', 'data-chart-uid': a.uid },
        })]),
        el('td', { class: 'font-monospace small', text: a.uid }),
        el('td', { text: a.realm || '–' }),
        el('td', {}, [
          statusBadge(a),
          a.checked_in_today ? el('span', {
            class: 'badge text-bg-success-subtle text-success-emphasis ms-1',
            text: '今日已签',
            title: a.last_checkin_status ? '最近一次：' + a.last_checkin_status : '',
          }) : null,
        ].filter(Boolean)),
        el('td', { class: 'text-start', text: fmtNum(a.credits) }),
        el('td', { class: 'text-start', text: fmtNum(a.in_flight) }),
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
      const reason = prompt('禁用原因（禁用期间签到/上报/旅行/保活全部停止；只想养号请用「休养」）：', '手动禁用');
      if (reason === null) return;
      await api('/admin/accounts/' + encodeURIComponent(uid) + '/disable', { method: 'POST', body: { reason } });
      toast(uid + ' 已禁用：不接下游流量，所有自动任务停止', 'secondary');
    } else if (action === 'enable') {
      await api('/admin/accounts/' + encodeURIComponent(uid) + '/enable', { method: 'POST', body: {} });
      toast(uid + ' 已启用（手动/系统禁用一并解除）', 'success');
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
      el('td', { class: 'text-start', text: fmtNum(m.context_length) }),
      el('td', { class: 'text-start', text: fmtNum(m.max_output_tokens) }),
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

// ── 首次初始化 / 设置页 ─────────────────────────────────────
// 首次安装没有密码：后端 /admin/setup/status 会告诉我们 initialized=false，
// 页面切成"设置密码"表单；初始化成功后自动登录（管理面同时被打开）。

async function fetchSetupStatus() {
  try {
    const r = await fetch('/admin/setup/status');
    if (!r.ok) return null;
    return await r.json();
  } catch {
    return null; // 网络问题交给后续登录流程报错
  }
}

function showSetup(minLen) {
  state.setupMode = true;
  $('#app').hidden = true;
  $('#loginWrap').hidden = false;
  $('#loginForm').hidden = true;
  $('#setupForm').hidden = false;
  $('#loginSubtitle').textContent = '首次使用：初始化管理员密码';
  if (minLen) $('#setupMinLen').textContent = String(minLen);
  const err = $('#setupError');
  err.classList.add('d-none');
  err.textContent = '';
  setTimeout(() => $('#setupPassword').focus(), 30);
}

function boxSetupError(msg) {
  const box = $('#setupError');
  box.textContent = msg;
  box.classList.remove('d-none');
}

async function submitSetup(ev) {
  ev.preventDefault();
  const pw = ($('#setupPassword').value || '').trim();
  const confirm = ($('#setupConfirm').value || '').trim();
  const minLen = Number($('#setupMinLen').textContent) || 8;
  if (pw.length < minLen) return boxSetupError('密码至少 ' + minLen + ' 个字符');
  if (pw !== confirm) return boxSetupError('两次输入的密码不一致');
  const btn = $('#btnSetup');
  btn.disabled = true;
  btn.textContent = '初始化中…';
  try {
    const resp = await fetch('/admin/setup', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ password: pw, confirm }),
    });
    if (!resp.ok) {
      const data = await resp.json().catch(() => ({}));
      throw new Error(data?.error?.message || ('HTTP ' + resp.status));
    }
    await login(pw); // 初始化完成后直接登录并进入后台
    state.setupMode = false;
    showApp();
    render();
    toast('管理员密码已初始化', 'success');
  } catch (e) {
    boxSetupError(String(e.message || e));
  } finally {
    btn.disabled = false;
    btn.textContent = '初始化并进入';
  }
}

// updateRestartBanner 顶部的"待重启"横幅：保存成功即出现；进程重启后（flag 清空）消失。
function updateRestartBanner() {
  const show = !!state.restartPending;
  const box = $('#restartBanner');
  if (box) box.classList.toggle('d-none', !show);
}

async function refreshConfigMeta() {
  try {
    const d = await api('/admin/config');
    state.config = d;
    state.restartPending = !!d.meta?.restart_pending;
    updateRestartBanner();
  } catch {
    /* 管理面关闭或配置接口未接线时忽略：设置页会再显示具体错误 */
  }
}

function confirmLeaveSettings() {
  if (state.view !== 'settings' || state.configDirty.size === 0) return true;
  return confirm('设置页有未保存的修改，确定离开并丢弃吗？');
}

async function loadConfig() {
  const form = $('#cfgForm');
  if (!form) return;
  if (!state.config) {
    form.replaceChildren(el('div', { class: 'text-body-secondary small py-4 text-center', text: '加载配置中…' }));
  }
  try {
    const d = await api('/admin/config');
    state.config = d;
    state.configDirty.clear();
    state.configErrors.clear();
    state.restartPending = !!d.meta?.restart_pending;
    renderSettings();
    updateRestartBanner();
  } catch (e) {
    $('#cfgNotice').replaceChildren(el('div', { class: 'alert alert-danger py-2 small', text: String(e.message || e) }));
    form.replaceChildren();
  }
}

function renderSettings() {
  const d = state.config;
  if (!d) return;
  const notice = $('#cfgNotice');
  notice.replaceChildren();
  if (!d.meta?.writable) {
    notice.appendChild(el('div', {
      class: 'alert alert-warning py-2 small',
      text: '配置文件当前不可写（' + (d.meta?.path || '') + '）：容器内请 chown -R 10001:10001 ./data 后重试。',
    }));
  }
  $('#cfgPath').textContent = '配置文件：' + (d.meta?.path || '–');
  buildGroupNav(d.schema);
  buildConfigForm(d);
  refreshSaveState();
  // 重新渲染（保存 / 复位 / 改密后）不应丢掉搜索过滤条件。
  const q = $('#cfgSearch') ? $('#cfgSearch').value : '';
  if (q) filterSettings(q);
}

function buildGroupNav(schema) {
  const nav = $('#cfgGroups');
  nav.replaceChildren();
  for (const g of schema.groups || []) {
    const b = el('button', {
      class: 'nav-link text-start',
      text: g.title,
      attrs: { type: 'button', 'data-goto': 'cfg-group-' + g.key },
    });
    b.addEventListener('click', () => {
      const target = $('#' + b.dataset.goto);
      if (target) target.scrollIntoView({ behavior: 'smooth', block: 'start' });
    });
    nav.appendChild(b);
  }
}

function buildConfigForm(d) {
  const form = $('#cfgForm');
  form.replaceChildren();
  const values = d.values || {};
  const groups = d.schema.groups || [];
  const fields = d.schema.fields || [];
  for (const g of groups) {
    const list = fields.filter((f) => f.group === g.key);
    if (!list.length) continue;
    const card = el('div', { class: 'card mb-3 wp-cfg-card', attrs: { id: 'cfg-group-' + g.key } });
    card.appendChild(el('div', { class: 'card-header d-flex align-items-start gap-2' }, [
      el('div', { class: 'flex-grow-1' }, [
        el('h3', { class: 'card-title mb-1', text: g.title }),
        el('div', { class: 'small text-body-secondary', text: g.desc || '' }),
      ]),
      el('button', {
        class: 'btn btn-sm btn-outline-secondary flex-shrink-0',
        attrs: { type: 'button', 'data-reset-section': g.key },
      }, [icon('ti-rotate'), document.createTextNode('恢复本组默认')]),
    ]));
    const body = el('div', { class: 'card-body py-2' });
    for (const f of list) body.appendChild(renderField(f, values));
    card.appendChild(body);
    form.appendChild(card);
  }
}

function icon(name) {
  const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  svg.setAttribute('class', 'wp-ico');
  svg.setAttribute('aria-hidden', 'true');
  const use = document.createElementNS('http://www.w3.org/2000/svg', 'use');
  use.setAttribute('href', '#' + name);
  svg.appendChild(use);
  return svg;
}

function renderField(field, values) {
  const row = el('div', { class: 'wp-cfg-row', attrs: { 'data-path': field.path } });
  const labelId = 'cfg-lbl-' + field.path.replace(/[^a-zA-Z0-9]/g, '-');
  const inputId = 'cfg-in-' + field.path.replace(/[^a-zA-Z0-9]/g, '-');

  const labelBox = el('div', { class: 'wp-cfg-label' }, [
    el('label', { class: 'form-label mb-0', text: field.label, attrs: { for: inputId, id: labelId } }),
    el('div', { class: 'wp-cfg-key', text: field.path }),
    field.hint ? el('div', { class: 'wp-cfg-hint', text: field.hint }) : null,
  ].filter(Boolean));

  const value = getPath(values, field.path);
  const { node, read } = buildControl(field, value, inputId);
  const control = el('div', { class: 'wp-cfg-control' }, [node]);

  const isDefault = sameValue(value, getPath(state.config.defaults || {}, field.path));
  const badge = el('span', { class: 'badge wp-cfg-badge wp-cfg-state ' + (isDefault ? 'text-bg-secondary' : 'text-bg-info'), text: isDefault ? '默认' : '已改' });
  const reset = el('button', {
    class: 'btn btn-sm btn-outline-secondary',
    attrs: { type: 'button', title: '恢复默认值' },
  }, [icon('ti-rotate'), document.createTextNode('恢复默认')]);
  reset.addEventListener('click', () => resetField(field.path));

  const errBox = el('div', { class: 'wp-cfg-error small text-danger', attrs: { hidden: '' } });
  const actions = el('div', { class: 'wp-cfg-actions' });
  if (!field.restart) {
    actions.appendChild(el('span', {
      class: 'badge wp-cfg-badge wp-cfg-instant text-bg-success',
      text: '即时生效',
      title: '保存后立即生效，无需重启',
    }));
  }
  actions.append(badge, reset);
  row.append(labelBox, control, actions, errBox);
  row.dataset.search = (field.label + ' ' + field.path + ' ' + (field.hint || '')).toLowerCase();
  row.dataset.badge = 'cfg-badge-' + field.path;

  const onEdit = () => {
    const out = read();
    if (out.error) {
      state.configErrors.set(field.path, out.error);
      showFieldError(field.path, out.error);
      state.configDirty.delete(field.path);
      refreshSaveState();
      return;
    }
    const err = validateField(field, out.value);
    if (err) {
      state.configErrors.set(field.path, err);
      showFieldError(field.path, err);
    } else {
      state.configErrors.delete(field.path);
      showFieldError(field.path, '');
    }
    const orig = getPath(state.config.values || {}, field.path);
    if (sameValue(out.value, orig)) state.configDirty.delete(field.path);
    else state.configDirty.set(field.path, out.value);
    refreshSaveState();
  };
  node.addEventListener('input', onEdit);
  node.addEventListener('change', onEdit);
  return row;
}

function showFieldError(path, msg) {
  const row = document.querySelector('.wp-cfg-row[data-path="' + cssEscape(path) + '"]');
  if (!row) return;
  const box = row.querySelector('.wp-cfg-error');
  if (!box) return;
  if (msg) {
    box.textContent = msg;
    box.removeAttribute('hidden');
  } else {
    box.textContent = '';
    box.setAttribute('hidden', '');
  }
}

function cssEscape(s) {
  return String(s).replace(/"/g, '\\"');
}

// buildControl 按 kind 生成控件，并返回 read()（把界面值解析成 JSON 值）。
function buildControl(field, value, inputId) {
  const common = { id: inputId };
  switch (field.kind) {
    case 'bool': {
      const wrap = el('div', { class: 'form-check form-switch' });
      const input = el('input', { class: 'form-check-input', attrs: { ...common, type: 'checkbox' } });
      input.checked = !!value;
      wrap.appendChild(input);
      return { node: wrap, read: () => ({ value: input.checked }) };
    }
    case 'enum': {
      const sel = el('select', { class: 'form-select form-select-sm', attrs: common });
      for (const o of field.options || []) sel.appendChild(el('option', { text: o.label, attrs: { value: o.value } }));
      sel.value = value == null ? '' : String(value);
      return { node: sel, read: () => ({ value: sel.value }) };
    }
    case 'int':
    case 'float': {
      const input = el('input', {
        class: 'form-control form-control-sm',
        attrs: { ...common, type: 'number', step: field.kind === 'float' ? 'any' : '1', min: field.min, max: field.max },
      });
      input.value = value == null ? '' : String(value);
      return { node: input, read: () => readNumber(field, input.value) };
    }
    case 'timelist':
    case 'durationlist':
    case 'stringlist': {
      const ta = el('textarea', {
        class: 'form-control form-control-sm',
        attrs: { ...common, rows: field.kind === 'stringlist' ? 3 : 2, placeholder: listPlaceholder(field) },
      });
      ta.value = fmtValue(field, value);
      return {
        node: ta,
        read: () => ({ value: splitList(ta.value, field.kind) }),
      };
    }
    case 'stringmap': {
      const ta = el('textarea', {
        class: 'form-control form-control-sm',
        attrs: { ...common, rows: 3, placeholder: '线路名 = http://127.0.0.1:7890\n直线 = socks5://127.0.0.1:1080' },
      });
      ta.value = fmtValue(field, value);
      return { node: ta, read: () => parseMapText(ta.value) };
    }
    case 'pairs': {
      const ta = el('textarea', {
        class: 'form-control form-control-sm',
        attrs: { ...common, rows: 3, placeholder: '原串 => 替换串' },
      });
      ta.value = fmtValue(field, value);
      return { node: ta, read: () => parsePairs(ta.value) };
    }
    default: {
      const input = el('input', {
        class: 'form-control form-control-sm',
        attrs: { ...common, type: field.secret ? 'password' : 'text', placeholder: field.placeholder || '' },
      });
      input.value = value == null ? '' : String(value);
      if (!field.secret) return { node: input, read: () => ({ value: input.value }) };
      const btn = el('button', {
        class: 'btn btn-outline-secondary btn-icon',
        attrs: { type: 'button', title: '显示 / 隐藏' },
      }, [icon('ti-eye')]);
      btn.addEventListener('click', () => {
        input.type = input.type === 'password' ? 'text' : 'password';
      });
      const group = el('div', { class: 'input-group input-group-sm' }, [input, btn]);
      return { node: group, read: () => ({ value: input.value }) };
    }
  }
}

function readNumber(field, raw) {
  const s = String(raw).trim();
  if (s === '') return { value: null };
  const n = Number(s);
  if (!Number.isFinite(n)) return { error: '应为数字' };
  if (field.kind === 'int' && !Number.isInteger(n)) return { error: '应为整数' };
  return { value: n };
}

function splitList(text, kind) {
  const parts = String(text).split(/[\n,]+/).map((s) => s.trim()).filter(Boolean);
  if (kind === 'timelist') return parts.map((s) => s);
  return parts;
}

function parseMapText(text) {
  const out = {};
  for (const line of String(text).split('\n')) {
    const t = line.trim();
    if (!t) continue;
    const i = t.search(/[=:]/);
    if (i <= 0) return { error: '每行应为「名称 = 地址」' };
    const k = t.slice(0, i).trim();
    const v = t.slice(i + 1).trim();
    if (!k || !v) return { error: '「名称 = 地址」两侧都不能为空' };
    out[k] = v;
  }
  return { value: out };
}

function parsePairs(text) {
  const out = [];
  for (const line of String(text).split('\n')) {
    const t = line.trim();
    if (!t) continue;
    const m = t.split(/=>/);
    if (m.length !== 2) return { error: '每行应为「原串 => 替换串」' };
    out.push([m[0].trim(), m[1].trim()]);
  }
  return { value: out };
}

function listPlaceholder(field) {
  if (field.kind === 'timelist') return '09:00, 21:00';
  if (field.kind === 'durationlist') return '5s, 15s, 60s';
  return '一行一个值';
}

function fmtValue(field, v) {
  if (v == null) return '';
  switch (field.kind) {
    case 'timelist': return Array.isArray(v) ? v.join(', ') : String(v);
    case 'durationlist':
    case 'stringlist': return Array.isArray(v) ? v.join('\n') : String(v);
    case 'stringmap': return Object.entries(v).map(([k, val]) => k + ' = ' + val).join('\n');
    case 'pairs': return (v || []).map((p) => p[0] + ' => ' + p[1]).join('\n');
    default: return String(v);
  }
}

// validateField 前端即时校验：规则与后端 schema 一一对应（后端仍是最终裁决者）。
function validateField(field, v) {
  if (field.kind === 'bool') return '';
  if (field.kind === 'int' || field.kind === 'float') {
    if (v == null) return '';
    if (field.min != null && v < field.min) return '不能小于 ' + field.min;
    if (field.max != null && v > field.max) return '不能大于 ' + field.max;
    return '';
  }
  if (field.kind === 'duration') {
    const s = String(v || '').trim();
    if (s === '') return '';
    if (!/^\d+(\.\d+)?(ns|us|µs|ms|s|m|h)$/.test(s)) return '时长格式应为 500ms / 30s / 5m / 2h';
    return '';
  }
  if (field.kind === 'timelist') {
    for (const t of v || []) {
      if (!/^([01]?\d|2[0-3]):[0-5]\d$/.test(String(t).trim())) return '时刻格式应为 HH:MM';
    }
    return '';
  }
  if (field.kind === 'durationlist') {
    for (const t of v || []) {
      if (!/^\d+(\.\d+)?(ns|us|µs|ms|s|m|h)$/.test(String(t).trim())) return '时长格式不正确：' + t;
    }
    return '';
  }
  if (field.kind === 'stringmap') {
    for (const [k, val] of Object.entries(v || {})) {
      if (!String(val).trim()) return k + ' 的值不能为空';
    }
    return '';
  }
  if (field.kind === 'pairs') {
    for (const p of v || []) {
      if (!Array.isArray(p) || p.length !== 2) return '每行应为「原串 => 替换串」';
    }
    return '';
  }
  if (field.kind === 'enum') {
    const opts = (field.options || []).map((o) => o.value);
    return opts.includes(String(v)) ? '' : '取值不在允许范围内';
  }
  if (field.pattern) {
    try {
      if (!new RegExp(field.pattern).test(String(v ?? ''))) return '格式不正确';
    } catch { /* schema 正则写错时不阻塞前端 */ }
  }
  return '';
}

function sameValue(a, b) {
  return JSON.stringify(a === undefined ? null : a) === JSON.stringify(b === undefined ? null : b);
}

function refreshSaveState() {
  const dirty = state.configDirty.size;
  const errors = state.configErrors.size;
  const hint = $('#cfgDirtyHint');
  if (hint) {
    hint.textContent = errors > 0
      ? ('有 ' + errors + ' 处格式不正确，修正后才能保存')
      : (dirty > 0 ? ('有 ' + dirty + ' 处修改未保存') : '没有未保存的修改');
  }
  const btn = $('#btnSaveConfig');
  if (btn) btn.disabled = dirty === 0 || errors > 0;
  // 更新每行的"默认 / 已改 / 待保存"徽章（编辑回原值时要能退回去）。
  document.querySelectorAll('#cfgForm .wp-cfg-row').forEach((row) => {
    const badge = row.querySelector('.wp-cfg-state');
    if (!badge) return;
    const path = row.dataset.path;
    if (state.configDirty.has(path)) {
      badge.textContent = '待保存';
      badge.className = 'badge wp-cfg-badge wp-cfg-state text-bg-warning';
      return;
    }
    const cur = getPath(state.config.values || {}, path);
    const def = getPath(state.config.defaults || {}, path);
    const isDefault = sameValue(cur, def);
    badge.textContent = isDefault ? '默认' : '已改';
    badge.className = 'badge wp-cfg-badge wp-cfg-state ' + (isDefault ? 'text-bg-secondary' : 'text-bg-info');
  });
}

async function saveConfig() {
  if (!state.configDirty.size) return;
  if (state.configDirty.get('admin.enabled') === false) {
    const ok = confirm('关闭「启用管理面」会立刻让设置页和其它管理接口返回 404；\n' +
      '要重新打开只能改 data/config.json 再重启容器。确定继续吗？');
    if (!ok) {
      state.configDirty.delete('admin.enabled');
      renderSettings();
      return;
    }
  }
  const values = {};
  for (const [path, v] of state.configDirty) setObjPath(values, path, v);
  const btn = $('#btnSaveConfig');
  btn.disabled = true;
  try {
    const resp = await fetch('/admin/config', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json', Authorization: 'Bearer ' + state.password },
      body: JSON.stringify({ values }),
    });
    const data = await resp.json().catch(() => ({}));
    if (resp.status === 401) {
      clearSession();
      showLogin('管理密码已失效，请重新登录');
      return;
    }
    if (!resp.ok) {
      const fields = data?.error?.fields || [];
      for (const f of fields) {
        state.configErrors.set(f.path, f.message);
        showFieldError(f.path, f.message);
      }
      if (!fields.length) alertBox(data?.error?.message || ('HTTP ' + resp.status), 'danger');
      refreshSaveState();
      return;
    }
    state.config.values = data.values || state.config.values;
    state.configDirty.clear();
    state.configErrors.clear();
    state.restartPending = !!data.restart_pending;
    state.config.meta = { ...(state.config.meta || {}), restart_pending: state.restartPending };
    renderSettings();
    updateRestartBanner();
    toast('配置已保存：重启容器后生效', 'success');
  } catch (e) {
    alertBox('保存失败：' + String(e.message || e), 'danger');
  } finally {
    refreshSaveState();
  }
}

async function resetConfig(body, okMsg) {
  try {
    const data = await api('/admin/config/reset', { method: 'POST', body });
    if (state.config) {
      state.config.values = data.values || state.config.values;
      state.configDirty.clear();
      state.configErrors.clear();
      state.restartPending = !!data.restart_pending;
      state.config.meta = { ...(state.config.meta || {}), restart_pending: state.restartPending };
      renderSettings();
      updateRestartBanner();
    }
    toast(okMsg, 'success');
  } catch (e) {
    alertBox('恢复默认失败：' + String(e.message || e), 'danger');
  }
}

function resetField(path) {
  resetConfig({ scope: 'fields', paths: [path] }, '已恢复该字段默认值（重启后生效）');
}

function resetSection(section) {
  if (!confirm('把「' + section + '」整组恢复默认？该组内所有修改都会丢失。')) return;
  resetConfig({ scope: 'section', section }, '已恢复该组默认值（重启后生效）');
}

function resetAll() {
  if (!confirm('把所有配置恢复默认？\n\n注意：api_key 会回到 test_key，监听地址等也会重置；管理密码与「启用管理面」保持不变。')) return;
  resetConfig({ scope: 'all' }, '全部配置已恢复默认（重启后生效）');
}

async function changePassword() {
  const pw = ($('#newPassword').value || '').trim();
  const confirm2 = ($('#newPassword2').value || '').trim();
  const minLen = state.config?.meta?.min_password_length || 8;
  if (pw.length < minLen) return alertBox('新密码至少 ' + minLen + ' 个字符', 'warning');
  if (pw !== confirm2) return alertBox('两次输入的新密码不一致', 'warning');
  const btn = $('#btnChangePassword');
  btn.disabled = true;
  try {
    await api('/admin/password', { method: 'POST', body: { password: pw, confirm: confirm2 } });
    state.password = pw;
    localStorage.setItem(PW_STORE, pw);
    $('#newPassword').value = '';
    $('#newPassword2').value = '';
    toast('管理员密码已更新', 'success');
  } catch (e) {
    alertBox('修改失败：' + String(e.message || e), 'danger');
  } finally {
    btn.disabled = false;
  }
}

function getPath(obj, path) {
  let cur = obj;
  for (const seg of String(path).split('.')) {
    if (cur == null || typeof cur !== 'object') return undefined;
    cur = cur[seg];
  }
  return cur;
}

function setObjPath(obj, path, val) {
  const segs = String(path).split('.');
  let cur = obj;
  for (const seg of segs.slice(0, -1)) {
    if (cur[seg] == null || typeof cur[seg] !== 'object') cur[seg] = {};
    cur = cur[seg];
  }
  cur[segs[segs.length - 1]] = val;
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
      if (!confirmLeaveSettings()) return;
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

  // 账号行操作走事件委托（行是动态生成的，逐个绑定会漏）。
  // 第一列 UID 是「打开积分曲线」，其余按钮是「动作」——同一委托里按目标分流。
  $('#accountsBody').addEventListener('click', (ev) => {
    const btn = ev.target.closest('button[data-action]');
    if (btn) {
      doAccountAction(btn.dataset.uid, btn.dataset.action);
      return;
    }
    const chartBtn = ev.target.closest('button[data-chart-uid]');
    if (chartBtn) openCreditChart(chartBtn.dataset.chartUid);
  });

  // 积分曲线弹窗的区间切换（7 / 15 / 30 天）
  $$('#creditModalRange button').forEach((b) => {
    b.addEventListener('click', () => {
      state.chartDays = Number(b.dataset.days) || 7;
      renderCreditChart();
    });
  });

  // 概览「积分日账本」曲线的区间切换（7 / 15 / 30 天）
  $$('#overviewRange button').forEach((b) => {
    b.addEventListener('click', () => {
      state.overviewDays = Number(b.dataset.days) || 7;
      renderOverviewChart();
    });
  });

  $('#btnSend').addEventListener('click', sendPlayground);
  $('#btnAbort').addEventListener('click', () => {
    if (state.aborter) state.aborter.abort();
  });
  $('#btnAuthStart').addEventListener('click', startAuth);

  // 首次初始化
  if ($('#setupForm')) $('#setupForm').addEventListener('submit', submitSetup);

  // 设置页
  if ($('#btnSaveConfig')) $('#btnSaveConfig').addEventListener('click', saveConfig);
  if ($('#btnResetAll')) $('#btnResetAll').addEventListener('click', resetAll);
  if ($('#btnChangePassword')) $('#btnChangePassword').addEventListener('click', changePassword);
  if ($('#cfgSearch')) {
    $('#cfgSearch').addEventListener('input', (ev) => filterSettings(ev.target.value));
  }
  if ($('#cfgForm')) {
    $('#cfgForm').addEventListener('click', (ev) => {
      const btn = ev.target.closest('button[data-reset-section]');
      if (btn) resetSection(btn.dataset.resetSection);
    });
  }
  window.addEventListener('beforeunload', (ev) => {
    if (state.configDirty && state.configDirty.size > 0) {
      ev.preventDefault();
      ev.returnValue = '';
    }
  });

  window.addEventListener('hashchange', render);
}

// filterSettings 按名称/key/说明过滤字段（分组内全被过滤时整组隐藏）。
function filterSettings(q) {
  const s = String(q || '').trim().toLowerCase();
  document.querySelectorAll('#cfgForm .wp-cfg-card').forEach((card) => {
    let visible = 0;
    card.querySelectorAll('.wp-cfg-row').forEach((row) => {
      const hit = !s || (row.dataset.search || '').includes(s);
      row.hidden = !hit;
      if (hit) visible++;
    });
    card.hidden = visible === 0;
  });
}

// boot：先问"密码初始化了吗"，再决定显示初始化向导还是登录页。
// 有本地密码就先验一次：密码改过/失效就清掉并停在登录页。
(async function boot() {
  // 标记：app.js 已执行并接管页面（自动化脚本据此判断可交互，见 scripts/ui-login-check.mjs）。
  document.documentElement.dataset.appBooted = '1';
  applyTheme(localStorage.getItem(THEME_STORE) || (matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light'));
  bind();

  const st = await fetchSetupStatus();
  if (st && st.initialized === false) {
    showSetup(st.min_password_length || 8);
    return;
  }
  if (st && st.admin_enabled === false) {
    showLogin('管理面已关闭：在 data/config.json 里把 admin.enabled 改回 true 后重启容器');
    return;
  }
  if (!state.password) {
    showLogin(null);
    return;
  }
  try {
    await login(state.password);
    showApp();
    render();
    refreshConfigMeta();
  } catch (e) {
    clearSession();
    showLogin('登录失败：' + String(e.message || e));
  }
})();
