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
// 前端资源版本（/assets/version.json）与本地已加载版本的键。
const VERSION_STORE = 'wp.frontend.version';
const VERSION_POLL_MS = 60000;

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

  // 概览轮询（5s）：timer 只在概览视图存在；in-flight 防叠加；raw 用于比对
  // 避免每轮都重写整段 JSON；chartSig/DrawnAt 控制曲线的重画频率。
  overviewTimer: null,
  overviewInFlight: false,
  overviewRaw: '',
  overviewChartSig: '',
  overviewChartDrawnAt: 0,
  overviewDays: 7,   // 概览「积分日账本」曲线区间（7 / 15 / 30，默认 7）

  // 设置页
  config: null,          // {values, defaults, schema, meta}
  configDirty: new Map(),// path -> 待保存值
  configErrors: new Map(),// path -> 错误文案
  configFilter: '',
  setupMode: false,      // 首次初始化密码
  restartPending: false, // 有改动等待重启

  // 接入映射（下游 key 的账号/模型白名单）
  accessKeys: [],        // /admin/access-keys 的 keys[]
  accessAccounts: [],    // 账号选择器数据（/admin/accounts）
  accessModels: [],      // 模型选择器数据（/admin/models）
  accessDraft: null,     // {id,name,key,enabled,all_accounts,all_models,accounts:Set,models:Set}
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

// fmtAgo 把 ISO 时间渲染成「x 分钟前 / x 小时前 / x 天前」（本地时区、粗粒度够用）。
function fmtAgo(iso) {
  if (!iso) return '';
  const t = Date.parse(iso);
  if (!Number.isFinite(t)) return '';
  const sec = Math.max(0, Math.round((Date.now() - t) / 1000));
  if (sec < 90) return '刚刚';
  const min = Math.round(sec / 60);
  if (min < 90) return min + ' 分钟前';
  const h = Math.round(min / 60);
  if (h < 36) return h + ' 小时前';
  return Math.round(h / 24) + ' 天前';
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
  stopOverviewPolling();
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
const VIEWS = ['overview', 'accounts', 'models', 'access', 'playground', 'add', 'settings'];

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
    case 'overview':
      // 首次进入强制重画曲线；之后每 5 秒只更新 KPI（曲线最多 60 秒重画一次）。
      refreshOverview({ forceChart: !state.creditDaily });
      startOverviewPolling();
      break;
    case 'accounts': loadAccounts(); break;
    case 'access': loadAccessKeys(); break;
    case 'models': loadModels(); break;
    case 'playground': loadModels(true); break;
    case 'add': break;
    case 'settings': loadConfig(); break;
  }
  if (state.view !== 'overview') stopOverviewPolling();
  updateRestartBanner();
  // 视图从 hidden 变可见后容器才有真实宽高：下一帧统一校正（详见 fitChart）。
  requestAnimationFrame(() => resizeCharts());
}

// ── 概览（5 秒轮询；后端只读内存/落盘缓存，不碰上游）──────────────
// 为什么轮询：概览是"运维瞄一眼"的页面，手动刷新不现实；但也不能让前端直接打上游
// （签到/余额这类动作不在概览的职责里）。所以由后端提供 /admin/overview 聚合快照：
// status（池运行态）+ credits（当日热值 + 落盘历史），一次请求、纯本地读取。
const OVERVIEW_POLL_MS = 5000;
// 曲线重画的最小间隔：今天的热值每 5 秒都在变，但 ECharts setOption(notMerge) 会重置
// tooltip/缩放；所以 KPI 走 5 秒，曲线最多 60 秒重画一次（历史日变化时立即重画）。
const OVERVIEW_CHART_MIN_MS = 60000;

function setText(sel, v) {
  const el = $(sel);
  if (el) el.textContent = v;
}

// refreshOverview 拉取聚合快照并渲染；in-flight 标记防止慢请求叠加。
async function refreshOverview(opts = {}) {
  if (state.overviewInFlight) return;
  state.overviewInFlight = true;
  try {
    const d = await api('/admin/overview');
    applyOverview(d, opts);
    const stamp = new Date().toLocaleTimeString('zh-CN', { hour12: false });
    setText('#overviewUpdated', '更新于 ' + stamp + ' · 每 5 秒自动刷新');
  } catch (e) {
    setText('#overviewChartHint', String(e.message || e));
    setText('#statusRaw', String(e.message || e));
  } finally {
    state.overviewInFlight = false;
  }
}

function applyOverview(d, opts = {}) {
  const s = d.status || {};
  setText('#kpiTotal', fmtNum(s.total));
  setText('#kpiHealthy', fmtNum(s.healthy));
  setText('#kpiCooling', fmtNum(s.cooling));
  setText('#kpiDisabled', fmtNum(s.disabled));
  setText('#kpiInFlight', fmtNum(s.in_flight_full));
  setText('#kpiSticky', fmtNum(s.sticky_sessions));
  // 总积分 = 池内各账号余额之和（与账号页同源）；曲线"今天"的点也用它。
  state.totalCredits = (s.accounts || []).reduce((acc, a) => acc + Number(a.credits || 0), 0);
  setText('#kpiCredits', fmtNum(state.totalCredits));
  const raw = JSON.stringify(s, null, 2);
  if (raw !== state.overviewRaw) {
    state.overviewRaw = raw;
    setText('#statusRaw', raw);
  }
  if (d.credits) applyCredits(d.credits, opts);
}

function applyCredits(c, opts = {}) {
  state.creditDaily = c; // 缓存给「单账号曲线」弹窗复用
  const t = c.totals || {};
  setText('#creditToday',
    c.today + '：获得 ' + fmtNum(t.earned || 0) +
    ' / 核销（消耗 ' + fmtNum(t.spent || 0) + ' + 过期&校正 ' + fmtNum(t.adjust || 0) + '）' +
    ' / 净 ' + fmtNum(t.net || 0) +
    '　（账号 ' + Object.keys(c.accounts || {}).length + ' 个）');
  setText('#kpiEarned', fmtNum(t.earned || 0));
  setText('#kpiSpent', fmtNum(t.spent || 0));
  setText('#kpiAdjust', fmtNum(t.adjust || 0));
  // 今日净变化 = 今日获得 − 今日核销（核销 = 消耗 + 过期&校正）。
  const net = Number(t.net) || 0;
  setText('#kpiNet', net > 0 ? '+' + fmtNum(net) : net < 0 ? '-' + fmtNum(-net) : '0');

  // 只对"历史日"做变化检测：今天的热值每轮都在变，不能拿它当重画依据。
  const past = (c.histories || []).filter((h) => h.date !== c.today);
  const sig = JSON.stringify(past);
  const now = Date.now();
  const due = now - (state.overviewChartDrawnAt || 0) > OVERVIEW_CHART_MIN_MS;
  if (opts.forceChart || sig !== state.overviewChartSig || due) {
    state.overviewChartSig = sig;
    state.overviewChartDrawnAt = now;
    renderOverviewChart();
  }
}

// startOverviewPolling 让刷新落在墙钟 5 秒整数倍（:00/:05/:10…），而不是从页面
// 加载起算的"每 5 秒"。每次 tick 后按当前时钟重新计算到下一个边界的时间，避免
// setTimeout 的累积漂移；页面隐藏/切走时仍会继续排下一轮，但不会发请求。
function scheduleOverviewTick() {
  const wait = OVERVIEW_POLL_MS - (Date.now() % OVERVIEW_POLL_MS);
  state.overviewTimer = setTimeout(() => {
    state.overviewTimer = null;
    if (!document.hidden && state.view === 'overview' && state.password) refreshOverview();
    if (state.view === 'overview' && state.password) scheduleOverviewTick();
  }, wait);
}

function startOverviewPolling() {
  if (state.overviewTimer) return;
  scheduleOverviewTick();
}

function stopOverviewPolling() {
  if (state.overviewTimer) {
    clearTimeout(state.overviewTimer);
    state.overviewTimer = null;
  }
}

