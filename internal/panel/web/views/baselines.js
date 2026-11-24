import { h, mount, errorBox, loading, empty, field, table } from '../lib/dom.js';
import { get, put, post, del } from '../lib/api.js';
import { openDialog } from '../lib/dialog.js';
import { fixed } from '../lib/format.js';

// The performance baseline database on the settings page: one row per vast
// GPU model and power level, spec values for identity checks and measured
// references for throughput (see lib/bench.js for how they are used).

const FIELDS = [
  ['gpu_name', 'vast 型号名', 'text', '和 vast 列表里的写法一样，比如 RTX PRO 6000 WS'],
  ['variant', '版本', 'text', '同型号不同版本时区分，比如 80GB、SXM2 32GB；没有就空着'],
  ['full_name', '官方全名', 'text'],
  ['power_w', '功耗 W', 'number', '默认功耗；实测值是在什么功耗上限下测的'],
  ['sms', 'SM 数', 'number'],
  ['cc', '算力版本', 'text', '比如 8.9、9.0、12.0'],
  ['vram_gb', '显存 GB', 'number'],
  ['mem_bw_gbs', '规格显存带宽 GB/s', 'number'],
  ['fp32_spec', '规格 FP32 TFLOPS', 'number', '官方非 tensor 峰值，只作参考，不参与判断'],
  ['fp16', '实测 FP16 TFLOPS', 'number', '8192×8192 矩阵乘，没测过就空着'],
  ['bf16', '实测 BF16 TFLOPS', 'number'],
  ['fp32', '实测 FP32 TFLOPS', 'number', '关掉 TF32 的真 FP32'],
  ['copy_gbs', '实测显存拷贝 GB/s', 'number', '读 + 写都算'],
  ['measured_source', '实测来源', 'text'],
  ['spec_source', '规格来源', 'text', '网址'],
  ['note', '备注', 'text'],
];

const v = (x, d = 0) => (x > 0 ? fixed(x, d) : '—');

function sourceCell(b) {
  const links = (b.spec_source || '').split(/\s+/).filter((u) => /^https?:\/\//.test(u));
  return [
    b.measured_source ? h('div', null, b.measured_source) : null,
    links.length ? h('div', { class: 'hint' }, '规格：', links.map((u, i) => [i ? ' · ' : '', h('a', { href: u, target: '_blank', rel: 'noopener noreferrer' }, new URL(u).hostname)])) : null,
    b.note ? h('div', { class: 'hint' }, b.note) : null,
  ];
}

export function baselinesSection() {
  const box = h('div', null, loading());
  const msg = h('div');
  let list = [];

  function editDialog(row) {
    const d = openDialog({ wide: true });
    const inputs = {};
    const fieldEls = FIELDS.map(([key, label, type, hint]) => {
      inputs[key] = h('input', { type, step: type === 'number' ? 'any' : null, min: type === 'number' ? '0' : null, value: row[key] ? String(row[key]) : '' });
      return field(label, inputs[key], hint);
    });
    const err = h('div');
    const ok = h('button', { type: 'submit', class: 'primary' }, '保存');
    mount(d.body, h('form', {
      onsubmit: async (e) => {
        e.preventDefault();
        const body = { id: row.id || 0 };
        for (const [key, , type] of FIELDS) body[key] = type === 'number' ? Number(inputs[key].value) || 0 : inputs[key].value.trim();
        ok.disabled = true;
        try {
          list = (await put('baselines', body)).baselines;
          d.close();
          render();
        } catch (ex) {
          ok.disabled = false;
          mount(err, errorBox(ex));
        }
      },
    },
    h('h2', null, row.id ? `编辑：${row.gpu_name}${row.variant ? ' ' + row.variant : ''}` : '新增基准'),
    row.builtin ? h('p', { class: 'hint' }, '这是内置数据，保存后变成你自己的一行（内置数据更新时不会覆盖它）。') : null,
    h('div', { class: 'form-grid wide' }, fieldEls),
    err,
    h('div', { class: 'toolbar end' }, h('button', { type: 'button', onclick: d.close }, '取消'), ok)));
  }

  async function remove(b) {
    if (!window.confirm(`删除基准 ${b.gpu_name}${b.variant ? ' ' + b.variant : ''} ${v(b.power_w)} W？`)) return;
    try {
      list = (await del(`baselines/${b.id}`)).baselines;
      render();
    } catch (err) {
      mount(msg, errorBox(err));
    }
  }

  function render() {
    mount(box, list.length ? table([
      { title: '型号', cell: (b) => [h('span', { class: 'strong' }, b.gpu_name), b.variant ? h('span', { class: 'hint' }, ` ${b.variant}`) : null,
        b.builtin ? h('span', { class: 'badge', title: '内置数据' }, '内置') : h('span', { class: 'badge on', title: '自己加的或改过的' }, '自定义'),
        b.full_name ? h('div', { class: 'hint' }, b.full_name) : null] },
      { title: '功耗 W', class: 'num', cell: (b) => v(b.power_w) },
      { title: 'SM', class: 'num', cell: (b) => v(b.sms) },
      { title: '算力', class: 'num', cell: (b) => b.cc || '—' },
      { title: '显存 GB', class: 'num', cell: (b) => v(b.vram_gb) },
      { title: '带宽 GB/s', class: 'num', cell: (b) => v(b.mem_bw_gbs) },
      { title: 'FP32 规格', class: 'num', cell: (b) => v(b.fp32_spec, 1) },
      { title: '实测 FP16 / BF16 / FP32', class: 'num', cell: (b) => (b.fp16 > 0 ? `${v(b.fp16, 1)} / ${v(b.bf16, 1)} / ${v(b.fp32, 1)}` : h('span', { class: 'muted' }, '没测过')) },
      { title: '实测拷贝', class: 'num', cell: (b) => v(b.copy_gbs) },
      { title: '来源', cell: sourceCell },
      { title: '', class: 'nowrap', cell: (b) => [
        h('button', { type: 'button', class: 'small', onclick: () => editDialog(b) }, '编辑'), ' ',
        h('button', { type: 'button', class: 'small danger', onclick: () => remove(b) }, '删除')] },
    ], list) : empty('数据库是空的'));
  }

  const addBtn = h('button', { type: 'button', onclick: () => editDialog({}) }, '新增一行');
  const resetBtn = h('button', {
    type: 'button',
    onclick: async () => {
      if (!window.confirm('把内置数据恢复成程序自带的版本？自己加的、改过的行不受影响。')) return;
      try {
        list = (await post('baselines/reset')).baselines;
        render();
      } catch (err) {
        mount(msg, errorBox(err));
      }
    },
  }, '恢复内置数据');

  (async () => {
    try {
      list = (await get('baselines')).baselines;
      render();
    } catch (err) {
      mount(box, errorBox(err));
    }
  })();

  return h('section', { class: 'card' },
    h('h2', null, '性能基准数据库'),
    h('p', { class: 'hint' },
      '算力检测拿它判断真假卡：每行是一个 vast 型号 + 一个功耗档。规格（SM 数、显存、带宽、功耗、算力版本）来自 NVIDIA 官方和 TechPowerUp，用来核对身份；',
      '实测值只放在正常卡上真测过的，用来比算力。检测结论“正常”时可以在结果里点“存为基准”。同型号有几行时，按显存和最接近的功耗挑。'),
    h('div', { class: 'toolbar' }, addBtn, resetBtn),
    msg, box);
}
