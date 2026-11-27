import { h, mount, errorBox, loading, empty, table, field } from '../lib/dom.js';
import { get, post, put, del, isAbort, currentSession } from '../lib/api.js';
import { openRentDialog } from './rent.js';
import { marksFrom, markBadge, markKind } from '../lib/marks.js';
import { watchMap, rentStar } from '../lib/rentwatch.js';
import { fmtGeo, geoMode, setGeoMode, GEO_MODES } from '../lib/geo.js';
import { fmtClock, fmtAgo, fmtDuration, fixed, gb, days, costClass, shortCPU, cores } from '../lib/format.js';

const POLL_MS = 30000;
// SHOW_FIRST is the localStorage key remembering whether 首次出现 is shown.
const SHOW_FIRST = 'vastmon.showFirstSeen';

// FIELDS are the filter's numeric inputs, grouped; 0 or empty means no limit.
const FIELDS = [
  ['机器', [
    ['num_gpus', 'GPU 数', true],
    ['min_vram', '单卡显存 ≥ GB'],
    ['min_cpu', 'CPU 核 ≥'],
    ['min_ram', '内存 ≥ GB'],
    ['min_disk', '磁盘 ≥ GB'],
    ['min_cuda', 'CUDA ≥'],
    ['min_rel', '可靠性 ≥'],
    ['min_days', '可租天数 ≥'],
    ['min_ports', '开放端口 ≥', true],
    ['max_dph', '单价 ≤ $/h'],
  ]],
  ['网络', [
    ['min_down', '下行 ≥ Mbps'],
    ['min_up', '上行 ≥ Mbps'],
    ['max_down_tb', '下行流量费 ≤ $/TB'],
    ['max_up_tb', '上行流量费 ≤ $/TB'],
  ]],
  ['估算日费用的流量', [
    ['exp_down_gb', '每天下行 GB'],
    ['exp_up_gb', '每天上行 GB'],
  ]],
];

function stat(k, v, sub, bad) {
  return h('div', { class: bad ? 'stat bad' : 'stat' },
    h('div', { class: 'k' }, k), h('div', { class: 'v' }, v), sub ? h('div', { class: 'sub' }, sub) : null);
}

function renderStats(st) {
  const p = st.poll;
  const online = st.rows.filter((r) => r.online);
  const best = online.length ? online.reduce((a, b) => (b.est_day < a.est_day ? b : a)) : null;
  const cheapest = online.length ? online.reduce((a, b) => (b.dph_total < a.dph_total ? b : a)) : null;
  return h('div', { class: 'cards' },
    stat('上次抓取', fmtClock(p.ts), p.err ? '出错' : `${fmtAgo(p.ts)} · ${p.n} 条报价`, Boolean(p.err)),
    stat('所选型号在线', String(st.online), `每 ${fmtDuration(st.interval_sec)}抓取`),
    stat('符合条件', String(online.length), (() => {
      const rented = st.rows.filter((r) => r.rented).length;
      const gone = st.rows.length - online.length - rented;
      const more = [rented ? `${rented} 台被租走` : '', gone ? `${gone} 台已下架` : ''].filter(Boolean).join('、');
      return more ? `可租；另有 ${more}` : '可租的机器';
    })()),
    stat('最低估算日费', best ? '$' + fixed(best.est_day, 2) : '—', best ? `机器 ${best.machine_id} · ${fmtGeo(best.geolocation)}` : ''),
    stat('估算周费', best ? '$' + fixed(best.est_day * 7, 2) : '—', best ? `同一台 × 7 天` : ''),
    stat('估算月费', best ? '$' + fixed(best.est_day * 30, 2) : '—', best ? `同一台 × 30 天` : ''),
    stat('最低单价', cheapest ? '$' + fixed(cheapest.dph_total, 3) + '/h' : '—', cheapest ? `机器 ${cheapest.machine_id}` : ''));
}

const median = (xs) => {
  const s = [...xs].sort((a, b) => a - b);
  const m = s.length >> 1;
  return s.length % 2 ? s[m] : (s[m - 1] + s[m]) / 2;
};

