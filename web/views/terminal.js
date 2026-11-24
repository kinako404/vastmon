import { h, mount, errorBox, loading, empty } from '../lib/dom.js';
import { get, currentSession, loginHash } from '../lib/api.js';
import { openDialog } from '../lib/dialog.js';
import { fmtGeo } from '../lib/geo.js';
import { createFileBrowser } from '../lib/filebrowser.js';

// The terminal page keeps its tabs alive across page changes: the module
// holds them, and the view only re-attaches the root element. Each tab is
// one instance: an xterm.js shell over a WebSocket on the left and a file
// browser on the right.

let xtermMods = null;
function loadXterm() {
  if (!xtermMods) {
    if (!document.querySelector('link[data-xterm]')) {
      document.head.appendChild(h('link', { rel: 'stylesheet', href: 'vendor/xterm/xterm.css', 'data-xterm': '1' }));
    }
    xtermMods = Promise.all([import('../vendor/xterm/xterm.mjs'), import('../vendor/xterm/addon-fit.mjs')]);
    xtermMods.catch(() => { xtermMods = null; });
  }
  return xtermMods;
}

const THEME = {
  background: '#11141a', foreground: '#d8dde6', cursor: '#d8dde6', selectionBackground: '#3a4a66',
  black: '#1d2330', red: '#ef7d71', green: '#7bc47f', yellow: '#e0a64a', blue: '#6c9cf0',
  magenta: '#c792ea', cyan: '#5fc9d3', white: '#d8dde6',
  brightBlack: '#6b7280', brightRed: '#ff9b90', brightGreen: '#9be09e', brightYellow: '#f3c06f',
  brightBlue: '#8fb5ff', brightMagenta: '#ddb0ff', brightCyan: '#82e0e8', brightWhite: '#ffffff',
};

const tabs = [];
let active = null;
let seq = 0;
let root = null;
let tabBar = null;
let body = null;

function wsURL(instanceId, cols, rows) {
  const u = new URL(`api/term/${instanceId}/ws`, location.href);
  u.protocol = u.protocol === 'https:' ? 'wss:' : 'ws:';
  u.search = `?cols=${cols}&rows=${rows}`;
  u.hash = '';
  return u.toString();
}

function renderTabs() {
  mount(tabBar,
    tabs.map((t) => h('div', {
      class: t === active ? 'term-tab active' : 'term-tab',
      title: `${t.title}\n${t.sub}`,
      onclick: () => activate(t),
      onauxclick: (e) => { if (e.button === 1) closeTab(t); },
    },
    h('span', { class: `term-dot ${t.state}` }),
    h('span', { class: 'term-tab-title' }, t.title),
    h('button', {
      type: 'button', class: 'term-tab-close', title: '关闭这个窗口',
      onclick: (e) => { e.stopPropagation(); closeTab(t); },
    }, '×'))),
    h('button', { type: 'button', class: 'term-new', title: '新建窗口', onclick: () => pickInstance() }, '+ 新建窗口'));
  if (!tabs.length) {
    mount(body, h('div', { class: 'term-empty' },
      empty('还没有打开的终端。'),
      h('button', { type: 'button', class: 'primary', onclick: () => pickInstance() }, '+ 新建窗口')));
  } else if (body.querySelector('.term-empty')) {
    body.replaceChildren(...tabs.map((t) => t.el));
  }
}

function activate(t) {
  active = t;
  for (const x of tabs) x.el.hidden = x !== t;
  renderTabs();
  requestAnimationFrame(() => {
    fitTab(t);
    if (t.term) t.term.focus();
  });
}

function fitTab(t) {
  if (!t.fit || !t.el.isConnected || t.el.hidden) return;
  try {
    t.fit.fit();
  } catch {
    // not laid out yet
  }
}

function closeTab(t) {
  if (t.state === 'on' && !window.confirm(`关闭 ${t.title} 的终端？正在跑的前台命令会被中断。`)) return;
  t.closing = true;
  if (t.ws) t.ws.close();
  if (t.term) t.term.dispose();
  if (t.ro) t.ro.disconnect();
  t.el.remove();
  const i = tabs.indexOf(t);
  tabs.splice(i, 1);
  if (active === t) active = tabs[Math.max(0, i - 1)] || null;
  if (active) activate(active);
  else renderTabs();
}

function setState(t, state, note) {
  t.state = state;
  t.latency = h('span', { class: 'term-latency' });
  mount(t.status, h('span', { class: `term-dot ${state}` }), ' ', note, state === 'on' ? t.latency : null);
  renderTabs();
}

// showLatency shows the panel server → instance SSH round trip.
function showLatency(t, ms) {
  if (!t.latency) return;
  const cls = ms < 100 ? 'cost-ok' : ms < 250 ? 'cost-warn' : 'cost-bad';
  t.latency.className = 'term-latency ' + cls;
  t.latency.title = '面板服务器到这台实例的 SSH 往返延迟，每 3 秒测一次';
  t.latency.textContent = `延迟 ${ms < 10 ? ms.toFixed(1) : Math.round(ms)} ms`;
}

