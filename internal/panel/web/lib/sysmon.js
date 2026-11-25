import { h, mount, errorBox } from './dom.js';
import { get } from './api.js';

// createSysMonitor is the system monitor column of a terminal tab: uptime
// and load, CPU / memory / swap bars, the busiest processes, network speed
// and SSH latency charts, and disk space. It asks the panel every 2 seconds
// while it is on screen; the panel keeps the history, so a monitor that was
// hidden for a while catches up on the samples it missed.

const POLL_MS = 2000;
const SHOW_PROCS = 6;

// short formats bytes compactly: 36K, 2.4M, 109G.
export function short(n) {
  if (!Number.isFinite(n) || n <= 0) return '0';
  const u = ['B', 'K', 'M', 'G', 'T', 'P'];
  let i = 0;
  let v = n;
  while (v >= 1024 && i < u.length - 1) {
    v /= 1024;
    i++;
  }
  if (!i) return `${Math.round(v)}B`;
  return `${v < 10 ? v.toFixed(1) : Math.round(v)}${u[i]}`;
}

function fmtUptime(sec) {
  if (!sec) return '—';
  const d = Math.floor(sec / 86400);
  const hr = Math.floor((sec % 86400) / 3600);
  const m = Math.floor((sec % 3600) / 60);
  if (d) return `${d} 天 ${hr} 小时`;
  if (hr) return `${hr} 小时 ${m} 分`;
  return `${m} 分钟`;
}

const fmtMs = (ms) => `${ms < 10 ? ms.toFixed(1) : Math.round(ms)}ms`;
const pct = (used, total) => (total > 0 ? Math.min(100, (used / total) * 100) : 0);

function bar() {
  const fill = h('div', { class: 'mon-fill' });
  const left = h('span');
  const right = h('span');
  const el = h('div', { class: 'mon-bar' }, fill, left, right);
  return {
    el,
    set(p, l, r, title) {
      fill.style.width = `${p}%`;
      fill.className = 'mon-fill' + (p >= 90 ? ' bad' : p >= 70 ? ' warn' : '');
      left.textContent = l;
      right.textContent = r;
      el.title = title || '';
    },
  };
}

// shade paints a bar across the right part of a table cell.
function shade(cell, frac, color) {
  const p = Math.round(Math.max(0, Math.min(1, frac)) * 100);
  cell.style.background = p ? `linear-gradient(to left, color-mix(in srgb, var(${color}) 30%, transparent) ${p}%, transparent ${p}%)` : '';
  return cell;
}

// drawChart draws series (oldest first, newest at the right edge) on canvas.
// Each series is {values, color, line}: bars by default, a line when line is
// set; a null value is a gap. The scale runs from 0 to the largest value on
// show (at least floor), with labels at 1/3, 2/3 and the top.
function drawChart(canvas, series, { floor, fmt }) {
  const w = canvas.clientWidth;
  const ht = canvas.clientHeight;
  if (!w || !ht) return;
  const dpr = window.devicePixelRatio || 1;
  if (canvas.width !== Math.round(w * dpr) || canvas.height !== Math.round(ht * dpr)) {
    canvas.width = Math.round(w * dpr);
    canvas.height = Math.round(ht * dpr);
  }
  const css = getComputedStyle(canvas);
  const color = (name) => css.getPropertyValue(name).trim() || '#888';
  const g = canvas.getContext('2d');
  g.setTransform(dpr, 0, 0, dpr, 0, 0);
  g.clearRect(0, 0, w, ht);
  const gutter = 34;
  const step = 3;
  const top = 6;
  const plotH = ht - top - 2;
  const fit = Math.max(1, Math.floor((w - gutter) / step));
  const shown = series.map((s) => ({ ...s, values: s.values.slice(-fit) }));
  let peak = floor;
  for (const s of shown) for (const v of s.values) if (v !== null && v > peak) peak = v;
  const y = (v) => top + plotH - (v / peak) * plotH;

  g.font = '10px system-ui, sans-serif';
  g.textBaseline = 'middle';
  g.fillStyle = color('--muted');
  g.strokeStyle = color('--border');
  g.lineWidth = 1;
  for (const f of [1, 2 / 3, 1 / 3]) {
    const yy = Math.round(y(peak * f)) + 0.5;
    g.fillText(fmt(peak * f), 0, yy);
    g.beginPath();
    g.moveTo(gutter, yy);
    g.lineTo(w, yy);
    g.stroke();
  }
  g.beginPath();
  g.moveTo(gutter, ht - 1.5);
  g.lineTo(w, ht - 1.5);
  g.stroke();

  const n = Math.max(...shown.map((s) => s.values.length), 0);
  const x = (i) => w - (n - i) * step;
  const lines = shown.filter((s) => s.line);
  const bars = shown.filter((s) => !s.line);
  for (let i = 0; i < n; i++) {
    // taller bar first, so the shorter one stays visible on top of it
    const at = bars.map((s) => ({ v: s.values[i - (n - s.values.length)], c: s.color }))
      .filter((b) => b.v > 0).sort((a, b) => b.v - a.v);
    for (const b of at) {
      g.fillStyle = color(b.c);
      const yy = Math.min(y(b.v), ht - 3);
      g.fillRect(x(i), yy, step - 1, ht - 1 - yy);
    }
  }
  for (const s of lines) {
    g.strokeStyle = color(s.color);
    g.lineWidth = 1.5;
    g.beginPath();
    let pen = false;
    s.values.forEach((v, k) => {
      const i = k + (n - s.values.length);
      if (v === null) {
        pen = false;
        return;
      }
      if (pen) g.lineTo(x(i) + step / 2, y(v));
      else g.moveTo(x(i) + step / 2, y(v));
      pen = true;
    });
    g.stroke();
  }
}