// ── 账号 ────────────────────────────────────────────────────

// ── 积分曲线（ECharts：三条独立折线，共用对数刻度轴）────────────────────────
// 数据源：GET /admin/credit-daily?days=30 —— histories[] 已按天给出**每账号**
// earned/spent/adjust/credits_end，所以 7/15/30 天只是同一份数据切片，不需要额外接口。
//
// 为什么用 ECharts：CSP 是 `default-src 'self'`（引不了 CDN），所以把 echarts.min.js
// 原样 vendored 进本目录（见 NOTICE.txt、internal/webui/webui.go 的 frameworkAssets），
// 从 /assets 自托管；仍然没有 npm/构建步骤这一层。
// 三条线**各自独立**：每条线的位置只由当天的值决定，0 就落在 0 轴上。
// 早期版本把它们堆在同一个 stack 里，导致「调整=0」被画到「获得+消耗」的累计高度
// （工具提示是 0、图上却不是 0）；现在不堆叠，读数与曲线一致。

// 三条序列的语义（ax 字段决定挂哪个轴；flow=true 是流水线）。
// 「核销」= 消耗 + 过期&校正：曲线上画两项之和，tooltip 仍显示 A+B 让来源可核对。
const CREDIT_SERIES = [
  { key: 'credits', name: '日终总计', color: '#206bc4', ax: 'left' },
  { key: 'earned', name: '获得', color: '#2fb344', ax: 'right', flow: true },
  { key: 'writeoff', name: '核销', color: '#d63939', ax: 'right', flow: true },
];

// chartInstances 每个宿主一个 ECharts 实例（切区间/切主题复用，不重复 init）。
const chartInstances = new Map();

// chartTheme 从当前主题取一套配色：ECharts 画在 canvas 上，拿不到 CSS 变量，
// 只能读 dataset.bsTheme 自己给色值（深浅两套）。
function chartTheme() {
  const dark = document.documentElement.dataset.bsTheme === 'dark';
  return {
    text: dark ? '#9aa4b2' : '#667382',
    axis: dark ? '#39424e' : '#e6e7e9',
    split: dark ? 'rgba(255,255,255,.07)' : 'rgba(24,36,51,.08)',
    tooltipBg: dark ? 'rgba(24,32,44,.96)' : 'rgba(255,255,255,.98)',
    tooltipText: dark ? '#e6eaf0' : '#182433',
    tooltipBorder: dark ? '#39424e' : '#e6e7e9',
  };
}

// fmtAxis 坐标轴刻度用的紧凑数字（12000 → 1.2万，3400 → 3.4k）。
function fmtAxis(v) {
  const abs = Math.abs(v);
  if (abs >= 10000) return (v / 10000).toFixed(abs >= 100000 ? 0 : 1) + '万';
  if (abs >= 1000) return (v / 1000).toFixed(abs >= 10000 ? 0 : 1) + 'k';
  return String(Math.round(v));
}

// ── 对数刻度（带偏移）：压低低值区占比 ────────────────────────────────
// 纯 log1p 会让 0–100 吃掉太多轴高；改成 log10(v + 1000)，把低值段抬离 0。
// 轴上限不写死：由 chartBaselines 按数据最大值取 1/2/5 刻度，刚好覆盖数据。
const SCALE_OFFSET = 1000;

function scaleData(v, axisMaxRaw) {
  const n = Math.max(0, Number(v) || 0);
  const cap = Math.max(1, Number(axisMaxRaw) || 1);
  const span = Math.log10((cap + SCALE_OFFSET) / SCALE_OFFSET);
  if (span <= 0) return 0;
  return Math.log10((n + SCALE_OFFSET) / SCALE_OFFSET) / span;
}

// invScale 仅供调试/反解：把缩放后的坐标还原成原始积分。
function invScale(v, axisMaxRaw) {
  const cap = Math.max(1, Number(axisMaxRaw) || 1);
  const span = Math.log10((cap + SCALE_OFFSET) / SCALE_OFFSET);
  const n = Math.max(0, Number(v) || 0);
  return SCALE_OFFSET * Math.pow(10, n * span) - SCALE_OFFSET;
}

// chartBaselines 生成"常识基准线"：0 / 1 / 2 / 5 / 10 / 20 / 50 / 100 / 200 / 500 / 1000 …
// 即每个数量级取 1×、2×、5× 三档（k≥0，个位也算），最后一条 ≥ 数据最大值
// （这样顶端刻度总有意义）。
// 为什么不按等分：对数轴上等分出来的刻度是 0/6/54/402/3k 这种"看着像随机数"的值；
// 1/2/5 阶梯是工程上最顺手的量级参照，10 与 20、200 与 500 之间一眼能定位。
// 低值区仍然会被日志压缩，由 creditChartOption 按缩放后的间距再筛线/标签。
function chartBaselines(maxRaw) {
  const out = [0];
  for (let k = 0; k <= 12; k++) {
    const p = Math.pow(10, k);
    for (const m of [1, 2, 5]) {
      const v = m * p;
      out.push(v);
      if (v >= maxRaw) return out;
    }
  }
  return out;
}

