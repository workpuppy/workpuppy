// ui-login-check.mjs —— 用真浏览器（Edge/Chrome 的 CDP）跑管理后台的登录流程回归。
//
// 为什么需要它：管理后台的"登录后跳转"是纯前端行为（app.js 切 hidden + hash 路由），
// Go 单测只能断言静态资源内容与加载顺序，抓不到"CSS 优先级把 hidden 盖掉"这类问题
// ——线上就踩过一次：密码正确、/admin/status 200，页面却停在登录页。
//
// 用法（需要本机有 Edge 或 Chrome + Node，无需 npm 依赖）：
//
//   1) 起一个开了管理面的服务（任意方式），记下端口与日志里的管理密码：
//        ./wp -config config.json        # admin.enabled=true
//   2) 起浏览器（headless + CDP）：
//        msedge --headless=new --remote-debugging-port=9222 \
//               --user-data-dir=/tmp/pw-prof about:blank
//   3) 跑检查：
//        WP_PORT=7863 WP_PW=<日志里的登录密码> node scripts/ui-login-check.mjs
//
// 断言：① 登录页可见、主界面隐藏、app.css 已加载；② 提交正确密码后主界面可见、
// 登录页隐藏、密码已存 localStorage、概览已渲染；③ 带密码请求 /admin/status = 200。
//
// 不用 npm 包：直接连 Chrome DevTools Protocol（Node 自带 fetch/WebSocket）。
// 做法：先用一次 evaluate 把"提交 + 轮询结果"挂到 window.__check 上（fire-and-forget），
// 再由 Node 侧短轮询读取——避免长任务横跨页面上下文切换（hash 路由会让 CDP 报
// "Inspected target navigated"）。
const PORT = process.env.WP_PORT;
const PW = process.env.WP_PW;
// CDP 端口可覆盖（默认 9222）：便于本机同时跑多个实例，或在 CI 上换端口。
const CDP = process.env.WP_CDP || 'http://127.0.0.1:9222';
const base = `http://127.0.0.1:${PORT}/`;

