import { h, table } from './dom.js';
import { fixed, fmtAgo, fmtTime } from './format.js';

// Judging a quick benchmark (bench.py) against the performance baseline
// database (设置 → 性能基准数据库). A baseline row is one vast GPU model at
// one power level, with spec values (SMs, VRAM, bandwidth, default power,
// compute capability) and, when someone has measured a good card, measured
// references for the four throughput metrics.
//
// References for a metric, best first:
//   1. the matching baseline row's measured value;
//   2. the best result so far for the same model at a similar power limit;
//   3. for copy bandwidth only: 80% of the spec bandwidth (a good RTX PRO 6000
//      WS copies at 82% of spec), else 2 × vast's gpu_mem_bw.
// ≥ 85% of the reference is fine, 65–85% low, below 65% suspicious. Identity
// checks (model name, VRAM, SM count, VRAM fill, result correctness) fail
// hard; a power limit well below the model's default is reported as low.

const METRICS = [
  ['fp16', 'FP16 矩阵乘', 'TFLOPS', 1],
  ['bf16', 'BF16 矩阵乘', 'TFLOPS', 1],
  ['fp32', 'FP32 矩阵乘', 'TFLOPS', 1],
  ['copy_gbs', '显存拷贝', 'GB/s', 0],
];

const LEVEL_TEXT = { ok: '正常', warn: '偏低', bad: '疑似假卡', none: '无参考' };
const COPY_OF_SPEC = 0.8;

function level(ratio) {
  if (!Number.isFinite(ratio)) return 'none';
  if (ratio >= 0.85) return 'ok';
  if (ratio >= 0.65) return 'warn';
  return 'bad';
}

const numOr = (v) => {
  const n = parseFloat(v);
  return Number.isFinite(n) ? n : null;
};

// power reads the card's power limits from a result: enforced limit, default,
// max, and what the sustained load drew.
export function power(r) {
  const g = (r && r.smi && r.smi[0]) || {};
  const s = (r && r.sustain) || {};
  return {
    limit: numOr(g['enforced.power.limit']) ?? numOr(g['power.limit']),
    def: numOr(g['power.default_limit']),
    max: numOr(g['power.max_limit']),
    peak: numOr(s.power_peak_w),
    avg: numOr(s.power_avg_w),
  };
}

const near = (a, b, tol) => a && b && Math.abs(a - b) / b <= tol;

// pickBaseline chooses the baseline row for benchmark b: same vast model,
// matching VRAM size when variants differ, power closest to the card's
// limit; own rows win over built-in ones, rows with measurements over rows
// without.
export function pickBaseline(b, baselines) {
  const r = b.result || {};
  const pw = power(r).limit;
  const rows = (baselines || []).filter((x) => x.gpu_name === b.gpu_name);
  if (!rows.length) return null;
  const vram = r.vram_gb || (b.vast_gpu_ram ? b.vast_gpu_ram / 1024 : null);
  const score = (x) => {
    let s = 0;
    if (vram && x.vram_gb) s += near(vram, x.vram_gb, 0.1) ? 0 : 1000;
    if (pw && x.power_w) s += Math.abs(x.power_w - pw);
    if (!(x.fp16 > 0)) s += 50;
    if (x.builtin) s += 1;
    return s;
  };
  return [...rows].sort((a, c) => score(a) - score(c))[0];
}

// bestByModel keeps, per metric, the best value among successful runs of the
// same model at a power limit within 15% of pw (any power when pw is unknown).
function bestByModel(all, model, skipID, pw) {
  const best = {};
  for (const b of all) {
    if (b.id === skipID || b.gpu_name !== model || !b.result || !b.result.ok) continue;
    const other = power(b.result).limit;
    if (pw && other && !near(other, pw, 0.15)) continue;
    for (const [k] of METRICS) {
      const v = b.result[k];
      if (Number.isFinite(v) && (!best[k] || v > best[k].value)) best[k] = { value: v, bench: b };
    }
  }
  return best;
}