// bestPicks compares the online rows measure by measure: for each, the best
// machine (ties go to the lower $/day) against the median of all of them.
// Every figure is one the table already shows or a plain ratio of two.
function bestPicks(st) {
  const rows = st.rows.filter((r) => r.online);
  if (rows.length < 2) return [];
  const f = st.filter;
  const perDay = f.exp_down_gb > 0 || f.exp_up_gb > 0;
  const traffic = perDay
    ? (r) => (f.exp_down_gb * r.internet_down_cost_per_tb + f.exp_up_gb * r.internet_up_cost_per_tb) / 1024
    : (r) => r.internet_down_cost_per_tb + r.internet_up_cost_per_tb;
  const watts = (r) => (r.gpu_max_power || 0) * r.num_gpus;
  const measures = [
    {
      title: '最低日费', low: true, cmp: 'less',
      tip: '估算 $/天最低：单价 × 24 + 每天预计流量的费用',
      score: (r) => r.est_day, fmt: (v) => `$${fixed(v, 2)}/天`,
      value: (r) => `$${fixed(r.est_day, 2)}/天`, detail: (r) => `$${fixed(r.dph_total, 3)}/h`,
    },
    {
      title: '每 $ 下载最快', cmp: 'times',
      tip: '下行带宽 ÷ $/h：同样的钱下载最快，开机拉模型最省时间',
      score: (r) => (r.dph_total > 0 ? r.inet_down / r.dph_total : NaN), fmt: (v) => `${fixed(v, 0)} Mbps/$`,
      value: (r) => `${fixed(r.inet_down, 0)} Mbps`, detail: (r) => `$${fixed(r.dph_total, 3)}/h`,
    },
    {
      title: '流量最便宜', low: true, cmp: 'less',
      tip: perDay ? `按过滤条件里每天下行 ${f.exp_down_gb} GB、上行 ${f.exp_up_gb} GB 算的每天流量费` : '下行 + 上行流量费（$/TB）',
      score: traffic, fmt: (v) => (perDay ? `$${fixed(v, 2)}/天` : `$${fixed(v, 1)}/TB`),
      value: (r) => (perDay ? `$${fixed(traffic(r), 2)}/天` : `$${fixed(traffic(r), 1)}/TB`),
      detail: (r) => `↓$${fixed(r.internet_down_cost_per_tb, 1)} ↑$${fixed(r.internet_up_cost_per_tb, 1)} /TB`,
    },
    {
      title: '最稳定', cmp: 'plain',
      tip: 'vast 给的可靠性最高（主机历史在线率）',
      score: (r) => r.reliability, fmt: (v) => fixed(v, 3),
      value: (r) => fixed(r.reliability, 3), detail: (r) => `$${fixed(r.dph_total, 3)}/h`,
    },
    {
      title: '每 $ 功耗最高', cmp: 'times',
      tip: '功耗上限 × 卡数 ÷ $/h。同型号的卡长时间满载的算力随功耗上限走（RTX PRO 6000 实测持续 FP16：600W 约 345–358、550W 约 330、400W 约 283 TFLOPS），功耗上限是 vast 报的，和卡上 nvidia-smi 一致',
      score: (r) => (watts(r) > 0 && r.dph_total > 0 ? watts(r) / r.dph_total : NaN), fmt: (v) => `${fixed(v, 0)} W/$`,
      value: (r) => `${fixed(watts(r), 0)}W`, detail: (r) => `$${fixed(r.dph_total, 3)}/h`,
    },
  ];
  const picks = [];
  for (const m of measures) {
    const scored = rows.map((r) => ({ r, v: m.score(r) })).filter((x) => Number.isFinite(x.v));
    if (scored.length < 2) continue;
    const best = m.low ? Math.min(...scored.map((x) => x.v)) : Math.max(...scored.map((x) => x.v));
    const tied = scored.filter((x) => Math.abs(x.v - best) <= 1e-9 * Math.max(1, Math.abs(best)))
      .sort((a, b) => a.r.est_day - b.r.est_day || a.r.dph_total - b.r.dph_total);
    picks.push({ ...m, row: tied[0].r, best, ties: tied.length - 1, med: median(scored.map((x) => x.v)), n: scored.length });
  }
  return picks;
}