function connect(t) {
  if (t.ws) t.ws.close();
  setState(t, 'wait', '连接中…');
  const ws = new WebSocket(wsURL(t.instanceId, t.term.cols, t.term.rows));
  ws.binaryType = 'arraybuffer';
  t.ws = ws;
  let opened = false;
  ws.onopen = () => { opened = true; };
  ws.onmessage = (ev) => {
    if (typeof ev.data !== 'string') {
      t.term.write(new Uint8Array(ev.data));
      return;
    }
    let m = {};
    try {
      m = JSON.parse(ev.data);
    } catch {
      return;
    }
    if (m.type === 'ready') {
      setState(t, 'on', `已连接 ${t.sub}`);
      sendResize(t);
    } else if (m.type === 'latency') {
      showLatency(t, m.ms);
    } else if (m.type === 'exit') {
      t.exitMsg = m.msg;
    }
  };
  ws.onclose = async () => {
    if (t.ws !== ws || t.closing) return;
    t.ws = null;
    // a full-screen program (nvtop, htop, vim) may have been running: leave
    // its alternate screen, scroll region and mouse / key modes so the
    // message and the next shell show normally; the scrollback stays.
    leaveFullScreen(t);
    let msg = t.exitMsg || '连接断开';
    t.exitMsg = '';
    if (!opened) msg = await whyNoSocket(t);
    setState(t, 'off', `${msg} · 在终端里按回车重连`);
    t.term.write(`\r\n\x1b[33m[${msg}，按回车重连]\x1b[0m\r\n`);
  };
}

// whyNoSocket runs when the WebSocket closed before it ever opened: the
// browser does not say why, so ask the server over plain HTTP whether SSH
// to the instance works. If it does, a CDN / reverse proxy in front of the
// panel is dropping WebSocket upgrades.
async function whyNoSocket(t) {
  setState(t, 'wait', '连不上，检查原因…');
  try {
    const r = await get(`term/${t.instanceId}/check`);
    if (r.ok) {
      return `面板到实例的 SSH 正常（${Math.round(r.ms)} ms），是浏览器到面板的 WebSocket 连不上：${location.host} 前面的 CDN / 反向代理没放行 WebSocket（要转发 Upgrade、Connection 头）`;
    }
    return `面板 SSH 连不上实例：${r.error}`;
  } catch (err) {
    return `连接断开（检查原因也失败：${err.message}）`;
  }
}

// MODES_OFF turns off colours, hidden cursor, application cursor keys /
// keypad, mouse tracking and bracketed paste.
const MODES_OFF = '\x1b[0m\x1b[?25h\x1b[?1l\x1b>\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?2004l';

// leaveFullScreen undoes what a full-screen program leaves behind. The
// alternate screen is left only when it is active (leaving it restores the
// cursor saved on entry); resetting the scroll region homes the cursor, so
// it is wrapped in save / restore cursor.
function leaveFullScreen(t) {
  const alt = t.term.buffer.active.type === 'alternate';
  t.term.write((alt ? '\x1b[r\x1b[?1049l' : '') + '\x1b7\x1b[r\x1b8' + MODES_OFF);
}

// clearTerm wipes the terminal (screen, scrollback and modes); when
// connected, Ctrl+L makes the shell (or a full-screen program) redraw.
function clearTerm(t) {
  if (!t.term) return;
  t.term.reset();
  if (t.ws && t.ws.readyState === WebSocket.OPEN) t.ws.send(new TextEncoder().encode('\x0c'));
  t.term.focus();
}

function sendResize(t) {
  if (t.ws && t.ws.readyState === WebSocket.OPEN) {
    t.ws.send(JSON.stringify({ type: 'resize', cols: t.term.cols, rows: t.term.rows }));
  }
}

// typeCommand sends text as if typed into tab t's shell.
function typeCommand(t, text) {
  if (t.ws && t.ws.readyState === WebSocket.OPEN) {
    t.ws.send(new TextEncoder().encode(text));
    t.term.focus();
  }
}

