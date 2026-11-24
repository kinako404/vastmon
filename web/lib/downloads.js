import { h, mount, errorBox, loading, empty, field } from './dom.js';
import { get, post } from './api.js';
import { openDialog } from './dialog.js';
import { fmtBytes } from './filebrowser.js';
import { fmtTime } from './format.js';

// The startup downloader: aria2 on the instance pulls the configured files
// (settings → 启动下载配置), optionally after pushing the ComfyUI input backup.

export const COMMON_DIRS = [
  'models/diffusion_models', 'models/checkpoints', 'models/vae', 'models/text_encoders', 'models/clip',
  'models/loras', 'models/upscale_models', 'models/latent_upscale_models', 'models/vae_approx',
  'models/clip_vision', 'models/controlnet', 'models/audio_encoders', 'models/refmods', 'custom_nodes', 'input',
];

const FILE_STATUS = {
  queued: ['等待', ''],
  downloading: ['下载中', 'warn'],
  done: ['完成', 'on'],
  exists: ['已存在', 'on'],
  failed: ['失败', 'bad'],
  nospace: ['空间不够', 'bad'],
  canceled: ['已取消', ''],
};

const JOB_STATUS = {
  waiting: ['等实例跑起来', 'warn'],
  running: ['进行中', 'warn'],
  done: ['全部完成', 'on'],
  partial: ['部分没完成', 'bad'],
  failed: ['失败', 'bad'],
  canceled: ['已取消', ''],
};

export const isActive = (j) => j && (j.status === 'waiting' || j.status === 'running');

export function bytes(n) {
  return n > 0 ? fmtBytes(n) : '—';
}

function fmtETA(sec) {
  if (!Number.isFinite(sec) || sec <= 0) return '';
  if (sec < 60) return `${Math.ceil(sec)} 秒`;
  if (sec < 3600) return `${Math.ceil(sec / 60)} 分`;
  return `${Math.floor(sec / 3600)} 小时 ${Math.ceil((sec % 3600) / 60)} 分`;
}

export function bar(frac, cls) {
  const f = Number.isFinite(frac) ? Math.max(0, Math.min(1, frac)) : 0;
  return h('span', { class: 'dl-bar' }, h('span', { class: 'dl-bar-fill' + (cls ? ' ' + cls : ''), style: `width:${(f * 100).toFixed(1)}%` }));
}

// totals sums what the job has left to fetch and how fast it goes.
function totals(j) {
  let size = 0;
  let done = 0;
  let speed = 0;
  for (const f of j.files) {
    if (f.status === 'exists' || f.status === 'canceled' || f.status === 'nospace' || f.status === 'failed') continue;
    size += Math.max(f.size, 0);
    done += f.status === 'done' ? Math.max(f.size, f.done) : Math.max(f.done, 0);
    if (f.status === 'downloading') speed += f.speed || 0;
  }
  return { size, done, speed };
}

function fileRow(f) {
  const [text, cls] = FILE_STATUS[f.status] || [f.status, ''];
  const frac = f.status === 'done' || f.status === 'exists' ? 1 : f.size > 0 ? f.done / f.size : 0;
  return h('div', { class: 'dl-row' },
    h('div', { class: 'dl-name', title: `${f.url}\n→ ${f.dir}/${f.name || ''}` },
      h('span', { class: 'mono' }, f.name || f.url), h('span', { class: 'hint' }, ` ${f.dir}`)),
    bar(frac, f.status === 'failed' || f.status === 'nospace' ? 'bad' : f.status === 'done' || f.status === 'exists' ? 'ok' : ''),
    h('span', { class: 'dl-num' }, f.status === 'exists' ? bytes(f.size) : `${bytes(f.done)} / ${bytes(f.size)}`),
    h('span', { class: 'dl-num' }, f.status === 'downloading' && f.speed > 0 ? `${fmtBytes(f.speed)}/s` : ''),
    h('span', { class: 'badge ' + cls, title: f.attempts ? `试了 ${f.attempts} 次` : '' }, text),
    f.err ? h('div', { class: f.status === 'downloading' || f.status === 'queued' ? 'dl-err hint' : 'dl-err cost-bad' },
      f.status === 'downloading' || f.status === 'queued' ? `上次出错（第 ${f.attempts} 次，会自动重试）：${f.err}` : f.err) : null);
}

