import { h, mount, errorBox, loading, empty } from './dom.js';
import { get, post } from './api.js';
import { openDialog } from './dialog.js';
import { fmtTime } from './format.js';

// Startup scripts: bash scripts from settings → 开机脚本, run one after
// another on an instance over SSH, with files pushed from the panel.

const STEP_STATUS = {
  queued: ['等待', ''],
  running: ['运行中', 'warn'],
  done: ['完成', 'on'],
  failed: ['失败', 'bad'],
  canceled: ['已取消', ''],
};

const JOB_STATUS = {
  waiting: ['等实例跑起来', 'warn'],
  running: ['进行中', 'warn'],
  done: ['全部成功', 'on'],
  partial: ['有失败', 'bad'],
  failed: ['失败', 'bad'],
  canceled: ['已取消', ''],
};

export const isActive = (j) => j && (j.status === 'waiting' || j.status === 'running');

function took(st, now) {
  if (!st.started) return '';
  const sec = (st.finished || now) - st.started;
  return sec < 60 ? `${sec} 秒` : `${Math.floor(sec / 60)} 分 ${sec % 60} 秒`;
}

// stepRow shows one script; its output is folded unless it is running or
// failed. open (a Set of "job:step" keys) keeps the user's choice.
function stepRow(j, st, i, open, now) {
  const [text, cls] = STEP_STATUS[st.status] || [st.status, ''];
  const key = `${j.id}:${i}`;
  const auto = st.status === 'running' || st.status === 'failed';
  const isOpen = open.has(key) ? !auto : auto;
  const logBox = h('pre', { class: 'code sc-log' }, st.log || (st.status === 'queued' ? '（还没开始）' : '（没有输出）'));
  const det = h('details', { class: 'sc-step', open: isOpen },
    h('summary', null,
      h('span', { class: 'strong' }, st.name),
      h('span', { class: 'badge ' + cls }, text),
      st.started ? h('span', { class: 'hint' }, took(st, now)) : null,
      st.err ? h('span', { class: 'cost-bad' }, st.err) : null),
    logBox);
  det.addEventListener('toggle', () => {
    if (det.open === auto) open.delete(key);
    else open.add(key);
  });
  if (isOpen) requestAnimationFrame(() => { logBox.scrollTop = logBox.scrollHeight; });
  return det;
}

export function scriptJobPanel(j, { onCancel, folds, open, now }) {
  const active = isActive(j);
  const [text, cls] = JOB_STATUS[j.status] || [j.status, ''];
  const doneN = j.steps.filter((s) => s.status === 'done').length;
  const isOpen = folds.has(j.id) ? !active : active;
  const details = h('details', { class: 'dl-panel', open: isOpen },
    h('summary', null,
      h('span', { class: 'strong' }, '开机脚本'),
      h('span', { class: 'badge ' + cls }, text),
      j.auto ? h('span', { class: 'hint' }, '开机自动') : null,
      active && j.phase ? h('span', { class: 'hint' }, j.phase) : null,
      !active && j.finished ? h('span', { class: 'hint' }, `${fmtTime(j.finished)} 结束`) : null,
      h('span', { class: 'spacer' }),
      h('span', { class: 'dl-total' }, `${doneN} / ${j.steps.length} 个完成`),
      active && onCancel ? h('button', {
        type: 'button', class: 'small danger',
        onclick: (e) => { e.preventDefault(); onCancel(j); },
      }, '取消') : null),
    j.err ? h('div', { class: 'error' }, j.err) : null,
    j.steps.map((st, i) => stepRow(j, st, i, open, now)));
  details.addEventListener('toggle', () => {
    if (details.open === active) folds.delete(j.id);
    else folds.add(j.id);
  });
  return details;
}

export function latestScriptJobs(jobs) {
  const m = new Map();
  for (const j of jobs) if (!m.has(j.instance_id)) m.set(j.instance_id, j);
  return m;
}

// openScriptsDialog picks scripts and runs them on instance x.
export function openScriptsDialog(x, onStarted) {
  const d = openDialog({ wide: true });
  mount(d.body, h('h2', null, `运行脚本：实例 ${x.id}`), loading());
  get('scripts').then(render, (err) => mount(d.body, errorBox(err),
    h('div', { class: 'toolbar end' }, h('button', { type: 'button', onclick: d.close }, '关闭'))));

  function render(cfg) {
    const checks = new Map();
    const msg = h('div');
    const ok = h('button', { type: 'submit', class: 'primary' }, '运行');
    const update = () => { ok.disabled = ![...checks.values()].some((c) => c.checked); };
    const rows = cfg.scripts.map((sc) => {
      const c = h('input', { type: 'checkbox', checked: sc.auto, onchange: update });
      checks.set(sc.id, c);
      return h('label', { class: 'dl-pick sc-pick' }, c,
        h('span', { class: 'strong' }, sc.name),
        h('span', { class: 'hint' }, sc.files.length ? `附带 ${sc.files.join('、')}` : ''),
        h('span', { class: 'hint' }, sc.auto ? '开机自动' : ''),
        sc.note ? h('span', { class: 'hint' }, sc.note) : null);
    });
    mount(d.body, h('form', {
      onsubmit: async (e) => {
        e.preventDefault();
        ok.disabled = true;
        d.setBusy(true);
        try {
          const ids = [...checks].filter(([, c]) => c.checked).map(([id]) => id);
          const j = await post(`instances/${x.id}/scripts`, { script_ids: ids });
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
    h('h2', null, `运行脚本：实例 ${x.id} · ${x.num_gpus}× ${x.gpu_name}`),
    h('p', { class: 'hint' }, `按顺序在实例上运行（附带的文件先传到 ${cfg.files_dir}/）。脚本在“设置 → 开机脚本”里写。`),
    rows.length ? h('div', { class: 'dl-picks' }, rows) : empty('还没有脚本'),
    msg,
    h('div', { class: 'toolbar end' },
      h('a', { class: 'button', href: '#/settings', onclick: () => d.close() }, '去设置'),
      h('button', { type: 'button', onclick: d.close }, '取消'), ok)));
    update();
  }
}