function nameMatches(vastName, torchName) {
  if (!vastName || !torchName) return null;
  const t = torchName.toUpperCase();
  const tokens = vastName.toUpperCase().split(/[\s_-]+/).filter((w) => /\d/.test(w) || w === 'PRO');
  return tokens.every((w) => t.includes(w));
}

function healthProblems(r) {
  const out = [];
  for (const g of r.smi || []) {
    if (g['retired_pages.pending'] === 'Yes') out.push(`${g.name} 显存有坏页等待隔离（出过 ECC 不可纠正错误），CUDA 用不了，要重启宿主机才能恢复`);
    const vol = parseInt(g['ecc.errors.uncorrected.volatile.total'], 10);
    if (vol > 0) out.push(`${g.name} 本次开机后有 ${vol} 个 ECC 不可纠正错误`);
    if (g.compute_mode && !/default/i.test(g.compute_mode)) out.push(`${g.name} 计算模式是 ${g.compute_mode}，别的进程可能占着卡`);
  }
  return out;
}

const W = (v) => (v ? `${fixed(v, 0)} W` : '?');

// evaluate turns benchmark b into rows and a verdict.
export function evaluate(b, all, baselines) {
  const r = b.result || {};
  const pw = power(r);
  const base = pickBaseline(b, baselines);
  const problems = [];
  let worst = 'ok';
  const bump = (lv) => {
    const order = ['ok', 'none', 'warn', 'bad'];
    if (order.indexOf(lv) > order.indexOf(worst)) worst = lv;
  };
  const health = healthProblems(r);
  if (b.err || !r.ok) {
    if (health.length) return { verdict: 'bad', rows: [], ident: [], problems: health, detail: r.error || b.err, summary: '坏卡', r, broken: true, base, pw };
    return { verdict: 'error', rows: [], problems: [b.err || r.error || '检测没跑完'], summary: '检测失败', r, base, pw };
  }
  for (const p of health) problems.push(p);
  if (health.length) bump('bad');

  const measuredPowerOK = base && base.fp16 > 0 && (!pw.limit || !base.power_w || near(pw.limit, base.power_w, 0.15));
  const hist = bestByModel(all || [], b.gpu_name, b.id, pw.limit);
  const rows = [];
  for (const [key, label, unit, digits] of METRICS) {
    const v = r[key];
    if (!Number.isFinite(v)) {
      if (r.errors && r.errors[key]) problems.push(`${label} 出错：${r.errors[key]}`);
      rows.push({ label, value: v === null && (r.skipped || []).some((s) => s.startsWith(key)) ? '不支持' : '—', ref: '—', ratio: '', lv: 'none' });
      continue;
    }
    let ref = null;
    let src = '';
    if (measuredPowerOK && base[key] > 0) {
      ref = base[key];
      src = `基准库：${base.gpu_name}${base.variant ? ' ' + base.variant : ''} ${W(base.power_w)}（${base.measured_source || '实测'}）`;
    } else if (hist[key]) {
      ref = hist[key].value;
      src = `同型号历史最好（机器 ${hist[key].bench.machine_id}，${fmtTime(hist[key].bench.ts).slice(0, 10)}）`;
    } else if (key === 'copy_gbs' && base && base.mem_bw_gbs > 0) {
      ref = base.mem_bw_gbs * COPY_OF_SPEC;
      src = `规格带宽 ${fixed(base.mem_bw_gbs, 0)} GB/s 的 80%`;
    } else if (key === 'copy_gbs' && b.vast_mem_bw > 0) {
      ref = b.vast_mem_bw * 2;
      src = 'vast 标称显存带宽 × 2';
    }
    const ratio = ref ? v / ref : NaN;
    const lv = level(ratio);
    bump(lv === 'none' ? 'ok' : lv);
    if (lv === 'bad') problems.push(`${label}只有参考值的 ${fixed(ratio * 100, 0)}%`);
    rows.push({ label, value: `${fixed(v, digits)} ${unit}`, ref: ref ? `${fixed(ref, digits)} ${unit}` : '—', ratio: ref ? `${fixed(ratio * 100, 0)}%` : '', lv, src });
  }
  const s = r.sustain;
  if (s && Number.isFinite(s.tflops)) {
    const burst = r.fp16;
    const ratio = burst ? s.tflops / burst : NaN;
    const lv = Number.isFinite(ratio) && ratio < 0.85 ? 'warn' : 'ok';
    if (lv === 'warn') problems.push(`持续满载只有短跑的 ${fixed(ratio * 100, 0)}%，可能过热或功耗墙降频`);
    bump(lv);
    rows.push({
      label: `持续满载 ${fixed(s.seconds, 0)} 秒（FP16）`, value: `${fixed(s.tflops, 1)} TFLOPS`,
      ref: burst ? `短跑 ${fixed(burst, 1)}` : '—', ratio: burst ? `${fixed(ratio * 100, 0)}%` : '', lv,
      src: `满载峰值 ${W(s.power_peak_w)} · 平均 ${W(s.power_avg_w)} · SM ${s.clock_sm_mhz || '?'} MHz · 最高 ${s.temp_max_c ?? '?'}°C`,
    });
  }

  const ident = [];
  const nm = nameMatches(b.gpu_name, r.name);
  ident.push({ label: '型号', value: r.name || '—', ref: `vast：${b.gpu_name}${base && base.full_name ? `（${base.full_name}）` : ''}`, lv: nm === false ? 'bad' : 'ok' });
  if (nm === false) problems.push(`实际型号是 ${r.name}，vast 标的是 ${b.gpu_name}`);
  const vastGB = b.vast_gpu_ram ? b.vast_gpu_ram / 1024 : null;
  const vramOK = vastGB ? Math.abs(r.vram_gb - vastGB) / vastGB < 0.05 : true;
  ident.push({ label: '显存容量', value: `${fixed(r.vram_gb, 1)} GB`, ref: [vastGB ? `vast：${fixed(vastGB, 1)} GB` : null, base && base.vram_gb ? `规格：${fixed(base.vram_gb, 0)} GB` : null].filter(Boolean).join(' · ') || '—', lv: vramOK ? 'ok' : 'bad' });
  if (!vramOK) problems.push(`显存只有 ${fixed(r.vram_gb, 1)} GB，vast 标 ${fixed(vastGB, 1)} GB`);
  const v = r.vram;
  if (v) {
    ident.push({ label: '显存写满校验', value: `写入 ${fixed(v.filled_gb, 1)} / ${fixed(v.target_gb, 1)} GB，坏块 ${v.bad_blocks}`, ref: `空闲 ${fixed(v.free_gb, 1)} GB 的 92%`, lv: v.ok ? 'ok' : 'bad' });
    if (!v.ok) problems.push(v.bad_blocks ? `显存读回有 ${v.bad_blocks} 块数据不对` : `显存只写进去 ${fixed(v.filled_gb, 1)} GB（应能写 ${fixed(v.target_gb, 1)} GB）`);
  }
  const c = r.correct;
  if (c) {
    ident.push({ label: '计算结果校验', value: `FP32 误差 ${c.fp32_rel_err.toExponential(1)}，FP16 误差 ${c.fp16_rel_err.toExponential(1)}`, ref: 'FP32 < 1e-4，FP16 < 2e-2', lv: c.ok ? 'ok' : 'bad' });
    if (!c.ok) problems.push('GPU 算出来的结果和 CPU 对不上');
  }
  const refSMs = base && base.sms;
  const smBad = refSMs && r.sms !== refSMs;
  const ccBad = base && base.cc && r.cc && r.cc !== base.cc;
  ident.push({ label: 'SM 数 / 算力版本', value: `${r.sms} / ${r.cc}`, ref: base ? `${refSMs || '?'} / ${base.cc || '?'}` : '—', lv: smBad || ccBad ? 'bad' : 'ok' });
  if (smBad) problems.push(`SM 数是 ${r.sms}，规格是 ${refSMs}`);
  if (ccBad) problems.push(`算力版本是 ${r.cc}，规格是 ${base.cc}`);
  if (pw.limit) {
    const std = base && base.power_w;
    const limited = std && pw.limit < std * 0.9;
    ident.push({
      label: '功耗上限', value: `${W(pw.limit)}（默认 ${W(pw.def)} · 最大 ${W(pw.max)}）`,
      ref: std ? `基准：${W(std)}${base.variant ? ' · ' + base.variant : ''}` : '—', lv: limited ? 'warn' : 'ok',
    });
    if (limited) problems.push(`功耗被限到 ${W(pw.limit)}（这个型号标准 ${W(std)}），算力会偏低`);
  }
  for (const i of ident) bump(i.lv);

  const verdict = worst === 'none' ? 'ok' : worst;
  return { verdict, rows, ident, problems, r, summary: LEVEL_TEXT[verdict], base, pw };
}