function inputRow(inp) {
  const st = { queued: ['等待', ''], running: ['上传中', 'warn'], done: ['完成', 'on'], failed: ['失败', 'bad'] }[inp.status] || [inp.status, ''];
  return h('div', { class: 'dl-row' },
    h('div', { class: 'dl-name' }, h('span', null, 'ComfyUI input 同步'), h('span', { class: 'hint' }, ' backups/input.tar.gz → 工作目录/input/')),
    bar(inp.status === 'done' ? 1 : inp.total > 0 ? inp.done / inp.total : 0, inp.status === 'failed' ? 'bad' : inp.status === 'done' ? 'ok' : ''),
    h('span', { class: 'dl-num' }, inp.status === 'done' ? `${inp.files} 个文件` : `${bytes(inp.done)} / ${bytes(inp.total)}`),
    h('span', { class: 'dl-num' }),
    h('span', { class: 'badge ' + st[1] }, st[0]),
    inp.err ? h('div', { class: 'dl-err cost-bad' }, inp.err) : null);
}

// jobPanel renders one download job; open (a Set of job ids the user
// expanded or collapsed) keeps the fold state across refreshes.
export function jobPanel(j, { onCancel, folds }) {
  const active = isActive(j);
  const [text, cls] = JOB_STATUS[j.status] || [j.status, ''];
  const t = totals(j);
  const eta = t.speed > 0 ? (t.size - t.done) / t.speed : NaN;
  const open = folds.has(j.id) ? !active : active;
  const details = h('details', { class: 'dl-panel', open },
    h('summary', null,
      h('span', { class: 'strong' }, '启动下载器'),
      j.profile ? h('span', { class: 'badge', title: '下载配置' }, j.profile) : null,
      h('span', { class: 'badge ' + cls }, text),
      j.auto ? h('span', { class: 'hint' }, '开机自动') : null,
      active && j.phase ? h('span', { class: 'hint' }, j.phase) : null,
      !active && j.finished ? h('span', { class: 'hint' }, `${fmtTime(j.finished)} 结束`) : null,
      h('span', { class: 'spacer' }),
      t.size > 0 ? h('span', { class: 'dl-total' }, `${bytes(t.done)} / ${bytes(t.size)}`,
        t.speed > 0 ? ` · ${fmtBytes(t.speed)}/s` : '', eta ? ` · 剩约 ${fmtETA(eta)}` : '') : null,
      t.size > 0 ? bar(t.done / t.size) : null,
      active && onCancel ? h('button', {
        type: 'button', class: 'small danger',
        onclick: (e) => { e.preventDefault(); onCancel(j); },
      }, '取消') : null),
    j.err ? h('div', { class: 'error' }, j.err) : null,
    j.input && j.input.status ? inputRow(j.input) : null,
    j.files.map(fileRow));
  details.addEventListener('toggle', () => {
    if (details.open === active) folds.delete(j.id);
    else folds.add(j.id);
  });
  return details;
}

// latestJobs maps instance id → its newest job.
export function latestJobs(jobs) {
  const m = new Map();
  for (const j of jobs) if (!m.has(j.instance_id)) m.set(j.instance_id, j);
  return m;
}

// enabledSummary counts the enabled files of a download set.
export function enabledSummary(set) {
  const on = set.items.filter((it) => it.enabled);
  const size = on.reduce((s, it) => s + Math.max(it.size, 0), 0);
  return { count: on.length, size, unknown: on.some((it) => !(it.size > 0)) };
}

