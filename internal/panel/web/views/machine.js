import { h, mount, table, kv, empty } from '../lib/dom.js';
import { summaryLine, report } from '../lib/bench.js';
import { openDialog } from '../lib/dialog.js';
import { get, currentSession } from '../lib/api.js';
import { openRentDialog } from './rent.js';
import { marksFrom, markButtons, markBadge } from '../lib/marks.js';
import { fmtGeo } from '../lib/geo.js';
import { fmtTime, fmtAgo, fixed, gb, days, costClass, cores } from '../lib/format.js';

function cost(v) {
  return h('span', { class: costClass(v) }, '$' + fixed(v, 2));
}

export async function machineView(ctx) {
  const { mid, n } = ctx.params;
  const [d, fv, bm, bl] = await Promise.all([
    get(`machines/${encodeURIComponent(mid)}/${encodeURIComponent(n)}`, ctx.signal),
    get('blacklist', ctx.signal),
    get('benchmarks', ctx.signal),
    get('baselines', ctx.signal),
  ]);
  const m = d.machine;
  const f = d.filter;
  const benches = bm.benchmarks;
  const baselines = bl.baselines;
  const mine = benches.filter((b) => b.machine_id === m.machine_id);
  const showReport = (b) => {
    const dlg = openDialog({ wide: true });
    dlg.dialog.classList.add('bench');
    mount(dlg.body, h('h2', null, `算力检测：机器 ${b.machine_id} · 实例 ${b.instance_id}`), report(b, benches, baselines).node,
      h('div', { class: 'toolbar end' }, h('button', { type: 'button', onclick: dlg.close }, '关闭')));
  };
  const favBox = h('span', { class: 'mark-box' });
  let marks = marksFrom(fv);
  const showMarks = (res) => {
    marks = marksFrom(res);
    mount(favBox, currentSession().logged_in ? markButtons(m, marks, '机器详情页', showMarks) : markBadge(marks, m.machine_id));
  };
  showMarks(fv);
  ctx.show(
    h('p', null, h('a', { href: '#/' }, '← 返回机器列表')),
    h('div', { class: 'title-row' },
      h('h1', null, `机器 ${m.machine_id} · ${m.num_gpus}× ${m.gpu_name}`),
      m.online ? h('span', { class: 'badge on' }, '在线') : h('span', { class: 'badge' }, '已下架'),
      currentSession().logged_in && m.online
        ? h('button', { type: 'button', class: 'primary', onclick: () => openRentDialog(m, null, { blacklisted: marks.black.get(m.machine_id) }) }, '开机') : null,
      favBox),
    h('div', { class: 'cols' },
      h('section', { class: 'card' },
        h('h2', null, '价格'),
        kv([
          ['单价', `$${fixed(m.dph_total, 3)}/h`],
          ['其中磁盘', `$${fixed(m.storage_total_cost, 3)}/h（$${fixed(m.storage_cost, 3)} /GB/月 × ${fixed(d.disk, 0)} GB）`],
          ['竞价最低', `$${fixed(m.min_bid, 3)}/h`],
          ['估算日费', [h('span', { class: 'strong' }, '$' + fixed(m.est_day, 2)),
            h('span', { class: 'muted' }, `（每天下行 ${f.exp_down_gb} GB、上行 ${f.exp_up_gb} GB）`)]],
          ['下行流量费', [cost(m.internet_down_cost_per_tb), ' / TB']],
          ['上行流量费', [cost(m.internet_up_cost_per_tb), ' / TB']],
        ])),
      h('section', { class: 'card' },
        h('h2', null, '配置'),
        kv([
          ['位置', h('span', { title: m.geolocation || '' }, fmtGeo(m.geolocation))],
          ['带宽', `下行 ${fixed(m.inet_down, 0)} / 上行 ${fixed(m.inet_up, 0)} Mbps`],
          ['CPU', `${m.cpu_name || '未知型号'}，分到 ${cores(m.cpu_cores_effective)} 核 / 整机 ${cores(m.cpu_cores)}（vast 按 vCPU 线程计数）`],
          ['内存', `${gb(m.cpu_ram)} GB`],
          ['显存', `${gb(m.gpu_ram)} GB`],
          ['算力（vast 参考值）', m.dlperf
            ? `DLPerf ${fixed(m.dlperf, 1)}（${fixed(m.dph_total ? m.dlperf / m.dph_total : 0, 1)} DLP/$/h）· ${fixed(m.total_flops, 1)} TFLOPS`
            : '—'],
          ['磁盘', `${fixed(m.disk_space, 0)} GB，${fixed(m.disk_bw, 0)} MB/s`],
          ['可靠性 / CUDA / 端口', `${fixed(m.reliability, 3)} / ${fixed(m.cuda_max_good, 1)} / ${m.direct_port_count}`],
          ['可租天数', days(m.duration)],
          ['最近报价 ID / 主机', h('span', { class: 'mono' }, `${m.id} / ${m.host_id}`)],
          ['首次出现', fmtTime(m.first_seen)],
          ['最后出现', `${fmtTime(m.last_seen)}（${fmtAgo(m.last_seen)}）`],
        ]))),
    h('section', { class: 'card' },
      h('h2', null, `算力检测记录（${mine.length}）`),
      mine.length
        ? table([
          { title: '时间', class: 'nowrap', cell: (b) => fmtTime(b.ts) },
          { title: '实例', class: 'mono', cell: (b) => String(b.instance_id) },
          { title: '结果', cell: (b) => h('button', { type: 'button', class: 'link', title: '看完整结果', onclick: () => showReport(b) }, summaryLine(b, benches, baselines)) },
        ], mine)
        : empty('还没测过。开机后在“实例”页的实例卡片上点“算力检测”。')),
    h('section', { class: 'card' },
      h('h2', null, '价格变化'),
      d.history.length
        ? table([
          { title: '时间', class: 'nowrap', cell: (p) => fmtTime(p.ts) },
          { title: '$/h', class: 'num', cell: (p) => fixed(p.dph, 3) },
          { title: '下行 $/TB', class: 'num', cell: (p) => h('span', { class: costClass(p.down_tb) }, fixed(p.down_tb, 2)) },
          { title: '上行 $/TB', class: 'num', cell: (p) => h('span', { class: costClass(p.up_tb) }, fixed(p.up_tb, 2)) },
        ], d.history)
        : empty('没有记录')),
    h('details', { class: 'card' },
      h('summary', null, '原始数据（最近一次报价）'),
      h('pre', { class: 'code' }, JSON.stringify(d.raw, null, 2))));
}