function shellQuote(p) {
  return `'${p.replace(/'/g, `'\\''`)}'`;
}

async function openTab(x) {
  const t = {
    id: ++seq, instanceId: x.id, state: 'wait',
    title: `${x.label ? x.label + ' · ' : ''}${x.id}`,
    sub: `${x.num_gpus}× ${x.gpu_name} · ${fmtGeo(x.geolocation)}`,
  };
  const shellBox = h('div', { class: 'term-xterm' });
  t.status = h('div', { class: 'term-status' });
  const files = createFileBrowser(x.id, {
    onCd: (dir) => typeCommand(t, `cd ${shellQuote(dir)}\r`),
  });
  const clearBtn = h('button', {
    type: 'button', class: 'term-clear',
    title: '清掉终端里的内容和残留的显示状态（比如断线后 nvtop、htop 的残影）；连着时让 shell 重新显示提示符',
    onclick: () => clearTerm(t),
  }, '清屏');
  const shellPane = h('div', { class: 'term-shell' }, shellBox, h('div', { class: 'term-bar' }, t.status, clearBtn));
  const splitter = h('div', { class: 'term-splitter', title: '拖动调整宽度' });
  const filePane = h('div', { class: 'term-files' }, files.el);
  t.el = h('div', { class: 'term-pane' }, shellPane, splitter, filePane);
  splitter.addEventListener('pointerdown', (e) => {
    e.preventDefault();
    splitter.setPointerCapture(e.pointerId);
    const box = t.el.getBoundingClientRect();
    const move = (ev) => {
      const frac = Math.min(0.85, Math.max(0.25, (ev.clientX - box.left) / box.width));
      shellPane.style.flexBasis = `${frac * 100}%`;
    };
    const up = () => {
      splitter.removeEventListener('pointermove', move);
      splitter.removeEventListener('pointerup', up);
      fitTab(t);
    };
    splitter.addEventListener('pointermove', move);
    splitter.addEventListener('pointerup', up);
  });
  tabs.push(t);
  if (body.querySelector('.term-empty')) body.replaceChildren();
  body.appendChild(t.el);
  activate(t);
  mount(t.status, loading());
  const [{ Terminal }, { FitAddon }] = await loadXterm();
  if (t.closing) return;
  t.term = new Terminal({
    cursorBlink: true, fontSize: 14, scrollback: 10000, theme: THEME, allowProposedApi: false,
    fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Consolas, "Noto Sans Mono CJK SC", monospace',
  });
  t.fit = new FitAddon();
  t.term.loadAddon(t.fit);
  t.term.open(shellBox);
  fitTab(t);
  t.term.onData((d) => {
    if (t.ws && t.ws.readyState === WebSocket.OPEN) t.ws.send(new TextEncoder().encode(d));
    else if (!t.ws && d === '\r') connect(t);
  });
  t.term.onResize(() => sendResize(t));
  t.ro = new ResizeObserver(() => fitTab(t));
  t.ro.observe(shellBox);
  connect(t);
  files.load();
  t.term.focus();
}

// pickInstance asks which running instance to open a new window on; 刷新
// reloads the list (an instance that just started shows up).
async function pickInstance() {
  const d = openDialog();
  const refresh = h('button', { type: 'button', title: '重新读取实例列表' }, '刷新');
  const close = h('button', { type: 'button', onclick: d.close }, '取消');
  const tools = h('div', { class: 'toolbar end' }, refresh, close);
  async function load() {
    refresh.disabled = true;
    mount(d.body, h('h2', null, '新建窗口：选一台机器'), loading(), tools);
    try {
      const res = await get('instances');
      const running = res.instances.filter((x) => x.actual_status === 'running');
      mount(d.body,
        h('h2', null, '新建窗口：选一台机器'),
        running.length
          ? h('ul', { class: 'plain pick-list' }, running.map((x) => h('li', null, h('button', {
            type: 'button', class: 'pick',
            onclick: () => {
              d.close();
              openTab(x);
            },
          },
          h('span', { class: 'strong' }, `${x.label ? x.label + ' · ' : ''}实例 ${x.id}`),
          h('span', { class: 'hint' }, ` ${x.num_gpus}× ${x.gpu_name} · ${fmtGeo(x.geolocation)} · 机器 ${x.machine_id}`)))))
          : empty('没有运行中的实例'),
        res.instances.length > running.length
          ? h('p', { class: 'hint' }, `另有 ${res.instances.length - running.length} 台没在运行，开不了终端。`) : null,
        tools);
    } catch (err) {
      mount(d.body, h('h2', null, '新建窗口'), errorBox(err), tools);
    } finally {
      refresh.disabled = false;
    }
  }
  refresh.addEventListener('click', load);
  load();
}

export async function terminalView(ctx) {
  if (!currentSession().logged_in) {
    ctx.show(h('h1', null, '终端'), h('p', null, '需要先', h('a', { href: loginHash('#/terminal') }, '登录'), '。'));
    return;
  }
  if (!root) {
    tabBar = h('div', { class: 'term-tabs' });
    body = h('div', { class: 'term-body' });
    root = h('div', { class: 'term-page' }, tabBar, body);
    renderTabs();
  }
  ctx.show(root);
  document.body.classList.add('full-page');
  ctx.signal.addEventListener('abort', () => document.body.classList.remove('full-page'));
  const want = Number(ctx.query.get('instance'));
  if (want) {
    const open = tabs.find((t) => t.instanceId === want);
    if (open) activate(open);
    else {
      try {
        const res = await get('instances', ctx.signal);
        const x = res.instances.find((v) => v.id === want);
        if (x) openTab(x);
      } catch {
        // the picker is still there
      }
    }
    history.replaceState(null, '', '#/terminal');
  } else if (active) {
    activate(active);
  }
}

// closeAllTerminals drops every tab, as on logout.
export function closeAllTerminals() {
  for (const t of [...tabs]) {
    t.closing = true;
    if (t.ws) t.ws.close();
    if (t.term) t.term.dispose();
    if (t.ro) t.ro.disconnect();
  }
  tabs.length = 0;
  active = null;
  root = null;
}