function connect(url) {
  const ws = new WebSocket(url);
  let id = 0;
  const pending = new Map();
  ws.addEventListener('message', (ev) => {
    const msg = JSON.parse(ev.data);
    if (msg.id && pending.has(msg.id)) {
      const { resolve, reject } = pending.get(msg.id);
      pending.delete(msg.id);
      msg.error ? reject(new Error(JSON.stringify(msg.error))) : resolve(msg.result);
    }
  });
  const ready = new Promise((res) => ws.addEventListener('open', res));
  const send = (method, params = {}) =>
    new Promise((resolve, reject) => {
      const mid = ++id;
      pending.set(mid, { resolve, reject });
      ws.send(JSON.stringify({ id: mid, method, params }));
    });
  return { ready, send, close: () => ws.close() };
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function main() {
  const target = await (await fetch(`${CDP}/json/new?${encodeURIComponent(base)}`, { method: 'PUT' })).json();
  const c = connect(target.webSocketDebuggerUrl);
  await c.ready;
  await c.send('Runtime.enable');

  const evalJS = async (expression) => {
    const r = await c.send('Runtime.evaluate', { expression, awaitPromise: false, returnByValue: true });
    if (r.exceptionDetails) throw new Error('JS 异常: ' + JSON.stringify(r.exceptionDetails.exception));
    return r.result.value;
  };

  // 等"页面真的可交互"：DOM + app.css 已加载 + app.js 已执行
  // （app.js 末尾的 boot() 会立刻设置 data-bs-theme，是最可靠的就绪标记）
  let ready = false;
  for (let i = 0; i < 50; i++) {
    ready = await evalJS(`(() => {
      const hasDom = !!document.getElementById('loginPassword');
      const booted = document.documentElement.dataset.appBooted === '1';
      const css = [...document.styleSheets].some(s => (s.href || '').endsWith('app.css'));
      return hasDom && booted && css;
    })()`);
    if (ready) break;
    await sleep(200);
  }
  if (!ready) throw new Error('页面 10s 内未就绪（DOM/app.css/app.js）');

  const before = await evalJS(`(() => {
    const login = document.getElementById('loginWrap'), app = document.getElementById('app');
    return {
      loginVisible: login.offsetParent !== null,
      appVisible: app.offsetParent !== null,
      cssLoaded: [...document.styleSheets].some(s => (s.href || '').includes('app.css')),
    };
  })()`);
  console.log('登录前:', JSON.stringify(before));

  // 首屏可能是"登录"，也可能是"首次初始化密码"（全新部署、data/config.json 里还没有
  // admin.password）。两种都覆盖：脚本是回归用例，不该假设部署已经初始化过。
  const mode = await evalJS(`(() => ({
    setup: !document.getElementById('setupForm')?.hidden,
    login: !document.getElementById('loginForm')?.hidden,
  }))()`);
  console.log('首屏模式:', JSON.stringify(mode));

  // 提交（fire-and-forget，结果写进 window.__check）
  await evalJS(`(() => {
    window.__check = { done: false };
    const t0 = Date.now();
    const login = document.getElementById('loginWrap');
    const app = document.getElementById('app');
    const setup = ${JSON.stringify(!!mode.setup)};
    if (setup) {
      document.getElementById('setupPassword').value = ${JSON.stringify(PW)};
      document.getElementById('setupConfirm').value = ${JSON.stringify(PW)};
      document.getElementById('setupForm').requestSubmit();
    } else {
      document.getElementById('loginPassword').value = ${JSON.stringify(PW)};
      document.getElementById('loginForm').requestSubmit();
    }
    const errText = () => {
      const a = document.getElementById('loginError');
      const b = document.getElementById('setupError');
      return [a, b].filter((n) => n && !n.classList.contains('d-none')).map((n) => n.textContent).join(' / ');
    };
    const tick = () => {
      const appVisible = app.offsetParent !== null;
      const loginHidden = login.offsetParent === null;
      if (appVisible && loginHidden) {
        window.__check = {
          done: true, ok: true, ms: Date.now() - t0, appVisible, loginHidden, setup,
          stored: !!localStorage.getItem('wp.adminPassword'),
          loginErr: errText(),
          overviewText: (document.getElementById('view-overview').offsetParent !== null
            ? document.getElementById('view-overview').innerText : '').slice(0, 80),
        };
        return;
      }
      if (Date.now() - t0 > 8000) {
        window.__check = {
          done: true, ok: false, appVisible, loginHidden, setup,
          stored: !!localStorage.getItem('wp.adminPassword'),
          loginErr: errText(),
        };
        return;
      }
      setTimeout(tick, 100);
    };
    setTimeout(tick, 100);
    return true;
  })()`);

  let after = null;
  for (let i = 0; i < 60; i++) {
    await sleep(200);
    try {
      const r = await evalJS(`window.__check || { done: false }`);
      if (r && r.done) {
        after = r;
        break;
      }
    } catch (e) {
      /* 上下文切换瞬间，重试即可 */
    }
  }
  console.log('登录后:', JSON.stringify(after));

  // 这个探针需要 await Promise，单独开一次 awaitPromise 求值
  const probeResp = await c.send('Runtime.evaluate', {
    expression: `(async () => {
      const pw = localStorage.getItem('wp.adminPassword');
      const r = await fetch('/admin/status', { headers: { Authorization: 'Bearer ' + pw } });
      return JSON.stringify({ status: r.status });
    })()`,
    awaitPromise: true,
    returnByValue: true,
  });
  const probe = probeResp.exceptionDetails
    ? { status: 'err' }
    : JSON.parse(probeResp.result.value);
  console.log('接口:', JSON.stringify(probe));

  c.close();
  const pass =
    before.loginVisible && !before.appVisible && before.cssLoaded &&
    after && after.ok && probe && probe.status === 200;
  console.log(pass ? 'RESULT: PASS' : 'RESULT: FAIL');
  process.exit(pass ? 0 : 1);
}

main().catch((e) => {
  console.error('ERROR:', e.message);
  process.exit(1);
});
