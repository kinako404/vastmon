import { h, table, empty } from '../lib/dom.js';
import { get } from '../lib/api.js';
import { fmtTime, fmtAgo, fmtDuration } from '../lib/format.js';

export async function pollsView(ctx) {
  const d = await get('polls', ctx.signal);
  ctx.show(
    h('h1', null, '抓取记录'),
    h('p', { class: 'hint' }, `每 ${fmtDuration(d.interval_sec)}自动抓取一次，型号变化或点“立即抓取”时会额外抓取。显示最近 200 次。`),
    h('section', { class: 'card' },
      d.polls.length
        ? table([
          { title: '时间', class: 'nowrap', cell: (p) => fmtTime(p.ts) },
          { title: '', class: 'nowrap muted', cell: (p) => fmtAgo(p.ts) },
          { title: '报价数', class: 'num', cell: (p) => String(p.n) },
          {
            title: '结果',
            cell: (p) => (p.err
              ? [h('span', { class: 'badge bad' }, '失败'), ' ', h('span', { class: 'mono' }, p.err)]
              : h('span', { class: 'badge on' }, '成功')),
          },
        ], d.polls)
        : empty('还没有抓取过')));
}