function compareText(p) {
  const med = `中位数 ${p.fmt(p.med)}`;
  if (p.cmp === 'less' && p.med > 0) {
    const pct = Math.round((1 - p.best / p.med) * 100);
    return pct > 0 ? `${med}，低 ${pct}%` : med;
  }
  if (p.cmp === 'times' && p.med > 0) return `${p.fmt(p.best)}，是中位数的 ${fixed(p.best / p.med, 1)} 倍（${p.fmt(p.med)}）`;
  return med;
}

function renderBest(picks, online) {
  if (!picks.length) return null;
  return [
    h('div', { class: 'best-head' }, h('span', { class: 'strong' }, '各项最佳'),
      h('span', { class: 'hint' }, ` 在符合条件的 ${online} 台在线机器里逐项比，和中位数对比；并列时取估算日费低的`)),
    h('div', { class: 'cards' }, picks.map((p) => h('div', { class: 'stat best', title: p.tip },
      h('div', { class: 'k' }, p.title),
      h('div', { class: 'v' }, p.value(p.row)),
      h('div', { class: 'sub' }, h('a', { href: `#/machines/${p.row.machine_id}/${p.row.num_gpus}` }, `机器 ${p.row.machine_id}`),
        ` · ${p.detail(p.row)}`),
      h('div', { class: 'sub' }, compareText(p), p.ties ? ` · 另有 ${p.ties} 台并列` : '')))),
  ];
}

function costCell(v) {
  return h('span', { class: costClass(v) }, fixed(v, 1));
}

// gpuPerf is the GPU cell's hover text: vast's reference compute figures.
function gpuPerf(r) {
  if (!r.dlperf) return 'vast 没给算力参考值';
  return [`DLPerf ${fixed(r.dlperf, 1)}（vast 的整机通用深度学习基准分）`,
    `${fixed(r.total_flops, 1)} TFLOPS（vast 标称）`,
    r.dph_total ? `${fixed(r.dlperf / r.dph_total, 1)} DLP/$/h（DLPerf ÷ $/h，越高越划算）` : null,
    '都是 vast 的估算参考值，不是实测'].filter(Boolean).join('\n');
}

