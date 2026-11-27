import { h, mount, table, kv, empty, errorBox } from '../lib/dom.js';
import { summaryLine, report } from '../lib/bench.js';
import { openDialog } from '../lib/dialog.js';
import { get, currentSession } from '../lib/api.js';
import { openRentDialog } from './rent.js';
import { marksFrom, markButtons, markBadge } from '../lib/marks.js';
import { fmtGeo } from '../lib/geo.js';
import { watchMap, rentStar, rentChart, pct, nowText } from '../lib/rentwatch.js';
import { fmtTime, fmtAgo, fixed, gb, days, costClass, cores } from '../lib/format.js';

function cost(v) {
  return h('span', { class: costClass(v) }, '$' + fixed(v, 2));
}

export async function machineView(ctx) {
  const { mid, n } = ctx.params;
  const [d, fv, bm, bl, rw, rs] = await Promise.all([
    get(`machines/${encodeURIComponent(mid)}/${encodeURIComponent(n)}`, ctx.signal),
    get('blacklist', ctx.signal),
    get('benchmarks', ctx.signal),
    get('baselines', ctx.signal),
    get('rentwatch', ctx.signal),
    get(`rentwatch/${encodeURIComponent(mid)}`, ctx.signal),
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
  const rentBox = h('section', { class: 'card' });
  const showRent = (list, series) => {
    const w = watchMap(list).get(m.machine_id);
    const head = h('h2', null, '被租监控 ', rentStar(watchMap(list), m.machine_id, currentSession().logged_in, async (next) => {
      try {
        showRent(next, (await get(`rentwatch/${m.machine_id}`)).series);
      } catch (err) {
        mount(rentBox, head, errorBox(err));
      }
    }, (err) => mount(rentBox, head, errorBox(err))));
    const hasData = series.total.some((t) => t >= 0);
    mount(rentBox, head,
      w ? kv([
        ['现在', `${nowText(w)}${w.check_err ? `（上次采样出错：${w.check_err}）` : ''}`],
        ['被租率', `近 24 小时 ${pct(w.day)} · 近 7 天 ${pct(w.week)} · 近 30 天 ${pct(w.month)}`],
        ['采样', `${w.month.samples} 次${w.month.offline ? `，其中 ${w.month.offline} 次没挂出来（不算进被租率）` : ''}；每 5 分钟一次，保留 30 天`],
      ]) : h('p', { class: 'hint' }, hasData
        ? '监控已关闭，下面是之前的记录（30 天后过期）。点星重新开启。'
        : '没开。点标题旁的星开启：每 5 分钟记一次这台机器有几张卡被租着，保留 30 天，算近一个月的被租率。'),
      hasData ? rentChart(series) : null,
      h('p', { class: 'hint' }, '被租率 = 被租的卡 ÷ 挂出来的卡，按每次采样累计；多卡机器租掉一部分卡按比例算。按机器号查 vast 每次都给全，结果是准的；你自己租着也算被租。'));
  };
  showRent(rw.watches, rs.series);
  ctx.show(
    h('p', null, h('a', { href: '#/' }, '← 返回机器列表')),
    h('div', { class: 'title-row' },
      h('h1', null, `机器 ${m.machine_id} · ${m.num_gpus}× ${m.gpu_name}`),
      m.online ? h('span', { class: 'badge on' }, '在线')
        : m.rented ? h('span', { class: 'badge warn', title: `${fmtAgo(m.rented_seen)}看到被租着${m.mine ? '（你自己的实例）' : ''}${m.last_seen ? `；上次能租是 ${fmtAgo(m.last_seen)}` : ''}` }, m.mine ? '你在租' : '被租走')
          : h('span', { class: 'badge' }, '已下架'),
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
          ['功耗上限', m.gpu_max_power ? `${fixed(m.gpu_max_power, 0)}W / 卡（vast 报的，和卡上 nvidia-smi 的生效上限一致）` : '—'],
          ['磁盘', `${fixed(m.disk_space, 0)} GB，${fixed(m.disk_bw, 0)} MB/s`],
          ['可靠性 / CUDA / 端口', `${fixed(m.reliability, 3)} / ${fixed(m.cuda_max_good, 1)} / ${m.direct_port_count}`],
          ['可租天数', days(m.duration)],
          ['最近报价 ID / 主机', h('span', { class: 'mono' }, `${m.id} / ${m.host_id}`)],
          ['首次出现', fmtTime(m.first_seen)],
          ['最后能租', m.last_seen ? `${fmtTime(m.last_seen)}（${fmtAgo(m.last_seen)}）` : '面板还没见过它空着'],
          m.rented_seen ? ['最后看到被租着', `${fmtTime(m.rented_seen)}（${fmtAgo(m.rented_seen)}）`] : null,
        ]))),
    rentBox,
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