export function verdictBadge(ev) {
  const cls = { ok: 'on', warn: 'warn', bad: 'bad', error: 'bad' }[ev.verdict] || '';
  const text = ev.broken ? '坏卡' : ev.verdict === 'error' ? '检测失败' : LEVEL_TEXT[ev.verdict];
  return h('span', { class: 'badge ' + cls }, text);
}

export function summaryLine(b, all, baselines) {
  const ev = evaluate(b, all, baselines);
  const r = ev.r || {};
  const parts = ev.verdict === 'error' || ev.broken ? [ev.problems[0] ? ev.problems[0].slice(0, 80) : '']
    : [`FP16 ${fixed(r.fp16, 1)}`, r.bf16 ? `BF16 ${fixed(r.bf16, 1)}` : null, `FP32 ${fixed(r.fp32, 1)} TFLOPS`, `拷贝 ${fixed(r.copy_gbs, 0)} GB/s`,
      ev.pw.limit ? W(ev.pw.limit) : null].filter(Boolean);
  return h('span', { class: 'bench-line', title: ev.problems.join('\n') || '各项都正常' },
    verdictBadge(ev), ' ', parts.join(' · '), h('span', { class: 'hint' }, ` · ${fmtAgo(b.ts)}`));
}

// powerBadge is the card's power limit from benchmark b, for the instance
// card header; null when b did not read it.
export function powerBadge(b) {
  if (!b || !b.result) return null;
  const pw = power(b.result);
  if (!pw.limit) return null;
  const tip = [`功耗上限 ${W(pw.limit)}（默认 ${W(pw.def)} · 最大 ${W(pw.max)}）`,
    pw.peak ? `算力检测满载峰值 ${W(pw.peak)}，平均 ${W(pw.avg)}` : null,
    `${fmtTime(b.ts)} 算力检测读到`].filter(Boolean).join('\n');
  const low = pw.def && pw.limit < pw.def * 0.9;
  return h('span', { class: low ? 'badge warn' : 'badge', title: tip }, `${W(pw.limit)}${low ? '（被限）' : ''}`);
}

