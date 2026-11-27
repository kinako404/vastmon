import { h } from './dom.js';
import { post, del } from './api.js';
import { fmtTime, fmtAgo } from './format.js';

// Rent watch: a blue star after a machine id. Filled = the panel records
// every 5 minutes how many of the machine's GPUs are rented and keeps 30
// days; hollow = off. Turning it off keeps the records already taken.

export function watchMap(list) {
  return new Map((list || []).map((w) => [w.machine_id, w]));
}

export function pct(s) {
  return s && s.rate >= 0 ? `${Math.round(s.rate * 100)}%` : '—';
}

export function nowText(w) {
  if (!w.last_ts) return '还没采样';
  if (!w.total) return '没挂出来';
  if (w.free === w.total) return '空着';
  if (!w.free) return '被租';
  return `${w.total - w.free}/${w.total} 张被租`;
}

function spanText(sec) {
  if (sec < 3600) return `${Math.max(1, Math.round(sec / 60))} 分钟`;
  if (sec < 86400) return `${(sec / 3600).toFixed(1)} 小时`;
  return `${(sec / 86400).toFixed(1)} 天`;
}

export function watchTip(w) {
  return [
    '被租监控中：每 5 分钟记一次这台机器有几张卡被租着，保留 30 天',
    `现在：${nowText(w)}（${w.last_ts ? fmtAgo(w.last_ts) + '采样' : '等第一次采样'}）`,
    `被租率：近 24 小时 ${pct(w.day)} · 近 7 天 ${pct(w.week)} · 近 30 天 ${pct(w.month)}`,
    w.since ? `已记录 ${spanText(Date.now() / 1000 - w.since)}，${w.month.samples} 次采样${w.month.offline ? `，其中 ${w.month.offline} 次没挂出来（不算进被租率）` : ''}` : null,
    w.check_err ? `上次采样出错：${w.check_err}` : null,
  ].filter(Boolean).join('\n');
}

// rentStar is the star for machine id. Logged-out viewers only see the filled
// ones. onChange gets the server's new watch list.
export function rentStar(watches, id, logged, onChange, onError) {
  const w = watches.get(id);
  if (!w && !logged) return null;
  const body = [w ? '★' : '☆', w ? h('span', { class: 'rw-pct' }, pct(w.month)) : null];
  if (!logged) return h('span', { class: 'rw-star on', title: watchTip(w) }, body);
  const btn = h('button', {
    type: 'button',
    class: w ? 'rw-star on' : 'rw-star',
    title: w ? watchTip(w) + '\n点一下关闭（已有记录保留）' : '开启被租监控：每 5 分钟记一次这台机器是否被租，保留 30 天，算近一个月的被租率',
  }, body);
  btn.addEventListener('click', async (e) => {
    e.preventDefault();
    e.stopPropagation();
    btn.disabled = true;
    try {
      const res = w ? await del(`rentwatch/${id}`) : await post(`rentwatch/${id}`);
      onChange(res.watches);
    } catch (err) {
      btn.disabled = false;
      if (onError) onError(err);
    }
  });
  return btn;
}

// cellStat sums samples [a, b) of the series: {n, off, rented, total}.
function cellStat(s, a, b) {
  const r = { n: 0, off: 0, rented: 0, total: 0 };
  for (let i = Math.max(0, a); i < Math.min(b, s.total.length); i++) {
    const t = s.total[i];
    if (t < 0) continue;
    r.n++;
    if (!t) r.off++;
    r.rented += t - s.free[i];
    r.total += t;
  }
  return r;
}

function cell(c, label) {
  if (!c.n) return h('i', { class: 'rw-c none', title: `${label}\n没有记录` });
  if (!c.total) return h('i', { class: 'rw-c off', title: `${label}\n没挂出来（${c.n} 次采样）` });
  const rate = c.rented / c.total;
  return h('i', {
    class: 'rw-c',
    style: `--r:${rate.toFixed(3)}`,
    title: `${label}\n被租 ${Math.round(rate * 100)}%（${c.n} 次采样${c.off ? `，${c.off} 次没挂出来` : ''}）`,
  });
}

const pad = (n) => String(n).padStart(2, '0');

// rentChart draws the last 24 hours sample by sample, and one cell per hour
// for each day since the first record (local time, newest day on top).
export function rentChart(series) {
  const s = series;
  const first = s.total.findIndex((t) => t >= 0);
  if (first < 0) return h('p', { class: 'hint' }, '还没有记录。');
  const at = (i) => s.from + i * s.step;
  const end = s.total.length;
  const recent = [];
  for (let i = Math.max(0, end - 288); i < end; i++) {
    const d = new Date(at(i) * 1000);
    recent.push(cell(cellStat(s, i, i + 1), `${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`));
  }
  const day0 = new Date(at(first) * 1000);
  day0.setHours(0, 0, 0, 0);
  const rows = [];
  for (let d = new Date(); d >= day0; d.setDate(d.getDate() - 1)) {
    const start = new Date(d);
    start.setHours(0, 0, 0, 0);
    const cells = [];
    for (let hr = 0; hr < 24; hr++) {
      const t0 = new Date(start);
      t0.setHours(hr);
      const t1 = new Date(t0);
      t1.setHours(hr + 1);
      const a = Math.ceil((t0 / 1000 - s.from) / s.step);
      const b = Math.ceil((t1 / 1000 - s.from) / s.step);
      cells.push(cell(cellStat(s, a, b), `${pad(start.getMonth() + 1)}-${pad(start.getDate())} ${pad(hr)}:00–${pad(hr + 1)}:00`));
    }
    const a = Math.ceil((start / 1000 - s.from) / s.step);
    const dayStat = cellStat(s, a, a + 288);
    rows.push(h('div', { class: 'rw-row' },
      h('span', { class: 'rw-day' }, `${pad(start.getMonth() + 1)}-${pad(start.getDate())}`),
      h('div', { class: 'rw-hours' }, cells),
      h('span', { class: 'rw-day-pct' }, dayStat.total ? `${Math.round((dayStat.rented / dayStat.total) * 100)}%` : '—')));
  }
  return h('div', { class: 'rw-chart' },
    h('div', { class: 'hint' }, '近 24 小时（每格 5 分钟）'),
    h('div', { class: 'rw-strip' }, recent),
    h('div', { class: 'hint rw-gap' }, '按天 × 小时（每格 1 小时，颜色越深被租越多；右边是当天被租率）'),
    h('div', { class: 'rw-row rw-axis' }, h('span', { class: 'rw-day' }),
      h('div', { class: 'rw-hours' }, Array.from({ length: 24 }, (_, i) => h('span', null, i % 3 ? '' : String(i)))),
      h('span', { class: 'rw-day-pct' })),
    rows,
    h('div', { class: 'rw-legend hint' },
      h('i', { class: 'rw-c', style: '--r:0' }), ' 空着 ',
      h('i', { class: 'rw-c', style: '--r:0.5' }), ' 部分时间 / 部分卡被租 ',
      h('i', { class: 'rw-c', style: '--r:1' }), ' 被租 ',
      h('i', { class: 'rw-c off' }), ' 没挂出来 ',
      h('i', { class: 'rw-c none' }), ' 没有记录'),
    h('div', { class: 'hint' }, `最早记录 ${fmtTime(at(first))}`));
}
