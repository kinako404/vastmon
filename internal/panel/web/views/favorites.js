import { h, mount, table, empty, errorBox, loading, field } from '../lib/dom.js';
import { get, post, put, del, currentSession, isAbort } from '../lib/api.js';
import { fmtTime, fmtAgo, fmtDuration, fixed, gb, costClass, shortCPU, cores } from '../lib/format.js';
import { fmtGeo } from '../lib/geo.js';
import { openDialog } from '../lib/dialog.js';
import { openRentDialog } from './rent.js';
import { watchTip, pct, nowText } from '../lib/rentwatch.js';

const POLL_MS = 30000;

function statusCell(f) {
  const o = f.offer;
  if (o) {
    return [
      h('span', { class: 'badge on' }, '可开机'), ' ',
      h('span', { class: 'strong' }, `$${fixed(o.dph_total, 3)}/h`),
      h('div', { class: 'hint' },
        `${o.num_gpus} 卡 · 可用磁盘 ${fixed(o.disk_space, 0)} GB · 流量费 `,
        h('span', { class: costClass(o.internet_down_cost_per_tb) }, '↓$' + fixed(o.internet_down_cost_per_tb, 1)),
        ' / ',
        h('span', { class: costClass(o.internet_up_cost_per_tb) }, '↑$' + fixed(o.internet_up_cost_per_tb, 1)),
        ' 每 TB'),
    ];
  }
  if (!f.checked_at) return h('span', { class: 'badge' }, '还没检查');
  return [
    h('span', { class: 'badge bad' }, '现在租不了'),
    h('div', { class: 'hint' }, f.last_avail_at ? `上次可租：${fmtAgo(f.last_avail_at)}` : '收藏后还没见它空出来过'),
  ];
}

function specCell(f) {
  const o = f.offer;
  if (!o) return h('span', { title: f.cpu_name || '' }, shortCPU(f.cpu_name));
  return [h('span', { title: o.cpu_name || '' }, shortCPU(o.cpu_name)), ' · ', h('span', { class: 'strong' }, cores(o.cpu_cores_effective) + ' 核'),
    h('div', { class: 'hint' }, `内存 ${gb(o.cpu_ram)} GB · ↓${fixed(o.inet_down, 0)} / ↑${fixed(o.inet_up, 0)} Mbps · 可靠 ${fixed(o.reliability, 3)}`)];
}