// creditChartOption 组装 ECharts option：三条线**各自独立**（不堆叠）画在同一个
// 对数刻度轴上。早期版本把四条线放进同一个 stack，结果"调整=0"的那条线会被画到
// "获得+消耗"的累计高度，看起来和获得持平——工具提示写着 0、图上却不是 0。
function creditChartOption(labels, rows) {
  const t = chartTheme();
  // 图例顺序：两条流水在前，日终余额最后（余额量级最大，放最后更符合阅读顺序）。
  const order = [...CREDIT_SERIES.filter((s) => s.flow), CREDIT_SERIES[0]];

  // 轴上限不写死：按数据最大值取 1/2/5 基准，刚好覆盖数据即可。
  let maxPositive = 0;
  rows.forEach((r) => {
    order.forEach((s) => {
      const raw = Number(r[s.key]) || 0;
      if (raw > maxPositive) maxPositive = raw;
    });
  });
  const baselines = chartBaselines(maxPositive);
  const axisRawMax = baselines[baselines.length - 1];
  // 每条线喂 scaleData(自己的值, axisRawMax)：0 仍然是 0，不会被相邻序列抬高。
  const seriesData = new Map(order.map((s) => [
    s.key, rows.map((r) => scaleData(r[s.key], axisRawMax)),
  ]));
  // 顶端留 3%：最后一条基准线的标签不会贴到画布上边缘。
  const axisMax = scaleData(axisRawMax, axisRawMax) * 1.03;
  // 低值区被日志压缩后基准线会挤在一起：先按"缩放后至少占轴高 1.2%"筛线，
  // 标签再按 3% 抽稀；0 与最后一条始终保留。
  const LINE_GAP = axisMax * 0.012;
  const LABEL_GAP = axisMax * 0.03;
  const marks = [];
  let lastLineY = -Infinity;
  let lastLabelY = -Infinity;
  baselines.forEach((v, i) => {
    const y = scaleData(v, axisRawMax);
    const isLast = v === axisRawMax;
    if (marks.length && !isLast && y - lastLineY < LINE_GAP) return;
    const show = i === 0 || isLast || y - lastLabelY >= LABEL_GAP;
    if (show) lastLabelY = y;
    marks.push({ v, y, show });
    lastLineY = y;
  });

  return {
    animationDuration: 280,
    textStyle: { fontFamily: 'inherit' },
    // 右侧留出基准线标签的宽度（标签挂在线的末端）
    grid: { left: 56, right: 12, top: 34, bottom: 2, containLabel: true },
    legend: {
      top: 0, left: 0, itemWidth: 10, itemHeight: 10, icon: 'roundRect',
      textStyle: { color: t.text, fontSize: 11 },
    },
    tooltip: {
      trigger: 'axis',
      backgroundColor: t.tooltipBg,
      borderColor: t.tooltipBorder,
      textStyle: { color: t.tooltipText, fontSize: 12 },
      axisPointer: { type: 'line', lineStyle: { color: t.axis } },
      // 图上的数经过对数刻度压缩，tooltip 直接读原始 rows，避免露出缩放值。
      formatter: (params) => {
        const i = params && params.length ? params[0].dataIndex : 0;
        const r = rows[i];
        if (!r) return '';
        const net = r.earned - r.spent - r.adjust;
        return [
          `<b>${labels[i]}</b>`,
          `日终总计：<b>${fmtNum(r.credits)}</b>`,
          `获得：${fmtNum(r.earned)}`,
          // 核销 = 消耗 + 过期&校正；按需求显示 A+B，而不是合并后的结果。
          `核销：${fmtNum(r.spent)} + ${fmtNum(r.adjust)}`,
          `净变化：${net > 0 ? '+' : ''}${fmtNum(net)}`,
        ].join('<br/>');
      },
    },
    xAxis: {
      type: 'category',
      boundaryGap: false,
      data: labels.map((d) => String(d).slice(5)), // 2026-11-12 → 11-12
      axisLine: { lineStyle: { color: t.axis } },
      axisTick: { show: false },
      axisLabel: { color: t.text, fontSize: 11, hideOverlap: true },
    },
    yAxis: {
      // 轴本身不画刻度/标签：基线由下面 series 的 markLine 按"常识基准线"画
      // （0/10/50/100/500/1k/5k/1万…），标签用原始积分值。
      type: 'value', min: 0, max: axisMax, position: 'left',
      axisLine: { show: false },
      axisTick: { show: false },
      axisLabel: { show: false },
      splitLine: { show: false },
    },
    series: order.map((s, idx) => ({
      // 三条线各自独立、不堆叠：每条线的位置只由自己的值决定（0 就画在 0 上）。
      name: s.name,
      type: 'line', smooth: 0.25,
      symbol: s.flow ? 'none' : 'circle', symbolSize: s.flow ? 0 : 5,
      showSymbol: !s.flow && labels.length <= 15,
      lineStyle: { width: s.flow ? 1.6 : 2.2, color: s.color },
      itemStyle: { color: s.color },
      emphasis: { focus: 'series' },
      z: s.flow ? 2 : 3,
      data: seriesData.get(s.key),
      // 基准线只挂在第一条 series 上（不然四条会重复画同一组线）
      markLine: idx === 0 ? {
        silent: true, symbol: 'none', animation: false,
        lineStyle: { color: t.split, width: 1, type: 'solid' },
        label: { position: 'start', color: t.text, fontSize: 10, distance: 6 },
        data: marks.map((m) => ({
          yAxis: m.y,
          label: {
            // 0 与最后一条必显，其余按缩放后的间距抽稀（线可能更多，标签不重叠）
            show: m.show,
            // 标签就是原始整数（0/10/50/100/500/1000/5000…），不做 1k/1万 的缩写：
            // 基准线只有几条，直白数字更好对
            formatter: String(m.v),
          },
        })),
      } : undefined,
    })),
  };
}

// renderChart 把数据画进指定宿主（概览/弹窗共用）。重复调用复用实例、整份替换 option。
// chartFor 取（必要时重建）某宿主上的 ECharts 实例。
//
// 为什么要校验 canvas：调用方常用 `host.replaceChildren(占位)` 显示"加载中…"，
// 这会把 ECharts 画出来的 <canvas> 从 DOM 里摘掉；而实例仍指向该宿主、`isDisposed()`
// 也是 false —— 继续 `setOption` 就画在**已脱离文档**的 canvas 上，页面表现为空白。
// 症状就是"第一次打开弹窗有图，之后每次都空白"。这里一旦发现 canvas 不在了就重建实例。
function chartFor(host) {
  let inst = chartInstances.get(host.id);
  const stale = !inst || inst.isDisposed() || inst.getDom() !== host;
  const canvasGone = inst && !inst.isDisposed() && !host.querySelector('canvas');
  if (stale || canvasGone) {
    if (inst) {
      try { inst.dispose(); } catch { /* 已销毁 */ }
    }
    host.replaceChildren(); // 清掉"加载中…"占位，再让 ECharts 挂 canvas
    inst = window.echarts.init(host, null, { renderer: 'canvas' });
    chartInstances.set(host.id, inst);
  }
  return inst;
}

// fitChart 只在宿主已经有真实尺寸时 resize：ECharts 在 display:none / 宽度为 0 时
// init 会把画布固定成 0 或动画中的临时宽度（表现为图例挤成竖排、曲线全挤在左边）；
// 等容器可见后必须再 resize 一次，否则会一直保持错误尺寸。
function fitChart(host, inst) {
  if (!host || !inst || inst.isDisposed()) return;
  if (host.clientWidth < 2 || host.clientHeight < 2) return;
  try { inst.resize(); } catch { /* 实例已销毁 */ }
}

function renderChart(hostId, labels, rows) {
  const host = document.getElementById(hostId);
  if (!host || !window.echarts) return;
  let inst = chartFor(host);
  if (!labels.length) {
    inst.clear();
    return;
  }
  inst.setOption(creditChartOption(labels, rows), true);
  // 宿主可能刚被显示（弹窗动画/切页签）且尺寸还是 0：立即试一次 + 下一帧再试一次，
  // 弹窗动画结束后再兜一次；容器从 hidden 变可见的最终兜底是下面的 ResizeObserver。
  fitChart(host, inst);
  requestAnimationFrame(() => fitChart(host, inst));
  setTimeout(() => fitChart(host, inst), 80);
}

// 容器显隐/布局变化时自动 resize（ECharts 不会跟随 DOM 尺寸）。这是"图挤在左边"
// 的根治手段：无论图表实例是在隐藏状态下初始化，还是在动画中间初始化，只要容器
// 最终拿到了真实宽高，ResizeObserver 就会把画布拉回正确尺寸。
let chartResizeObserver = null;
function observeChartHosts() {
  if (chartResizeObserver || typeof ResizeObserver === 'undefined') return;
  chartResizeObserver = new ResizeObserver((entries) => {
    for (const entry of entries) {
      const host = entry.target;
      fitChart(host, chartInstances.get(host.id));
    }
  });
  for (const id of ['overviewChart', 'creditModalChart']) {
    const host = document.getElementById(id);
    if (host) chartResizeObserver.observe(host);
  }
}

// resizeCharts 窗口尺寸或容器显隐变化时同步（ECharts 不会自动跟随 DOM 尺寸）。
function resizeCharts() {
  for (const [id, inst] of chartInstances) {
    fitChart(document.getElementById(id), inst);
  }
}