// asBaseline turns a good benchmark into a baseline row for the database.
export function asBaseline(b, ev) {
  const r = b.result || {};
  return {
    id: ev.base && !ev.base.builtin && near(ev.pw.limit, ev.base.power_w, 0.05) ? ev.base.id : 0,
    gpu_name: b.gpu_name,
    variant: ev.base ? ev.base.variant : '',
    full_name: (ev.base && ev.base.full_name) || r.name || '',
    power_w: ev.pw.limit || (ev.base && ev.base.power_w) || 0,
    sms: r.sms || 0,
    cc: r.cc || '',
    vram_gb: (ev.base && ev.base.vram_gb) || Math.round(r.vram_gb || 0),
    mem_bw_gbs: (ev.base && ev.base.mem_bw_gbs) || 0,
    fp32_spec: (ev.base && ev.base.fp32_spec) || 0,
    fp16: r.fp16 || 0,
    bf16: r.bf16 || 0,
    fp32: r.fp32 || 0,
    copy_gbs: r.copy_gbs || 0,
    measured_source: `实测：机器 ${b.machine_id} · 实例 ${b.instance_id} · ${fmtTime(b.ts).slice(0, 10)}`,
    spec_source: (ev.base && ev.base.spec_source) || '',
    note: (ev.base && ev.base.note) || '',
  };
}