function columns(f, disk, rentCol, marks, star) {
  return [
    {
      title: '机器',
      class: 'nowrap',
      cell: (r) => [
        h('a', { class: 'mid', href: `#/machines/${r.machine_id}/${r.num_gpus}` }, String(r.machine_id)), star(r.machine_id), ' ',
        markBadge(marks, r.machine_id),
        r.new ? h('span', { class: 'badge on' }, '新') : null,
        r.rented ? h('span', { class: 'badge warn', title: `${r.mine ? '你自己的实例在这台机器上' : 'vast 上这台眼下被别人租着'}（${fmtAgo(r.rented_seen)}看到被租着）${r.last_seen ? `，上次能租是 ${fmtAgo(r.last_seen)}` : '，面板还没见过它空着'}。价格是它挂的报价，空出来后可能会变` }, r.mine ? '你在租' : '被租走')
          : r.online ? null : h('span', { class: 'badge' }, '下架'),
      ],
    },
    rentCol,
    {
      title: 'GPU',
      class: 'nowrap',
      cell: (r) => h('span', { title: gpuPerf(r) }, `${r.num_gpus}× ${r.gpu_name} `, h('span', { class: 'muted' }, gb(r.gpu_ram) + 'G')),
    },
    {
      title: 'CPU · 分到的核',
      key: 'cpu',
      class: 'nowrap',
      cell: (r) => h('span', { title: `${r.cpu_name || '未知型号'}\n分到 ${cores(r.cpu_cores_effective)} / 整机 ${cores(r.cpu_cores)}（vast 按 vCPU 线程计数）` },
        shortCPU(r.cpu_name), ' · ', h('span', { class: 'strong' }, cores(r.cpu_cores_effective) + ' 核'),
        r.cpu_cores ? h('span', { class: 'muted' }, ' /' + cores(r.cpu_cores)) : null),
    },
    { title: '$/h', key: 'dph', class: 'num', cell: (r) => fixed(r.dph_total, 3) },
    {
      title: '磁盘 $/h',
      key: 'disk_h',
      class: 'num',
      cell: (r) => h('span', { title: `$${fixed(r.storage_cost, 3)} /GB/月，按 ${fixed(disk, 0)} GB 计；已含在 $/h 里` }, fixed(r.storage_total_cost, 3)),
    },
    { title: '估算 $/天', key: 'est', class: 'num strong', cell: (r) => fixed(r.est_day, 2) },
    { title: '下行 $/TB', key: 'down_tb', class: 'num', cell: (r) => costCell(r.internet_down_cost_per_tb) },
    { title: '上行 $/TB', key: 'up_tb', class: 'num', cell: (r) => costCell(r.internet_up_cost_per_tb) },
    {
      title: '预估流量费 $/天',
      key: 'traffic',
      class: 'num',
      cell: (r) => h('span', { title: `每天下行 ${f.exp_down_gb} GB × $${fixed(r.internet_down_cost_per_tb, 2)}/TB + 上行 ${f.exp_up_gb} GB × $${fixed(r.internet_up_cost_per_tb, 2)}/TB（1 TB 按 1024 GB）；已含在估算 $/天里` },
        fixed(r.est_day - r.dph_total * 24, 2)),
    },
    { title: '下行 Mbps', key: 'down', class: 'num', cell: (r) => fixed(r.inet_down, 0) },
    { title: '上行 Mbps', key: 'up', class: 'num', cell: (r) => fixed(r.inet_up, 0) },
    { title: '内存 GB', key: 'ram', class: 'num', cell: (r) => gb(r.cpu_ram) },
    { title: '磁盘 GB', key: 'disk', class: 'num', cell: (r) => fixed(r.disk_space, 0) },
    { title: '可靠性', key: 'rel', class: 'num', cell: (r) => fixed(r.reliability, 3) },
    { title: 'CUDA', class: 'num', cell: (r) => fixed(r.cuda_max_good, 1) },
    { title: '端口', class: 'num', cell: (r) => String(r.direct_port_count) },
    { title: '可租天', key: 'days', class: 'num', cell: (r) => days(r.duration) },
    { title: '位置', class: 'nowrap', cell: (x) => h('span', { title: x.geolocation || '' }, fmtGeo(x.geolocation)) },
    { title: '首次出现', key: 'first', optional: true, class: 'num', cell: (r) => fmtClock(r.first_seen) },
  ];
}