// openDownloadDialog lets the user pick files and start the downloader on
// instance x.
export function openDownloadDialog(x, onStarted) {
  const d = openDialog({ wide: true });
  mount(d.body, h('h2', null, `启动下载器：实例 ${x.id}`), loading());
  get('downloads/config').then(render, (err) => mount(d.body, errorBox(err),
    h('div', { class: 'toolbar end' }, h('button', { type: 'button', onclick: d.close }, '关闭'))));

  function render(cfg, pid = cfg.default) {
    const set = cfg.profiles.find((p) => p.id === pid) || cfg.profiles[0];
    const pick = h('select', { onchange: () => render(cfg, Number(pick.value)) },
      cfg.profiles.map((p) => h('option', { value: String(p.id), selected: p.id === set.id }, `${p.name}（${p.items.length} 个）${p.id === cfg.default ? ' · 默认' : ''}`)));
    const checks = new Map();
    const free = x.disk_space > 0 ? (x.disk_space - (x.disk_usage || 0)) * 1e9 : NaN;
    const archive = cfg.input_archive || {};
    const syncInput = h('input', { type: 'checkbox', checked: cfg.sync_input && archive.exists, disabled: !archive.exists });
    const sumBox = h('div', { class: 'price-box' });
    const msg = h('div');
    const ok = h('button', { type: 'submit', class: 'primary' }, '开始');
    const update = () => {
      const ids = [...checks].filter(([, c]) => c.checked).map(([id]) => id);
      const size = set.items.filter((it) => ids.includes(it.id)).reduce((s, it) => s + Math.max(it.size, 0), 0);
      const over = Number.isFinite(free) && size > free;
      mount(sumBox,
        h('div', { class: over ? 'cost-bad' : 'strong' }, `选了 ${ids.length} 个文件，共 ${bytes(size)}`,
          Number.isFinite(free) ? `，实例磁盘剩约 ${fmtBytes(free)}` : ''),
        over ? h('div', { class: 'hint' }, '装不下全部：按列表顺序下，放不下的文件会标“空间不够”跳过。实例上已有的同名同大小文件会直接跳过。')
          : h('div', { class: 'hint' }, '实例上已有的同名同大小文件会直接跳过；下了一半的会续传。'));
      ok.disabled = !ids.length && !syncInput.checked;
    };
    const rows = set.items.map((it) => {
      const c = h('input', { type: 'checkbox', checked: it.enabled, onchange: update });
      checks.set(it.id, c);
      return h('label', { class: 'dl-pick' }, c,
        h('span', { class: 'mono' }, it.name || it.url),
        h('span', { class: 'hint' }, it.dir),
        h('span', { class: 'dl-num' }, bytes(it.size)),
        it.note ? h('span', { class: 'hint' }, it.note) : null);
    });
    syncInput.addEventListener('change', update);
    mount(d.body, h('form', {
      onsubmit: async (e) => {
        e.preventDefault();
        ok.disabled = true;
        d.setBusy(true);
        try {
          const ids = [...checks].filter(([, c]) => c.checked).map(([id]) => id);
          const j = await post(`instances/${x.id}/download`, { profile_id: set.id, item_ids: ids, sync_input: syncInput.checked });
          d.setBusy(false);
          d.close();
          if (onStarted) onStarted(j);
        } catch (err) {
          d.setBusy(false);
          ok.disabled = false;
          mount(msg, errorBox(err));
        }
      },
    },
    h('h2', null, `启动下载器：实例 ${x.id} · ${x.num_gpus}× ${x.gpu_name}`),
    h('p', { class: 'hint' }, `实例上用 aria2（${cfg.connections} 线程）下载到 ${cfg.root} 下的目录；列表在“设置 → 启动下载配置”里改。`),
    field('下载配置', pick),
    rows.length ? h('div', { class: 'dl-picks' }, rows) : empty('这套配置还没有要下载的文件'),
    h('label', { class: 'field inline' }, syncInput,
      archive.exists ? `同步 ComfyUI 的 input（在 ComfyUI 里上传的图片 / 视频，${bytes(archive.size)}，${fmtTime(archive.mtime)} 的备份）` : '同步 ComfyUI 的 input（面板机上还没有备份）'),
    sumBox, msg,
    h('div', { class: 'toolbar end' },
      h('a', { class: 'button', href: '#/settings', onclick: () => d.close() }, '去设置'),
      h('button', { type: 'button', onclick: d.close }, '取消'), ok)));
    update();
  }
}