export function createSysMonitor(instanceId) {
  let epoch = 0;
  let after = 0;
  let samples = [];
  let now = null;
  let sortKey = 'cpu';
  let iface = '';
  let ifaceList = '';
  let busy = false;
  let skip = 0;
  let stopped = false;

  const upEl = h('div');
  const loadEl = h('div', { title: '1 / 5 / 15 分钟平均负载。实例是容器，这里读到的是整台宿主机的负载（上面可能还有别人的实例）' });
  const cpu = bar();
  const mem = bar();
  const swap = bar();
  const row = (label, b) => h('div', { class: 'mon-row' }, h('span', { class: 'mon-label' }, label), b.el);

  const procBody = h('tbody');
  const sortBtn = (key, text) => h('button', { type: 'button', 'data-key': key, title: `按${text}排序`, onclick: () => { sortKey = key; renderProcs(); } }, text);
  const procHead = h('tr', null, h('th', { class: 'num' }, sortBtn('mem', '内存')), h('th', { class: 'num' }, sortBtn('cpu', 'CPU')), h('th', null, '命令'));
  const procs = h('table', { class: 'mon-table mon-procs' }, h('thead', null, procHead), procBody);

  const upRate = h('span', { class: 'mon-up', title: '上行（实例发出）' });
  const downRate = h('span', { class: 'mon-down', title: '下行（实例收到）' });
  const ifaceSel = h('select', { class: 'mon-iface', title: '网卡', onchange: () => { iface = ifaceSel.value; renderNet(); } });
  const netChart = h('canvas', { class: 'mon-chart' });
  const latNow = h('span');
  const latChart = h('canvas', { class: 'mon-chart' });
  const diskBody = h('tbody');
  const disks = h('table', { class: 'mon-table mon-disks' },
    h('thead', null, h('tr', null, h('th', null, '路径'), h('th', { class: 'num' }, '可用/大小'))), diskBody);
  const msg = h('div', { class: 'mon-msg' }, '读取中…');
  const content = h('div', { class: 'mon-body', hidden: true },
    h('div', { class: 'mon-head' }, upEl, loadEl),
    row('CPU', cpu), row('内存', mem), row('交换', swap),
    procs,
    h('div', { class: 'mon-line' }, h('span', null, upRate, downRate), ifaceSel),
    netChart,
    h('div', { class: 'mon-line' }, latNow, h('span', { class: 'muted', title: '面板服务器到这台实例的 SSH 往返延迟' }, '面板 → 实例')),
    latChart,
    disks);
  const el = h('div', { class: 'mon' }, msg, content);

  function renderProcs() {
    for (const b of procHead.querySelectorAll('button')) b.classList.toggle('on', b.dataset.key === sortKey);
    if (!now) return;
    const list = [...now.procs].sort((a, b) => b[sortKey] - a[sortKey] || a.pid - b.pid).slice(0, SHOW_PROCS);
    mount(procBody, list.map((p) => h('tr', { title: `${p.name} · PID ${p.pid}\nCPU ${p.cpu.toFixed(1)}%（100% = 占满一个核）· 内存 ${short(p.mem)}` },
      shade(h('td', { class: 'num' }, short(p.mem)), p.mem / (now.mem.total || 1), '--warn'),
      shade(h('td', { class: 'num' }, p.cpu < 10 ? p.cpu.toFixed(1) : Math.round(p.cpu)), p.cpu / 100, '--ok'),
      h('td', { class: 'mon-cmd' }, p.name))));
  }

  function renderNet() {
    const names = now ? now.ifaces : [];
    if (!names.includes(iface)) iface = names.includes('eth0') ? 'eth0' : names.find((n) => n !== 'lo') || names[0] || '';
    if (names.join('\n') !== ifaceList) {
      ifaceList = names.join('\n');
      mount(ifaceSel, names.map((n) => h('option', { value: n }, n)));
    }
    ifaceSel.value = iface;
    const rates = samples.map((s) => s.net[iface] || [0, 0]);
    const last = rates[rates.length - 1] || [0, 0];
    upRate.textContent = `↑ ${short(last[1])}`;
    downRate.textContent = `↓ ${short(last[0])}`;
    drawChart(netChart, [
      { values: rates.map((r) => r[0]), color: '--ok' },
      { values: rates.map((r) => r[1]), color: '--warn' },
    ], { floor: 1024, fmt: short });
  }

  function renderLatency() {
    const lat = samples.map((s) => (s.lat >= 0 ? s.lat : null));
    const last = lat[lat.length - 1];
    latNow.textContent = last === null || last === undefined ? '—' : fmtMs(last);
    latNow.className = last === null || last === undefined ? 'muted' : last < 100 ? 'cost-ok' : last < 250 ? 'cost-warn' : 'cost-bad';
    drawChart(latChart, [{ values: lat, color: '--accent', line: true }], { floor: 10, fmt: (v) => `${Math.round(v)}` });
  }

  function render() {
    content.hidden = !now;
    if (!now) return;
    const scope = now.quota ? '按这台实例（容器）的配额算，不是整台宿主机' : '整台机器';
    upEl.textContent = `运行 ${fmtUptime(now.uptime)}`;
    loadEl.textContent = `负载 ${now.load.map((v) => v.toFixed(2)).join(', ')}`;
    const cores = Number.isInteger(now.cores) ? now.cores : now.cores.toFixed(1);
    cpu.set(now.cpu, `${Math.round(now.cpu)}%`, `${cores} 核`, `CPU：${scope}；${now.nprocs} 个进程`);
    const mp = pct(now.mem.used, now.mem.total);
    mem.set(mp, `${Math.round(mp)}%`, `${short(now.mem.used)}/${short(now.mem.total)}`, `内存：${scope}；已用里不算可回收的文件缓存`);
    const sp = pct(now.swap.used, now.swap.total);
    swap.set(sp, now.swap.total ? `${Math.round(sp)}%` : '无', now.swap.total ? `${short(now.swap.used)}/${short(now.swap.total)}` : '', now.swap.total ? '交换分区' : '这台实例没有交换分区');
    renderProcs();
    renderNet();
    renderLatency();
    mount(diskBody, now.disks.map((d) => shade(h('tr', { title: `${d.path}\n${d.fs} · 已用 ${short(d.size - d.avail)}（${Math.round(pct(d.size - d.avail, d.size))}%）` },
      h('td', { class: 'mon-cmd' }, d.path),
      h('td', { class: 'num' }, `${short(d.avail)}/${short(d.size)}`)),
    (d.size - d.avail) / (d.size || 1), pct(d.size - d.avail, d.size) >= 90 ? '--danger' : '--accent')));
  }

  function apply(res) {
    if (res.epoch !== epoch) {
      epoch = res.epoch;
      samples = [];
    }
    samples = samples.concat(res.samples).slice(-res.keep);
    if (samples.length) after = samples[samples.length - 1].seq;
    else after = 0;
    now = res.now;
    if (res.error) {
      mount(msg, errorBox({ message: res.error }), h('div', { class: 'hint' }, '会自动重试'));
      msg.hidden = false;
      skip = 2;
    } else {
      mount(msg, '读取中…');
      msg.hidden = Boolean(now);
    }
    render();
  }

  // onScreen is false while the tab, the page or the column is hidden; no
  // requests are made then, and the panel stops sampling after 20 seconds.
  const onScreen = () => !document.hidden && el.isConnected && el.offsetParent !== null;

  async function tick() {
    if (stopped || busy || !onScreen()) return;
    if (skip > 0) {
      skip--;
      return;
    }
    busy = true;
    try {
      const res = await get(`instances/${instanceId}/monitor?epoch=${epoch}&after=${after}`);
      if (!stopped) apply(res);
    } catch (err) {
      mount(msg, errorBox(err));
      msg.hidden = false;
      skip = 4;
    } finally {
      busy = false;
    }
  }

  const timer = setInterval(tick, POLL_MS);
  const ro = new ResizeObserver(() => {
    if (!now || !onScreen()) return;
    renderNet();
    renderLatency();
  });
  ro.observe(el);
  tick();

  return {
    el,
    // refresh asks right away, for when the column has just been shown.
    refresh: tick,
    stop() {
      stopped = true;
      clearInterval(timer);
      ro.disconnect();
    },
  };
}