export async function machinesView(ctx) {
  const { signal } = ctx;
  let st = await get('state', signal);

  const message = h('div');
  const stats = h('div');
  const formBox = h('div');
  const tableBox = h('div', null, loading());
  const geoSwitch = h('div', { class: 'table-foot' });
  function renderGeoSwitch() {
    const current = geoMode();
    mount(geoSwitch, h('span', { class: 'hint' }, '位置显示：'), h('div', { class: 'seg', role: 'group', 'aria-label': '位置显示' },
      Object.entries(GEO_MODES).map(([mode, text]) => h('button', {
        type: 'button',
        class: mode === current ? 'selected' : null,
        'aria-pressed': String(mode === current),
        onclick: () => {
          setGeoMode(mode);
          renderGeoSwitch();
          renderTable();
        },
      }, text))));
  }
  renderGeoSwitch();
  let showFirst = localStorage.getItem(SHOW_FIRST) === '1';
  const firstToggle = h('button', { type: 'button', class: 'link small' });
  firstToggle.addEventListener('click', () => {
    showFirst = !showFirst;
    localStorage.setItem(SHOW_FIRST, showFirst ? '1' : '0');
    renderTable();
  });
  const pollBtn = h('button', { type: 'button', class: 'primary' }, '立即抓取');

  // apply sends a filter change and shows the state the server returns.
  async function apply(change, busy) {
    if (busy) busy.disabled = true;
    try {
      st = await change();
      mount(message);
      renderTable();
    } catch (err) {
      if (!isAbort(err)) mount(message, errorBox(err));
    } finally {
      if (busy) busy.disabled = false;
    }
  }

  function sortHead(c) {
    if (!c.key) return () => c.title;
    return (title) => {
      const active = st.filter.sort === c.key;
      const arrow = active ? (st.filter.desc ? ' ↓' : ' ↑') : '';
      return h('button', {
        type: 'button',
        class: active ? 'sort active' : 'sort',
        title: '按此列排序，再点一次切换升降序',
        onclick: (e) => apply(() => put('filter', {
          ...st.filter, sort: c.key, desc: active ? !st.filter.desc : false,
        }), e.currentTarget),
      }, title + arrow);
    };
  }

  function renderTable() {
    mount(stats, renderStats(st), renderBest(bestPicks(st), st.rows.filter((r) => r.online).length));
    const marks = marksFrom(st);
    const rentCol = currentSession().logged_in ? {
      title: '',
      cell: (r) => (r.online ? h('button', {
        type: 'button', class: 'small primary', onclick: () => openRentDialog(r, null, { blacklisted: marks.black.get(r.machine_id) }),
      }, '开机') : null),
    } : null;
    mount(firstToggle, showFirst ? '收起“首次出现”列' : '展开“首次出现”列');
    const watches = watchMap(st.rent_watches);
    const star = (id) => rentStar(watches, id, currentSession().logged_in, (list) => {
      st.rent_watches = list;
      renderTable();
    }, (err) => mount(message, errorBox(err)));
    const cols = columns(st.filter, st.disk, rentCol, marks, star).filter((c) => c && (showFirst || !c.optional)).map((c) => ({ ...c, head: sortHead(c) }));
    mount(tableBox, st.rows.length
      ? table(cols, st.rows, (r) => [r.online ? '' : r.rented ? 'rented' : 'gone', markKind(marks, r.machine_id) ? 'row-' + markKind(marks, r.machine_id) : ''].join(' ').trim() || null)
      : empty('没有符合条件的机器'));
  }

  // samePreset tells whether filter f is preset p (sorting and "show gone"
  // are view settings, not part of a preset's match).
  const presetKey = (f) => JSON.stringify(Object.keys(f).filter((k) => !['sort', 'desc', 'show_gone', 'show_rented'].includes(k)).sort()
    .map((k) => [k, k === 'gpus' ? [...(f[k] || [])].sort() : f[k] || 0]));
  const activePreset = () => (st.presets || []).find((p) => presetKey(p.filter) === presetKey(st.filter));

  function presetBar(readForm) {
    const logged = currentSession().logged_in;
    const cur = activePreset();
    const presetMsg = h('span');
    const fail = (err) => mount(presetMsg, h('span', { class: 'cost-bad' }, err.message || String(err)));
    const savePresets = async (body) => {
      st.presets = (await put('filter/presets', body)).presets;
    };
    const ask = (text, init) => {
      const v = window.prompt(text, init || '');
      return v === null ? null : v.trim();
    };
    return h('div', { class: 'preset-bar' },
      h('span', { class: 'hint' }, '方案：'),
      (st.presets || []).length ? st.presets.map((p) => h('button', {
        type: 'button', class: p === cur ? 'tab active' : 'tab',
        title: p === cur ? '当前条件就是这个方案' : `切换到“${p.name}”`,
        onclick: (e) => {
          if (p === cur) return;
          apply(() => put('filter', { ...p.filter, sort: st.filter.sort, desc: st.filter.desc, show_gone: st.filter.show_gone, show_rented: st.filter.show_rented }), e.currentTarget).then(renderForm);
        },
      }, p.name)) : h('span', { class: 'hint' }, '还没有，把常用条件存成方案就能一点切换'),
      h('span', { class: 'spacer' }),
      logged ? h('button', {
        type: 'button', class: 'small', title: '把下面表单里的条件存成一个新方案（同时应用）',
        onclick: async () => {
          const name = ask('新方案的名字：', '');
          if (!name) return;
          try {
            const f = readForm();
            await savePresets({ name, filter: f });
            await apply(() => put('filter', f));
            renderForm();
          } catch (err) {
            fail(err);
          }
        },
      }, '+ 存为新方案') : h('span', { class: 'hint' }, '登录后可以保存方案'),
      logged && cur ? [
        h('button', {
          type: 'button', class: 'small', title: `用下面表单里的条件覆盖“${cur.name}”`,
          onclick: async () => {
            try {
              const f = readForm();
              await savePresets({ name: cur.name, filter: f });
              await apply(() => put('filter', f));
              renderForm();
            } catch (err) {
              fail(err);
            }
          },
        }, '更新方案'),
        h('button', {
          type: 'button', class: 'small',
          onclick: async () => {
            const name = ask('改名为：', cur.name);
            if (!name || name === cur.name) return;
            try {
              await savePresets({ name, old_name: cur.name });
              renderForm();
            } catch (err) {
              fail(err);
            }
          },
        }, '改名'),
        h('button', {
          type: 'button', class: 'small danger',
          onclick: async () => {
            if (!window.confirm(`删除方案“${cur.name}”？当前过滤条件不变。`)) return;
            try {
              st.presets = (await del(`filter/presets/${encodeURIComponent(cur.name)}`)).presets;
              renderForm();
            } catch (err) {
              fail(err);
            }
          },
        }, '删除方案')] : null,
      presetMsg);
  }

  function renderForm() {
    const f = st.filter;
    const inputs = {};
    const gpuBoxes = st.gpu_names.map((name) => {
      const box = h('input', { type: 'checkbox', value: name, checked: f.gpus.includes(name) });
      const label = h('label', { class: box.checked ? 'checked' : null }, box, name);
      box.addEventListener('change', () => label.classList.toggle('checked', box.checked));
      return { box, label };
    });
    // The model search: case and spaces ignored ("pro6000" finds RTX PRO 6000
    // WS); checked models stay visible; Enter checks the first match and does
    // not submit the filter form; Esc clears.
    const norm = (s) => s.toLowerCase().replace(/\s+/g, '');
    const search = h('input', { type: 'search', class: 'gpu-search', placeholder: '搜型号，如 4090、pro6000', spellcheck: 'false', autocomplete: 'off' });
    const matchInfo = h('span', { class: 'hint' });
    const filterGpus = () => {
      const q = norm(search.value);
      const hits = gpuBoxes.filter((g) => !q || norm(g.box.value).includes(q));
      for (const g of gpuBoxes) g.label.hidden = !(hits.includes(g) || g.box.checked);
      matchInfo.textContent = q ? `匹配 ${hits.length} 个` : '';
      return q ? hits : [];
    };
    search.addEventListener('input', filterGpus);
    search.addEventListener('click', (e) => e.preventDefault());
    // The search sits inside <summary>, which toggles the list on Space (on
    // key up) and Enter (on key press); stop that without stopping the typing.
    search.addEventListener('keyup', (e) => {
      if (e.key === ' ' || e.key === 'Enter') e.preventDefault();
    });
    search.addEventListener('keypress', (e) => {
      if (e.key === 'Enter') e.preventDefault();
    });
    search.addEventListener('keydown', (e) => {
      if (e.key === 'Enter') {
        e.preventDefault();
        const first = filterGpus().find((g) => !g.box.checked);
        if (first) {
          first.box.checked = true;
          first.box.dispatchEvent(new Event('change'));
        }
      } else if (e.key === 'Escape') {
        e.preventDefault();
        search.value = '';
        filterGpus();
      }
    });
    const groups = FIELDS.map(([title, fields]) => [
      h('div', { class: 'form-group' }, title),
      h('div', { class: 'form-grid' }, fields.map(([key, label, int]) => {
        inputs[key] = { int, el: h('input', { type: 'number', min: '0', step: int ? '1' : 'any', placeholder: '不限', value: f[key] ? String(f[key]) : '' }) };
        return field(label, inputs[key].el);
      })),
    ]);
    const gone = h('input', { type: 'checkbox', checked: f.show_gone });
    const rentedBox = h('input', { type: 'checkbox', checked: f.show_rented });
    const applyBtn = h('button', { type: 'submit', class: 'primary' }, '应用');
    const resetBtn = h('button', {
      type: 'button',
      onclick: (e) => apply(() => post('filter/reset'), e.currentTarget).then(renderForm),
    }, '恢复默认');
    const readForm = () => {
      const next = { ...st.filter, show_gone: gone.checked, show_rented: rentedBox.checked };
      next.gpus = gpuBoxes.filter((g) => g.box.checked).map((g) => g.box.value);
      for (const [key, { int, el }] of Object.entries(inputs)) {
        const v = Number(el.value) || 0;
        next[key] = int ? Math.round(v) : v;
      }
      return next;
    };
    const form = h('form', {
      onsubmit: (e) => {
        e.preventDefault();
        apply(() => put('filter', readForm()), applyBtn).then(renderForm);
      },
    },
    h('details', { class: 'gpu-pick' },
      h('summary', null, `型号：${f.gpus.length ? f.gpus.join('、') : '全部'}`,
        h('span', { class: 'gpu-search-wrap' }, search, matchInfo)),
      h('p', { class: 'hint' }, '只抓取勾选的型号（和填了的 GPU 数），都不勾则抓全部；型号或 GPU 数有变化时立即重新抓取。'),
      h('div', { class: 'gpu-list' }, gpuBoxes.map((g) => g.label))),
    groups,
    h('div', { class: 'toolbar' },
      applyBtn, resetBtn,
      h('label', { class: 'field inline', title: '列出 vast 上眼下被别人租着的机器（看行情、记下好机器等它空出来）；租不了，没有开机按钮' }, rentedBox, '显示被租走的'),
      h('label', { class: 'field inline' }, gone, '显示已下架'),
      h('span', { class: 'hint' }, '0 或留空 = 不限，条件保存在服务端。')));
    mount(formBox, h('section', { class: 'card' }, h('h2', null, '过滤条件'), presetBar(readForm), form));
  }

  pollBtn.addEventListener('click', () => apply(() => post('poll'), pollBtn));

  renderForm();
  renderTable();
  ctx.show(
    h('div', { class: 'title-row' }, h('h1', null, '机器'), h('span', { class: 'spacer' }), pollBtn),
    message, stats, formBox,
    h('section', { class: 'card' }, h('div', { class: 'table-tools' }, firstToggle), tableBox, geoSwitch),
    h('p', { class: 'hint' },
      '$/h 已含磁盘费（按 ' + st.disk + ' GB 计）。估算 $/天 = 单价 × 24 + 每天下行 GB × 下行流量费 + 每天上行 GB × 上行流量费（$/TB 按 1024 GB 折算），后半段就是“预估流量费”那列。',
      '鼠标停在 GPU 名上可以看 vast 给的算力参考值（DLPerf、TFLOPS、DLP/$/h），不是实测。',
      '机器号后面的蓝色星是被租监控：点亮（★）后每 5 分钟记一次这台机器有没有被租，保留 30 天，星旁边是近 30 天被租率；详情和图在机器详情页，全部列表在“收藏”页。',
      '“各项最佳”只比在线、符合条件的机器，每项单独比，不合成总分；鼠标停在卡片上有算法说明。',
      '流量费颜色：', h('span', { class: 'cost-ok' }, '< 8'), ' / ', h('span', { class: 'cost-warn' }, '8–20'), ' / ',
      h('span', { class: 'cost-bad' }, '≥ 20'), ' $/TB。', `每 ${POLL_MS / 1000} 秒自动刷新列表。`));

  // Each refresh is scheduled after the previous one ends, so they never
  // overlap; the form is left alone so edits in progress survive.
  let timer = 0;
  async function refresh() {
    try {
      st = await get('state', signal);
      mount(message);
      renderTable();
    } catch (err) {
      if (!isAbort(err)) mount(message, errorBox(err));
    }
    if (!signal.aborted) timer = setTimeout(refresh, POLL_MS);
  }
  timer = setTimeout(refresh, POLL_MS);
  signal.addEventListener('abort', () => clearTimeout(timer));
}