// refreshCharts 主题切换后重画（canvas 上的配色不会跟着 CSS 变）：用缓存数据按当前
// 区间重建两个图，避免出现"深色底 + 浅色文字"的残影。
function refreshCharts() {
  if (!state.creditDaily) return;
  if (document.getElementById('overviewChart')) {
    const { labels, rows } = chartRows(state.creditDaily, state.overviewDays || 7, null);
    renderChart('overviewChart', labels, rows);
  }
  const modal = document.getElementById('creditChartModal');
  if (state.chartUID && modal && modal.classList.contains('show')) {
    const { labels, rows } = chartRows(state.creditDaily, state.chartDays || 7, state.chartUID);
    renderChart('creditModalChart', labels, rows);
  }
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
//   - uid == null → 整池：earned/spent/adjust 按天相加，日终总计 = 当天各账号
//     credits_end 之和；
//   - uid 非空 → 单账号：取该账号当天那条记录。
//
// 缺失日期一律按 0（需求：实际只有 3 天数据却看 7 天时，空的那几天用 0 填充）——
// 三条线都不做沿用/插值，图上的空档就是 0。
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
      return { date, earned, spent, adjust, writeoff: spent + adjust, credits };
    }
    const rec = list.find((a) => a.uid === uid);
    const spent = rec ? Number(rec.spent || 0) : 0;
    const adjust = rec ? Number(rec.adjust || 0) : 0;
    return {
      date,
      earned: rec ? Number(rec.earned || 0) : 0,
      spent,
      adjust,
      writeoff: spent + adjust,
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

// renderOverviewChart 概览「积分日账本」曲线：整池三条独立折线（对数刻度轴），7/15/30 可切换。
function renderOverviewChart() {
  const days = state.overviewDays || 7;
  $$('#overviewRange button').forEach((b) => {
    b.classList.toggle('active', Number(b.dataset.days) === days);
  });
  const daysLabel = $('#overviewChartDays');
  if (daysLabel) daysLabel.textContent = String(days);

  const { labels, rows } = chartRows(state.creditDaily, days, null);
  const hint = $('#overviewChartHint');
  if (hint) {
    hint.textContent = labels.length
      ? (labels[0] + ' ~ ' + labels[labels.length - 1] + '（' + labels.length + ' 天）')
      : '';
  }
  renderChart('overviewChart', labels, rows);
}

// renderCreditChart 用当前缓存的账本数据 + 当前区间重画弹窗内容。
function renderCreditChart() {
  const uid = state.chartUID;
  if (!uid) return;
  const days = state.chartDays || 7;
  $$('#creditModalRange button').forEach((b) => {
    b.classList.toggle('active', Number(b.dataset.days) === days);
  });
  const { labels, rows } = chartRows(state.creditDaily, days, uid);
  $('#creditModalHint').textContent = labels.length
    ? (labels[0] + ' ~ ' + labels[labels.length - 1] + '（' + labels.length + ' 天）')
    : '';
  renderChart('creditModalChart', labels, rows);
}

// openCreditChart 打开某账号的积分曲线弹窗（默认近 7 天）。
async function openCreditChart(uid) {
  state.chartUID = uid;
  state.chartDays = 7;
  const acc = (state.accounts || []).find((a) => a.uid === uid);
  $('#creditModalTitle').textContent = '积分曲线 · ' + uid;
  $('#creditModalMeta').textContent = acc
    ? [acc.nickname || '', realmFlag(acc.realm), '当前积分 ' + fmtNum(acc.credits)].filter(Boolean).join(' · ')
    : '';
  const host = $('#creditModalChart');
  // 先丢弃旧实例再清空宿主：留着实例会指向被摘掉的 canvas（见 chartFor 的注释）。
  const stale = chartInstances.get('creditModalChart');
  if (stale) {
    try { stale.dispose(); } catch { /* 已销毁 */ }
    chartInstances.delete('creditModalChart');
  }
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

// catBadge 猫猫状态：来自最近一次旅行巡检的快照（a.cat），不是实时查询。
// 状态与徽章文字的对应关系（与 scheduler 的写入一一对应）：
//   none       未领养         还没领猫（含"领养前置对话量未达/已发起领养"）
//   idle       待派出         猫在空闲、今日还没出行
//   idle       今日已派       今日已出行过（daily_limit_reached=true）
//   traveling  旅行中         已派出，正在路上
//   arrived    可领 N         到站可领 N（领奖失败时也停在这里）
//   arrived    已领奖         兼容旧快照：detail 已写"已领奖"但 state 还是 arrived
//   error      查询失败        buddy-info / travel-status 查询失败或状态未知
// 从没巡检过（没有 a.cat）显示「猫未巡检」：页面打开时不为列表额外请求上游。
function catBadge(a) {
  const c = a.cat;
  if (!c || !c.state) {
    return el('span', {
      class: 'badge cat-badge text-bg-secondary',
      text: '猫未巡检',
      title: '猫状态来自最近一次旅行巡检的快照（不会为列表额外请求上游）；该账号还没巡检过',
    });
  }
  let text = '';
  let cls = 'text-bg-secondary';
  switch (c.state) {
    case 'none':
      text = '未领养';
      break;
    case 'idle':
      text = c.daily_limit_reached ? '今日已派' : '待派出';
      cls = c.daily_limit_reached ? 'text-bg-secondary' : 'text-bg-success';
      break;
    case 'traveling':
      text = '旅行中';
      cls = 'text-bg-info';
      break;
    case 'arrived': {
      // 兼容旧快照：修复前"领奖成功"仍把 state 记成 arrived，界面会显示「可领 N」
      // 但 tooltip 写「已领奖」。看到这种旧数据就按"已领奖"展示，别再诱导去领。
      const claimed = String(c.detail || '').includes('已领奖');
      if (claimed) {
        text = '已领奖';
        cls = 'text-bg-secondary';
      } else {
        text = '可领 ' + fmtNum(c.reward || 0);
        cls = 'text-bg-warning';
      }
      break;
    }
    case 'error':
      text = '查询失败';
      cls = 'text-bg-danger';
      break;
    default:
      text = '未知状态';
  }
  const tip = [
    c.buddy ? '猫：' + c.buddy : '',
    c.detail || '',
    c.updated_at ? '更新于 ' + fmtAgo(c.updated_at) : '',
  ].filter(Boolean).join(' · ');
  return el('span', { class: 'badge cat-badge ' + cls, text: text, title: tip });
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
        el('td', { text: realmFlag(a.realm), title: realmLabel(a.realm) }),
        // 顺序固定：在线状态 → 签到状态 → 领养（猫）状态。
        el('td', {}, [el('div', { class: 'badge-row' }, [
          statusBadge(a),
          a.checked_in_today
            ? el('span', {
              class: 'badge text-bg-success-subtle text-success-emphasis',
              text: '已签',
              title: a.last_checkin_status ? '最近一次：' + a.last_checkin_status : '',
            })
            : el('span', {
              class: 'badge text-bg-secondary-subtle text-secondary-emphasis',
              text: '未签',
              title: '今天还没签到' + (a.last_checkin_status ? '（最近一次：' + a.last_checkin_status + '）' : ''),
            }),
          catBadge(a),
        ].filter(Boolean))]),
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

// ── 接入映射（下游 key 的账号 / 模型白名单）─────────────────────
//
// 数据面鉴权只认这里的 key（config.json 的 api_key 已退役）。页面职责：
//   1. 列出每把 key 的授权范围与状态；
//   2. 新建/编辑：从账号池勾账号、从模型清单勾模型；空列表 = 全部拒绝，
//      所以「全部账号 / 全部模型」必须是显式开关，不能靠空列表暗示；
//   3. key 明文可查看/复制/重新生成（文件本身 0600，与旧 api_key 同口径）。
// 所有渲染仍走 textContent（key 名称、昵称、模型描述都是外部输入）。
let accessModalInst = null;
let accessHideTimer = null;
function accessModal() {
  if (!accessModalInst) accessModalInst = new bootstrap.Modal($('#accessKeyModal'));
  return accessModalInst;
}

async function loadAccessKeys() {
  const body = $('#accessKeysBody');
  body.replaceChildren(el('tr', {}, [el('td', {
    class: 'text-center text-body-secondary py-4', attrs: { colspan: '8' }, text: '加载中…',
  })]));
  try {
    // 账号/模型选择器与列表并行拉取：账号页的元信息（昵称/状态）与模型清单分别来自
    // /admin/accounts、/admin/models；两者失败不应阻断 key 列表本身。
    const [keys, accounts, models] = await Promise.all([
      api('/admin/access-keys'),
      api('/admin/accounts').catch(() => ({ accounts: [] })),
      api('/admin/models').catch(() => ({ data: [] })),
    ]);
    state.accessKeys = keys.keys || [];
    state.accessAccounts = accounts.accounts || [];
    state.accessModels = models.data || [];
    const enabled = state.accessKeys.filter((k) => k.enabled).length;
    $('#accessKeysHint').textContent =
      '共 ' + state.accessKeys.length + ' 把 · 启用 ' + enabled +
      ' · 存储 ' + (keys.path || 'data/access-keys.json');
    renderAccessKeyRows();
  } catch (e) {
    body.replaceChildren(el('tr', {}, [el('td', {
      class: 'text-center text-danger py-4', attrs: { colspan: '8' }, text: String(e.message || e),
    })]));
    $('#accessKeysHint').textContent = '';
  }
}

// realmLabel/modelRealm/accountRealm 三个小工具把「版本」口径收在一处：
// 裸模型名 = cn；账号缺 realm 也按 cn（与后端 SplitModel / auth.Realm 一致）。
function realmLabel(realm) {
  return realm === 'global' ? '全球 (global)' : '中国 (cn)';
}

// 版本在界面上一律只显示 emoji：中国国旗 / 全球地球（不再带 cn/global 文字）。
function realmFlag(realm) {
  return realm === 'global' ? '🌐' : '🇨🇳';
}

function modelRealm(id) {
  const s = String(id || '');
  const i = s.indexOf(':');
  if (i > 0) {
    const p = s.slice(0, i);
    if (p === 'cn' || p === 'global') return p;
  }
  return 'cn';
}

function accountRealm(uid) {
  const a = state.accessAccounts.find((x) => x.uid === uid);
  return a ? (a.realm || 'cn') : '';
}

// pruneAccessDraftForRealm 去掉不属于当前版本的已选项；账号目录未加载时保留
// （否则会把尚未拉到的 uid 误删），模型总能按前缀判断。
function pruneAccessDraftForRealm(d) {
  if (!d) return;
  if (state.accessAccounts.length) {
    d.accounts = new Set([...d.accounts].filter((uid) => {
      const r = accountRealm(uid);
      return !r || r === d.realm;
    }));
  }
  d.models = new Set([...d.models].filter((id) => modelRealm(id) === d.realm));
}

// accessKeyMask 列表页只显示省略后的 key：完整 key 不直接铺在表格里，
// 需要时点它复制（或进编辑弹窗看）。短自定义 key 不强行截断。
function accessKeyMask(k) {
  if (!k.key) return '';
  if (k.key.length <= 12) return k.key;
  return k.key.slice(0, 6) + '…' + k.key.slice(-4);
}

// accessAccountSummary 列表只描述数量（或「全部」），不再列出昵称。
function accessAccountSummary(k) {
  if (k.all_accounts) return '全部';
  const realm = k.realm || 'cn';
  const n = (k.accounts || []).filter((uid) => {
    const r = accountRealm(uid);
    return !r || r === realm;
  }).length;
  return n + ' 个';
}

// accessModelSummary 同上：只给数量或「全部」。
function accessModelSummary(k) {
  if (k.all_models) return '全部';
  const realm = k.realm || 'cn';
  return (k.models || []).filter((id) => modelRealm(id) === realm).length + ' 个';
}

function accessActionBtn(label, kind, id, action) {
  return el('button', {
    class: 'btn btn-sm btn-outline-' + kind + ' me-1',
    text: label,
    attrs: { type: 'button', 'data-ak-id': id, 'data-ak-action': action },
  });
}

function renderAccessKeyRows() {
  const body = $('#accessKeysBody');
  if (state.accessKeys.length === 0) {
    body.replaceChildren(el('tr', {}, [el('td', {
      class: 'text-center text-body-secondary py-4', attrs: { colspan: '8' },
      text: '还没有接入 key：下游现在无法调用 /v1；点「新建接入 Key」创建第一把',
    })]));
    return;
  }
  const rows = state.accessKeys.map((k) => {
    // Key 单元格：省略显示 + 点击即复制完整 key（不再有「查看/复制」两个按钮）。
    // 用 button 承载是为了键盘可达（Tab/Enter），外观由 .access-key-copy 压成纯文本。
    const keyCell = el('td', {}, [el('button', {
      class: 'access-key-copy access-key-mono',
      text: accessKeyMask(k),
      title: '点击复制完整 Key',
      attrs: { type: 'button', 'data-ak-id': k.id, 'data-ak-action': 'copy' },
    })]);
    return el('tr', {}, [
      el('td', { text: k.name || '–', title: k.id }),
      el('td', { class: 'small text-nowrap', text: realmFlag(k.realm), title: realmLabel(k.realm) }),
      keyCell,
      el('td', { class: 'small', text: accessAccountSummary(k) }),
      el('td', { class: 'small', text: accessModelSummary(k) }),
      el('td', {}, [el('span', {
        class: 'badge ' + (k.enabled ? 'text-bg-success' : 'text-bg-secondary'),
        text: k.enabled ? '启用' : '停用',
      })]),
      el('td', { class: 'small text-body-secondary', text: fmtAgo(k.updated_at || k.created_at) || '–' }),
      el('td', { class: 'text-start text-nowrap' }, [
        accessActionBtn('编辑', 'primary', k.id, 'edit'),
        accessActionBtn(k.enabled ? '停用' : '启用', k.enabled ? 'secondary' : 'success', k.id, 'toggle'),
        accessActionBtn('删除', 'danger', k.id, 'delete'),
      ]),
    ]);
  });
  body.replaceChildren(...rows);
}

async function doAccessKeyAction(id, action) {
  const k = state.accessKeys.find((x) => x.id === id);
  if (!k) return;
  try {
    if (action === 'copy') {
      await copyText(k.key);
      return;
    }
    if (action === 'edit') {
      openAccessKeyModal(k);
      return;
    }
    if (action === 'toggle') {
      const next = !k.enabled;
      await api('/admin/access-keys/' + encodeURIComponent(id) + '/' + (next ? 'enable' : 'disable'), {
        method: 'POST', body: {},
      });
      toast(next ? '已启用：该 key 可继续调用' : '已停用：该 key 的下游请求将立即 401', next ? 'success' : 'secondary');
      loadAccessKeys();
      return;
    }
    if (action === 'regenerate') {
      // 兼容旧页面残留的按钮：只把新值写进编辑表单，不直接改服务端 key。
      openAccessKeyModal(k);
      randomizeAccessKeySecret();
      return;
    }
    if (action === 'delete') {
      if (!confirm('删除「' + (k.name || id) + '」？下游用这把 key 的请求会立即 401，且不可恢复。')) return;
      await api('/admin/access-keys/' + encodeURIComponent(id), { method: 'DELETE' });
      toast('已删除接入 key', 'success');
      loadAccessKeys();
    }
  } catch (e) {
    toast(String(e.message || e), 'danger');
  }
}

// copyText 复制到剪贴板：优先 Clipboard API（CSP 下同源页面可用），失败回退 textarea。
async function copyText(text) {
  try {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      await navigator.clipboard.writeText(text);
      toast('已复制完整 Key', 'success');
      return;
    }
  } catch (_) { /* 回退到 execCommand */ }
  const ta = document.createElement('textarea');
  ta.value = text;
  ta.style.position = 'fixed';
  ta.style.opacity = '0';
  document.body.appendChild(ta);
  ta.select();
  let ok = false;
  try { ok = document.execCommand('copy'); } catch (_) { ok = false; }
  ta.remove();
  toast(ok ? '已复制完整 Key' : '复制失败：请手动选中复制', ok ? 'success' : 'warning');
}

function openAccessKeyModal(k) {
  // 取消上一次保存遗留的「过渡结束后补 hide」定时器：否则用户保存后立刻点编辑，
  // 新打开的弹窗会被这个补丁在 400ms 后关掉。
  clearTimeout(accessHideTimer);
  accessHideTimer = null;
  const draft = k
    ? {
      id: k.id, key: k.key, enabled: !!k.enabled, realm: k.realm || 'cn',
      all_accounts: !!k.all_accounts, all_models: !!k.all_models,
      accounts: new Set(k.accounts || []), models: new Set(k.models || []),
    }
    : {
      id: '', key: '', enabled: true, realm: 'cn',
      all_accounts: false, all_models: false,
      accounts: new Set(), models: new Set(),
    };
  // 「全部」是二选一模式：打开时只保留全选标记，不把历史逐项选择一起回显。
  if (draft.all_accounts) draft.accounts.clear();
  if (draft.all_models) draft.models.clear();
  pruneAccessDraftForRealm(draft);
  state.accessDraft = draft;
  $('#accessKeyModalTitle').textContent = k ? '编辑接入 Key' : '新建接入 Key';
  $('#accessKeyModalMeta').textContent = k
    ? 'ID ' + k.id + ' · 创建于 ' + new Date(k.created_at).toLocaleString('zh-CN', { hour12: false }) +
      ' · Key 可直接编辑'
    : 'Key 已预生成，保存后生效；账号/模型留空不会放行，必须显式勾选（或勾「全部」）。';
  $('#akName').value = k ? (k.name || '') : '';
  $('#akRealm').value = draft.realm;
  $('#akEnabled').checked = draft.enabled;
  $('#akAllAccounts').checked = draft.all_accounts;
  $('#akAllModels').checked = draft.all_models;
  // 新增与编辑都展示 Key：新增预生成一个（保存即用），编辑回显现有值。
  $('#akSecretWrap').hidden = false;
  $('#akSecret').readOnly = false;
  $('#akSecret').value = k ? k.key : generateClientSecret();
  $('#akSecret').placeholder = k ? '留空 = 保持原 key 不变' : '可随机生成，留空则由后端生成';
  $('#akAccountFilter').value = '';
  $('#akModelFilter').value = '';
  hideAccessKeyError();
  renderAccessPickers();
  // 账号/模型备选可能在页面加载后才新增（例如刚扫码加号/刷新模型目录）：
  // 弹窗打开时若本地缓存为空，补拉一次，保证“默认就能看到全部备选项”。
  if (state.accessAccounts.length === 0 || state.accessModels.length === 0) {
    refreshAccessOptions();
  }
  showAccessKeyModal();
}

// refreshAccessOptions 重新拉取账号与模型备选；成功后按当前弹窗草稿重绘。
// 失败保持现有缓存（列表里已有内容不应因为一次网络抖动被清空）。
async function refreshAccessOptions() {
  try {
    const [accounts, models] = await Promise.all([
      api('/admin/accounts'),
      api('/admin/models'),
    ]);
    state.accessAccounts = accounts.accounts || [];
    state.accessModels = models.data || [];
    if (state.accessDraft) renderAccessPickers();
  } catch (_) { /* 保持缓存；页面级 loadAccessKeys 会显示错误 */ }
}

function accessPickRow(kind, value, label, sub, checked, muted) {
  const input = el('input', {
    class: 'form-check-input',
    attrs: { type: 'checkbox', 'data-ak-kind': kind, value },
  });
  input.checked = !!checked;
  const text = el('span', { class: 'access-pick-text' }, [
    el('span', { text: label }),
    sub ? el('span', { class: 'access-pick-sub font-monospace', text: sub }) : null,
  ]);
  return el('label', {
    class: 'form-check access-pick' + (muted ? ' text-body-secondary' : ''),
  }, [input, text]);
}

function renderAccessPickers() {
  const d = state.accessDraft;
  if (!d) return;
  const realm = d.realm || 'cn';
  const accAll = state.accessAccounts.filter((a) => (a.realm || 'cn') === realm);
  const modAll = state.accessModels.filter((m) => modelRealm(m.id) === realm);
  // 过滤框留空 = 展示该版本全部备选项；placeholder 带总数。
  $('#akAccountFilter').placeholder =
    '过滤昵称 / UID（' + realmFlag(realm) + ' 共 ' + accAll.length + ' 个）';
  $('#akModelFilter').placeholder =
    '过滤模型 ID / 描述（' + realmFlag(realm) + ' 共 ' + modAll.length + ' 个）';
  const accKw = ($('#akAccountFilter').value || '').trim().toLowerCase();
  const accRows = accAll
    .filter((a) => !accKw ||
      (a.nickname || '').toLowerCase().includes(accKw) ||
      (a.uid || '').toLowerCase().includes(accKw))
    .map((a) => accessPickRow(
      'account', a.uid, a.nickname || a.uid,
      a.uid + ' · ' + realmFlag(a.realm) + (a.disabled ? ' · 已禁用' : ''),
      d.accounts.has(a.uid), a.disabled,
    ));
  $('#akAccountList').replaceChildren(...(accRows.length ? accRows : [el('div', {
    class: 'small text-body-secondary py-2',
    text: '该版本暂无账号（先到「账号」页加号）',
  })]));

  const modKw = ($('#akModelFilter').value || '').trim().toLowerCase();
  const modRows = modAll
    .filter((m) => !modKw ||
      (m.id || '').toLowerCase().includes(modKw) ||
      (m.description || '').toLowerCase().includes(modKw))
    .map((m) => accessPickRow(
      'model', m.id, m.id,
      m.description || '', d.models.has(m.id), false,
    ));
  $('#akModelList').replaceChildren(...(modRows.length ? modRows : [el('div', {
    class: 'small text-body-secondary py-2',
    text: '该版本暂无可用模型（先到「模型」页刷新清单）',
  })]));

  syncAccessPickerDisabled();
  updateAccessSelectionHint();
}

// 「全部账号/全部模型」启用后：候选项全部取消勾选并置灰不可选。
// 注意只 disable 输入，不改 .form-check-input 的 position（Tabler 的勾选图标
// ::before 依赖 relative，改 static 会出现铺满弹窗的巨勾）。
function syncAccessPickerDisabled() {
  const d = state.accessDraft;
  if (!d) return;
  [[$('#akAccountList'), $('#akAllAccounts').checked],
    [$('#akModelList'), $('#akAllModels').checked]].forEach(([host, all]) => {
    host.classList.toggle('is-disabled', !!all);
    host.querySelectorAll('input[data-ak-kind]').forEach((cb) => { cb.disabled = !!all; });
  });
}

function updateAccessSelectionHint() {
  const d = state.accessDraft;
  if (!d) return;
  const acc = $('#akAllAccounts').checked ? '全部账号' : '已选 ' + d.accounts.size + ' 个账号';
  const mod = $('#akAllModels').checked ? '全部模型' : '已选 ' + d.models.size + ' 个模型';
  $('#akSelectionHint').textContent = acc + ' · ' + mod;
}

function showAccessKeyError(msg) {
  const box = $('#akError');
  box.textContent = msg;
  box.classList.remove('d-none');
  box.scrollIntoView({ block: 'nearest' });
}

function hideAccessKeyError() {
  const box = $('#akError');
  box.classList.add('d-none');
  box.textContent = '';
}

// showAccessKeyModal 打开弹窗，并绕开 Bootstrap 的过渡竞态。
// show/hide 在 300ms 过渡期间互相忽略：保存后立刻点「编辑」时，show 会落在上一条
// hide 的过渡里被丢掉（表现：点了编辑没反应）。这里在过渡结束后重试，最多 1s。
function showAccessKeyModal() {
  const host = $('#accessKeyModal');
  const m = accessModal();
  const tryShow = () => {
    if (host.classList.contains('show')) return true;
    if (m._isTransitioning) return false;
    m.show();
    return true;
  };
  if (tryShow()) return;
  let tries = 0;
  const timer = setInterval(() => {
    tries += 1;
    if (tryShow() || tries > 20) clearInterval(timer);
  }, 50);
}

// hideAccessKeyModal 关闭编辑弹窗，并兜住「show 过渡中调 hide 被忽略」的边界。
// Bootstrap 的 Modal 在 show 的 300ms 过渡里会把 hide 当成过渡冲突直接丢掉；
// 用户手速极快（或自动化）时会出现「保存成功但弹窗还开着」。过渡结束后补一次。
function hideAccessKeyModal() {
  const m = accessModal();
  m.hide();
  clearTimeout(accessHideTimer);
  accessHideTimer = setTimeout(() => {
    accessHideTimer = null;
    const host = $('#accessKeyModal');
    if (host && host.classList.contains('show')) m.hide();
  }, 400);
}

// generateClientSecret 生成与后端同规则的 key：sk- + 32 位大小写字母/数字。
// 用 crypto.getRandomValues + 拒绝采样（248 = 62×4），避免 %62 的取模偏差；
// 旧浏览器没有 crypto 时退回 Math.random（仅影响随机质量，不影响格式）。
function generateClientSecret() {
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789';
  const out = ['s', 'k', '-'];
  const c = globalThis.crypto;
  if (c && typeof c.getRandomValues === 'function') {
    while (out.length < 35) {
      const buf = new Uint8Array(64);
      c.getRandomValues(buf);
      for (const b of buf) {
        if (b >= 248) continue;
        out.push(alphabet[b % alphabet.length]);
        if (out.length === 35) break;
      }
    }
  } else {
    while (out.length < 35) out.push(alphabet[Math.floor(Math.random() * alphabet.length)]);
  }
  return out.join('');
}

// randomizeAccessKeySecret 「随机生成」只改表单里的值：不落盘、不调用换 key 接口，
// 也不会弹「旧 key 立即失效」——用户还没点保存。真正生效发生在 saveAccessKey。
function randomizeAccessKeySecret() {
  const input = $('#akSecret');
  input.value = generateClientSecret();
  input.focus();
  input.select();
}

// clearAccessAccounts / clearAccessModels 清空逐项勾选，并关掉对应「全部」开关
// （否则「清空」看起来没有效果：全部仍然放行整池）。
function clearAccessAccounts() {
  const d = state.accessDraft;
  if (!d) return;
  d.accounts.clear();
  d.all_accounts = false;
  $('#akAllAccounts').checked = false;
  renderAccessPickers();
}

function clearAccessModels() {
  const d = state.accessDraft;
  if (!d) return;
  d.models.clear();
  d.all_models = false;
  $('#akAllModels').checked = false;
  renderAccessPickers();
}

async function saveAccessKey() {
  const d = state.accessDraft;
  if (!d) return;
  // 编辑语义：Key 输入框 trim 后非空且与原值不同 = 改 key；留空/原值 = 保持。
  // 新增：表单里预生成了一个 sk- key，非空就随创建一起提交；清空则由后端生成。
  const secret = ($('#akSecret').value || '').trim();
  const secretChanged = !!secret && (!d.id || secret !== d.key);
  if (secretChanged) {
    // 与后端同口径：sk- 前缀 + 32–61 位字母/数字，总长 ≤64。
    if (secret.length > 64 || !/^sk-[A-Za-z0-9]{32,61}$/.test(secret)) {
      showAccessKeyError('Key 必须是 sk- 开头，后接 32–61 位大小写字母或数字，总长度不超过 64。');
      return;
    }
    if (d.id && !confirm('修改 Key 后旧 key 立即失效，下游必须换成新 key。继续？')) return;
  }
  // 版本以当前下拉框为准；保存前再按版本清理一次越界选择（防切版本后残留）。
  d.realm = $('#akRealm').value || d.realm || 'cn';
  pruneAccessDraftForRealm(d);
  const body = {
    name: ($('#akName').value || '').trim(),
    realm: d.realm,
    enabled: $('#akEnabled').checked,
    all_accounts: $('#akAllAccounts').checked,
    all_models: $('#akAllModels').checked,
    accounts: Array.from(d.accounts),
    models: Array.from(d.models),
  };
  if (secretChanged) body.secret = secret;
  // 前端先拦一次：空白名单是「全部拒绝」，直接提交只会拿到 400；这里给出更具体的指引。
  if (!body.all_accounts && body.accounts.length === 0) {
    showAccessKeyError('至少选择一个账号（或勾选「全部账号」）——空列表等于全部拒绝。');
    return;
  }
  if (!body.all_models && body.models.length === 0) {
    showAccessKeyError('至少选择一个模型（或勾选「全部模型」）——空列表等于全部拒绝。');
    return;
  }
  const btn = $('#btnSaveAccessKey');
  btn.disabled = true;
  btn.textContent = '保存中…';
  hideAccessKeyError();
  try {
    if (d.id) {
      await api('/admin/access-keys/' + encodeURIComponent(d.id), { method: 'PUT', body });
      toast(secret && secret !== d.key
        ? '已保存：新 Key 已生效，旧 key 已失效'
        : '已保存：账号/模型授权立即生效', 'success');
    } else {
      const r = await api('/admin/access-keys', { method: 'POST', body });
      toast('已创建接入 key：' + r.key.key, 'success');
    }
    hideAccessKeyModal();
    loadAccessKeys();
  } catch (e) {
    showAccessKeyError(String(e.message || e));
  } finally {
    btn.disabled = false;
    btn.textContent = '保存';
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
    $('#addMeta').textContent = '等待授权…（' + realmFlag(r.realm || realm) + '）';
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
  // ECharts 画在 canvas 上，配色不跟随 CSS：主题切换后按新主题重画（数据取缓存）。
  refreshCharts();
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
    // null（清空 → 用默认）与当前值本来就是空串，视为同一状态：
    // 否则"清空一个本来就空的项"也会变成待保存的改动。
    if (sameValue(out.value, orig) || (configValueEmpty(out.value) && configValueEmpty(orig))) {
      state.configDirty.delete(field.path);
    } else {
      state.configDirty.set(field.path, out.value);
    }
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
      if (!field.secret) return { node: input, read: () => readString(input.value) };
      const btn = el('button', {
        class: 'btn btn-outline-secondary btn-icon',
        attrs: { type: 'button', title: '显示 / 隐藏' },
      }, [icon('ti-eye')]);
      btn.addEventListener('click', () => {
        input.type = input.type === 'password' ? 'text' : 'password';
      });
      const group = el('div', { class: 'input-group input-group-sm' }, [input, btn]);
      return { node: group, read: () => readString(input.value) };
    }
  }
}

// readString 文本字段取值：**先去首尾空白**；清空（或只剩空白）= 未设置 → null。
//
// 为什么发 null 而不是 ""：后端 Save 只对 null 做"删键"（configadmin.go 的 v == nil
// 分支），键删掉后该字段回落 config.default.json 的默认值；而空串会被原样落盘，
// 变成"显式空值"，默认配置再也不会生效。数字字段（readNumber）本来就是这么做的，
// 这里让字符串字段与之对齐。
function readString(raw) {
  const s = String(raw == null ? '' : raw).trim();
  return { value: s === '' ? null : s };
}

// configValueEmpty 值是否等于「未设置」：null/undefined，或去空白后的空串。
function configValueEmpty(v) {
  return v == null || (typeof v === 'string' && v.trim() === '');
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
  if (v == null) return ''; // 留空 = 用默认值（null），没有格式可校验
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
  if (!confirm('把所有配置恢复默认？\n\n注意：监听地址、上游指纹等会重置；管理密码、「启用管理面」与「接入映射」里的 key 保持不变。')) return;
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
  if ($('#btnCreditRefresh')) $('#btnCreditRefresh').addEventListener('click', () => refreshOverview({ forceChart: true }));

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

  // ECharts 不会自动跟随容器尺寸：窗口缩放、弹窗展开（容器从 display:none 变可见）都要手动 resize。
  window.addEventListener('resize', () => resizeCharts());
  const chartModal = $('#creditChartModal');
  if (chartModal) chartModal.addEventListener('shown.bs.modal', () => resizeCharts());
  observeChartHosts();

  $('#btnRefreshStatus').addEventListener('click', () => refreshOverview());
  $('#btnRefreshAccounts').addEventListener('click', loadAccounts);
  $('#btnCheckinAll').addEventListener('click', checkinAll);
  $('#btnRefreshModels').addEventListener('click', () => loadModels(true));
  $('#modelFilter').addEventListener('input', renderModelRows);

  // 接入映射：列表操作走事件委托（行是动态生成的）。
  if ($('#btnAccessKeyNew')) $('#btnAccessKeyNew').addEventListener('click', () => openAccessKeyModal(null));
  if ($('#btnRefreshAccessKeys')) $('#btnRefreshAccessKeys').addEventListener('click', loadAccessKeys);
  if ($('#accessKeysBody')) {
    $('#accessKeysBody').addEventListener('click', (ev) => {
      const btn = ev.target.closest('button[data-ak-action]');
      if (btn) doAccessKeyAction(btn.dataset.akId, btn.dataset.akAction);
    });
  }
  if ($('#akRealm')) {
    $('#akRealm').addEventListener('change', () => {
      const d = state.accessDraft;
      if (!d) return;
      const next = $('#akRealm').value === 'global' ? 'global' : 'cn';
      if (next === d.realm) return;
      // 切版本 = 换一套候选：清空逐项与「全部」标记，避免 cn 的选择残留到 global。
      d.realm = next;
      d.accounts.clear();
      d.models.clear();
      d.all_accounts = false;
      d.all_models = false;
      $('#akAllAccounts').checked = false;
      $('#akAllModels').checked = false;
      renderAccessPickers();
    });
  }
  const onAccessPickChange = (ev) => {
    const cb = ev.target.closest('input[data-ak-kind]');
    const d = state.accessDraft;
    if (!cb || !d) return;
    const isAccount = cb.dataset.akKind === 'account';
    const set = isAccount ? d.accounts : d.models;
    if (cb.checked) {
      // 「全部」与逐项勾选互斥：勾了某一项就自动关掉「全部」，只保留这一项模式。
      if (isAccount && $('#akAllAccounts').checked) {
        $('#akAllAccounts').checked = false;
        d.all_accounts = false;
      }
      if (!isAccount && $('#akAllModels').checked) {
        $('#akAllModels').checked = false;
        d.all_models = false;
      }
      set.add(cb.value);
    } else {
      set.delete(cb.value);
    }
    updateAccessSelectionHint();
  };
  if ($('#akAccountList')) $('#akAccountList').addEventListener('change', onAccessPickChange);
  if ($('#akModelList')) $('#akModelList').addEventListener('change', onAccessPickChange);
  if ($('#akAccountFilter')) $('#akAccountFilter').addEventListener('input', renderAccessPickers);
  if ($('#akModelFilter')) $('#akModelFilter').addEventListener('input', renderAccessPickers);
  if ($('#akAllAccounts')) {
    $('#akAllAccounts').addEventListener('change', () => {
      const d = state.accessDraft;
      if (!d) return;
      d.all_accounts = $('#akAllAccounts').checked;
      // 全选启用 = 只保留「全部账号」标记：清空逐项勾选，避免表单里两套状态并存。
      if (d.all_accounts) d.accounts.clear();
      renderAccessPickers();
    });
  }
  if ($('#akAllModels')) {
    $('#akAllModels').addEventListener('change', () => {
      const d = state.accessDraft;
      if (!d) return;
      d.all_models = $('#akAllModels').checked;
      if (d.all_models) d.models.clear();
      renderAccessPickers();
    });
  }
  if ($('#btnClearAccessAccounts')) $('#btnClearAccessAccounts').addEventListener('click', clearAccessAccounts);
  if ($('#btnClearAccessModels')) $('#btnClearAccessModels').addEventListener('click', clearAccessModels);
  if ($('#btnSaveAccessKey')) $('#btnSaveAccessKey').addEventListener('click', saveAccessKey);
  if ($('#btnCopyAccessKey')) $('#btnCopyAccessKey').addEventListener('click', () => copyText($('#akSecret').value));
  if ($('#btnRegenerateAccessKey')) $('#btnRegenerateAccessKey').addEventListener('click', randomizeAccessKeySecret);

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
  if ($('#btnApplyUpdate')) $('#btnApplyUpdate').addEventListener('click', applyFrontendUpdate);
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
  // 标签页切回前台时立刻补一次（轮询在后台是暂停的，不必等下一个 5s）。
  document.addEventListener('visibilitychange', () => {
    if (!document.hidden && state.password && state.view === 'overview') refreshOverview();
  });

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

// ── 前端版本 ─────────────────────────────────────────────────────
// /assets/version.json 永远 no-store，值是全部前端资源的内容哈希；资源 URL 也带
// 同一个 ?v=，所以"版本变了"等价于"服务端换了整套前端"。长驻页面定期查它：
// 发现本地版本落后就提示用户重新登录 + 清缓存 + 回源，避免新旧 JS 混用。
// PAGE_VERSION 是"当前页面实际加载到的前端版本"（index.html 里的 meta，服务端替换）。
// 用它区分两种情况：正常刷新后页面已经是新版（只需把本地记录补上），
// 以及长驻页面仍是旧版（服务端 version.json 已经变了，需要提示更新）。
function pageVersion() {
  const m = document.querySelector('meta[name="wp-frontend-version"]');
  return (m && m.content) || '';
}

async function checkFrontendVersion() {
  try {
    const mine = pageVersion();
    if (!mine) return;
    // 页面本身就是最新版：把本地记录对齐（避免"刚刷新完又提示更新"的假警报）。
    if (localStorage.getItem(VERSION_STORE) !== mine) {
      localStorage.setItem(VERSION_STORE, mine);
    }
    const resp = await fetch('/assets/version.json', { cache: 'no-store' });
    if (!resp.ok) return;
    const data = await resp.json();
    const latest = String(data.version || '');
    if (!latest) return;
    const box = $('#versionBanner');
    if (!box) return;
    if (latest === mine) {
      box.classList.add('d-none');
      return;
    }
    const label = $('#versionBannerNew');
    if (label) label.textContent = latest;
    box.classList.remove('d-none');
  } catch {
    /* 版本探针失败不影响使用（离线/接口尚未接线） */
  }
}

// applyFrontendUpdate 登出 + 清缓存 + 带一次性参数回源，拿到新版本的全部资源。
async function applyFrontendUpdate() {
  try {
    if (window.caches) {
      const keys = await caches.keys();
      await Promise.all(keys.map((k) => caches.delete(k)));
    }
  } catch {
    /* Cache Storage 不可用时忽略 */
  }
  clearSession();
  localStorage.removeItem(VERSION_STORE);
  stopOverviewPolling();
  const u = new URL('/', location.origin);
  u.searchParams.set('updated', String(Date.now()));
  location.replace(u.toString());
}

// boot：有本地密码就**立即**渲染主界面（数据请求自带凭据，失效会 401 回登录页），
// 不再等一次网络往返、也不再画加载遮罩 —— 刷新时不会闪登录页或 Logo。
// 没有密码才去问"是否已初始化"，决定显示登录还是初始化向导。
(async function boot() {
  // 标记：app.js 已执行并接管页面（自动化脚本据此判断可交互，见 scripts/ui-login-check.mjs）。
  document.documentElement.dataset.appBooted = '1';
  applyTheme(localStorage.getItem(THEME_STORE) || (matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light'));
  bind();

  checkFrontendVersion();
  setInterval(checkFrontendVersion, VERSION_POLL_MS);
  document.addEventListener('visibilitychange', () => {
    if (!document.hidden) checkFrontendVersion();
  });

  if (state.password) {
    showApp();
    render();
    refreshConfigMeta();
    try {
      await login(state.password);
    } catch (e) {
      clearSession();
      if (String(e.message || '').includes('尚未初始化')) {
        showSetup(8);
        return;
      }
      showLogin('登录失败：' + String(e.message || e));
    }
    return;
  }

  const st = await fetchSetupStatus();
  if (st && st.initialized === false) {
    showSetup(st.min_password_length || 8);
    return;
  }
  if (st && st.admin_enabled === false) {
    showLogin('管理面已关闭：在 data/config.json 里把 admin.enabled 改回 true 后重启容器');
    return;
  }
  showLogin(null);
})();