const LV_CLASS = { ok: 'cost-ok', warn: 'cost-warn', bad: 'cost-bad', none: 'muted' };

// report is the full result: verdict, problems, metrics and identity tables.
export function report(b, all, baselines) {
  const ev = evaluate(b, all, baselines);
  const r = ev.r || {};
  const smi = (r.smi && r.smi[0]) || {};
  const head = h('div', { class: `bench-verdict v-${ev.verdict}` },
    verdictBadge(ev), ' ',
    ev.broken ? ev.problems[0]
      : ev.verdict === 'ok' ? '各项都在参考值的 85% 以上，卡和显存都对得上。'
        : ev.verdict === 'error' ? '没拿到完整结果。' : ev.problems.join('；') || '有几项偏低。');
  if (ev.verdict === 'error' || ev.broken) {
    return { ev, node: h('div', null, head, h('pre', { class: 'code' }, [...(ev.broken ? ev.problems.slice(1) : ev.problems), ev.detail].filter(Boolean).join('\n')),
      r.name ? h('p', { class: 'hint' }, `PyTorch 看到的卡：${r.name}（算力 ${r.cc}，${r.sms} 个 SM，${fixed(r.vram_gb, 1)} GB）· 功耗上限 ${W(ev.pw.limit)} · ${r.python || ''} torch ${r.torch || ''}`) : null) };
  }
  const lvCell = (row) => h('span', { class: LV_CLASS[row.lv] }, LEVEL_TEXT[row.lv] === '无参考' ? '—' : LEVEL_TEXT[row.lv]);
  return {
    ev,
    node: h('div', null, head,
      table([
        { title: '项目', cell: (x) => x.label },
        { title: '结果', class: 'num strong', cell: (x) => x.value },
        { title: '参考', class: 'num', cell: (x) => h('span', { title: x.src || '' }, x.ref) },
        { title: '比例', class: 'num', cell: (x) => x.ratio },
        { title: '判断', cell: lvCell },
      ], ev.rows),
      r.sustain && r.sustain.power_peak_w ? h('p', { class: 'hint' },
        `持续满载：峰值 ${W(r.sustain.power_peak_w)} · 平均 ${W(r.sustain.power_avg_w)} · SM 频率 ${r.sustain.clock_sm_mhz} MHz · 最高 ${r.sustain.temp_max_c}°C`) : null,
      h('h3', null, '身份核对'),
      table([
        { title: '项目', cell: (x) => x.label },
        { title: '实测', cell: (x) => x.value },
        { title: '应为', cell: (x) => x.ref },
        { title: '判断', cell: (x) => h('span', { class: LV_CLASS[x.lv] }, x.lv === 'ok' ? '对得上' : x.lv === 'warn' ? '偏低' : '不对') },
      ], ev.ident),
      h('p', { class: 'hint' },
        `驱动 ${smi.driver_version || '?'} · 最高 SM 频率 ${smi['clocks.max.sm'] || '?'} MHz · `,
        `PCIe ${smi['pcie.link.gen.current'] || '?'}.0 ×${smi['pcie.link.width.current'] || '?'} · ${r.python || ''} torch ${r.torch || ''} · 用时 ${fixed(r.seconds, 0)} 秒`),
      ev.rows.some((x) => x.src) ? h('p', { class: 'hint' }, '参考值来源：', [...new Set(ev.rows.slice(0, 4).map((x) => x.src).filter(Boolean))].join('；')) : null),
  };
}