export async function favoritesView(ctx) {
  const { signal } = ctx;
  const message = h('div');
  const box = h('div', null, loading());
  const watchBox = h('div');
  let watches = [];
  const status = h('span', { class: 'hint' });
  const checkBtn = h('button', { type: 'button', class: 'primary' }, '立即检查');
  let data = null;

  function noteDialog(f) {
    const d = openDialog();
    const input = h('input', { value: f.note || '', maxlength: '200' });
    const msg = h('div');
    const ok = h('button', { type: 'submit', class: 'primary' }, '保存');
    mount(d.body, h('form', {
      onsubmit: async (e) => {
        e.preventDefault();
        d.setBusy(true);
        ok.disabled = true;
        try {
          data = await put(`favorites/${f.machine_id}/note`, { note: input.value.trim() });
          d.setBusy(false);
          d.close();
          render();
        } catch (err) {
          d.setBusy(false);
          ok.disabled = false;
          mount(msg, errorBox(err));
        }
      },
    }, h('h2', null, `机器 ${f.machine_id} 的收藏原因`), field('收藏原因', input, '比如“下载快、稳定不掉线”'), msg,
    h('div', { class: 'toolbar end' }, h('button', { type: 'button', onclick: d.close }, '取消'), ok)));
    input.focus();
  }

  async function remove(f, btn) {
    if (!window.confirm(`取消收藏机器 ${f.machine_id}？${f.note ? `\n收藏原因“${f.note}”也会删掉。` : ''}`)) return;
    btn.disabled = true;
    try {
      data = await del(`favorites/${f.machine_id}`);
      render();
    } catch (err) {
      btn.disabled = false;
      mount(message, errorBox(err));
    }
  }

  function render() {
    const favs = [...data.favorites].sort((a, b) => Boolean(b.offer) - Boolean(a.offer) || b.created - a.created);
    const checked = Math.max(0, ...favs.map((f) => f.checked_at || 0));
    const avail = favs.filter((f) => f.offer).length;
    mount(status, favs.length
      ? `${favs.length} 台收藏，现在 ${avail} 台可开机 · 上次检查 ${checked ? fmtAgo(checked) : '还没有'} · 每 ${fmtDuration(data.interval_sec)}随监控自动检查`
      : '');
    const logged = currentSession().logged_in;
    checkBtn.hidden = !logged || !favs.length;
    const errs = [...new Set(favs.map((f) => f.check_err).filter(Boolean))];
    mount(box,
      errs.length ? errorBox({ message: '上次检查出错：' + errs[0] }) : null,
      h('section', { class: 'card' }, favs.length ? table([
        {
          title: '机器',
          class: 'nowrap',
          cell: (f) => [h('a', { href: `#/machines/${f.machine_id}/${f.num_gpus}` }, String(f.machine_id)),
            f.source ? h('div', { class: 'hint' }, f.source) : null],
        },
        { title: 'GPU', class: 'nowrap', cell: (f) => `${f.num_gpus}× ${f.gpu_name || '—'}` },
        { title: '状态', cell: statusCell },
        { title: 'CPU · 配置', cell: specCell },
        { title: '位置', class: 'nowrap', cell: (f) => h('span', { title: f.geolocation || '' }, fmtGeo(f.geolocation)) },
        { title: '收藏原因', cell: (f) => f.note || h('span', { class: 'muted' }, '没写') },
        { title: '收藏于', class: 'nowrap', cell: (f) => [fmtTime(f.created).slice(0, 16), h('div', { class: 'hint' }, fmtAgo(f.created))] },
        {
          title: '',
          class: 'nowrap',
          cell: (f) => (logged ? [
            f.offer ? h('button', { type: 'button', class: 'small primary', onclick: () => openRentDialog(f.offer) }, '开机') : null, ' ',
            h('button', { type: 'button', class: 'small', onclick: () => noteDialog(f) }, '改原因'), ' ',
            h('button', { type: 'button', class: 'small danger', onclick: (e) => remove(f, e.currentTarget) }, '取消收藏'),
          ] : null),
        },
      ], favs, (f) => (f.offer ? null : 'unavail')) : empty('还没有收藏的机器。在“实例”页的实例卡片或开机记录里点“☆ 收藏本机”。')));
  }

  async function unwatch(w, btn) {
    btn.disabled = true;
    try {
      watches = (await del(`rentwatch/${w.machine_id}`)).watches;
      renderWatches();
    } catch (err) {
      btn.disabled = false;
      mount(message, errorBox(err));
    }
  }

  function renderWatches() {
    const logged = currentSession().logged_in;
    const rate = (s) => h('span', { class: 'num' }, pct(s));
    mount(watchBox, h('section', { class: 'card' },
      h('h2', null, `被租监控（${watches.length}）`),
      watches.length ? table([
        {
          title: '机器',
          class: 'nowrap',
          cell: (w) => [w.num_gpus ? h('a', { href: `#/machines/${w.machine_id}/${w.num_gpus}`, title: watchTip(w) }, String(w.machine_id)) : String(w.machine_id),
            h('span', { class: 'rw-star on', title: watchTip(w) }, '★')],
        },
        { title: 'GPU', class: 'nowrap', cell: (w) => (w.gpu_name ? `${w.total ? `整机 ${w.total}× ` : ''}${w.gpu_name}` : '—') },
        { title: '位置', class: 'nowrap', cell: (w) => h('span', { title: w.geolocation || '' }, fmtGeo(w.geolocation)) },
        { title: '现在', class: 'nowrap', cell: (w) => [nowText(w), w.check_err ? h('div', { class: 'hint cost-bad' }, w.check_err) : null] },
        { title: '近 24 小时', class: 'num', cell: (w) => rate(w.day) },
        { title: '近 7 天', class: 'num', cell: (w) => rate(w.week) },
        { title: '近 30 天', class: 'num strong', cell: (w) => rate(w.month) },
        { title: '采样', class: 'num', cell: (w) => String(w.month.samples) },
        { title: '开启于', class: 'nowrap', cell: (w) => [fmtTime(w.created).slice(0, 16), h('div', { class: 'hint' }, fmtAgo(w.created))] },
        { title: '', class: 'nowrap', cell: (w) => (logged ? h('button', { type: 'button', class: 'small', onclick: (e) => unwatch(w, e.currentTarget) }, '关闭监控') : null) },
      ], watches) : empty('没有开着的。在机器列表里点机器号后面的 ☆ 开启。'),
      h('p', { class: 'hint' }, '每 5 分钟记一次，保留 30 天。被租率 = 被租的卡 ÷ 挂出来的卡；没挂出来的时间不算。关闭后已有记录保留，机器详情页还能看到图。')));
  }

  checkBtn.addEventListener('click', async () => {
    checkBtn.disabled = true;
    try {
      data = await post('favorites/check');
      mount(message);
      render();
    } catch (err) {
      mount(message, errorBox(err));
    } finally {
      checkBtn.disabled = false;
    }
  });

  let timer = 0;
  async function refresh() {
    try {
      [data, { watches }] = await Promise.all([get('favorites', signal), get('rentwatch', signal)]);
      mount(message);
      render();
      renderWatches();
    } catch (err) {
      if (!isAbort(err)) mount(message, errorBox(err));
    }
    if (!signal.aborted) timer = setTimeout(refresh, POLL_MS);
  }
  signal.addEventListener('abort', () => clearTimeout(timer));

  ctx.show(
    h('div', { class: 'title-row' }, h('h1', null, '收藏的机器'), h('span', { class: 'spacer' }), checkBtn),
    h('p', null, status),
    message, box, watchBox);
  await refresh();
}
