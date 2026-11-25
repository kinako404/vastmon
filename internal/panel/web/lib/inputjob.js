import { h } from './dom.js';
import { fmtBytes } from './filebrowser.js';
import { fmtTime } from './format.js';
import { bar } from './downloads.js';

// Pulling an instance's ComfyUI input back to the panel runs as a job on
// the server, optionally followed by an action (stop / recycle / destroy)
// once the backup is in. inputJobPanel shows one job on its instance card:
// progress, speed, and what the user can do about a slow one — keep
// waiting, give the backup up and go ahead, or call the whole thing off.

export const inputActive = (j) => j && (j.status === 'backup' || j.status === 'acting');

// latestInputJobs maps instance id → its newest job.
export function latestInputJobs(jobs) {
  const m = new Map();
  for (const j of jobs) {
    const old = m.get(j.instance_id);
    if (!old || j.id > old.id) m.set(j.instance_id, j);
  }
  return m;
}

function fmtSecs(sec) {
  if (!Number.isFinite(sec) || sec < 0) return '';
  if (sec < 60) return `${Math.ceil(sec)} 秒`;
  if (sec < 3600) return `${Math.floor(sec / 60)} 分 ${Math.round(sec % 60)} 秒`;
  return `${Math.floor(sec / 3600)} 小时 ${Math.round((sec % 3600) / 60)} 分`;
}

// createRateMeter turns successive byte counts into a speed over the last
// 20 seconds or so; a single poll says nothing about a slow link.
export function createRateMeter() {
  const seen = new Map();
  return (j) => {
    if (!inputActive(j)) {
      seen.delete(j.id);
      return 0;
    }
    const list = seen.get(j.id) || [];
    const t = Date.now() / 1000;
    if (!list.length || list[list.length - 1].bytes !== j.bytes || t - list[list.length - 1].t >= 2) list.push({ t, bytes: j.bytes });
    while (list.length > 2 && t - list[0].t > 20) list.shift();
    seen.set(j.id, list);
    const a = list[0];
    const b = list[list.length - 1];
    return b.t - a.t >= 3 ? Math.max(0, (b.bytes - a.bytes) / (b.t - a.t)) : 0;
  };
}

export function inputJobTitle(j) {
  return j.action ? `${j.action_name}前备份 ComfyUI input` : '备份 ComfyUI input';
}

// inputJobPanel: now is the server's clock (seconds), rate bytes per second,
// askMin the 设置 value, onDecide(job, 'wait' | 'skip' | 'cancel').
export function inputJobPanel(j, { now, rate, askMin, onDecide }) {
  const title = h('span', { class: 'strong' }, inputJobTitle(j));
  if (j.status === 'acting') {
    return h('div', { class: 'dl-panel in-job' }, h('div', { class: 'in-line' }, title,
      h('span', { class: 'badge warn' }, `正在${j.action_name}`),
      h('span', { class: 'hint' }, j.skipped ? '没有备份 input' : 'input 已备份')));
  }
  if (j.status !== 'backup') {
    const [text, cls] = { done: ['完成', 'on'], failed: ['失败', 'bad'], canceled: ['已取消', ''] }[j.status] || [j.status, ''];
    return h('div', { class: 'dl-panel in-job' },
      h('div', { class: 'in-line' }, title, h('span', { class: 'badge ' + cls }, text),
        h('span', { class: 'hint' }, `${fmtTime(j.finished)} 结束`)),
      j.err ? h('div', { class: 'error' }, j.err) : null,
      j.text ? h('div', { class: j.status === 'done' ? '' : 'hint' }, j.text) : null);
  }

  const elapsed = Math.max(0, now - j.started);
  const asking = now >= j.ask_at;
  const left = j.total > j.bytes && rate > 0 ? (j.total - j.bytes) / rate : NaN;
  const then = j.action ? `备份完成后会自动${j.action_name}` : '';
  const btn = (text, decision, cls, tip) => h('button', { type: 'button', class: 'small' + (cls ? ' ' + cls : ''), title: tip, onclick: () => onDecide(j, decision) }, text);
  return h('div', { class: 'dl-panel in-job' + (asking ? ' ask' : '') },
    h('div', { class: 'in-line' }, title,
      h('span', { class: 'badge warn' }, '拉取中'),
      h('span', { class: 'spacer' }),
      h('span', { class: 'dl-total' },
        `${fmtBytes(j.bytes)}${j.total ? ` / 约 ${fmtBytes(j.total)}` : ''}`,
        rate > 0 ? ` · ${fmtBytes(rate)}/s` : '',
        ` · 已用 ${fmtSecs(elapsed)}`,
        Number.isFinite(left) ? ` · 大约还要 ${fmtSecs(left)}` : ''),
      j.total ? bar(j.bytes / j.total) : null),
    asking ? h('div', { class: 'in-ask' },
      `已经超过 ${askMin} 分钟还没拉完（这台实例到面板机的网络太慢）。传输没有停，可以继续等，也可以放弃。`) : null,
    h('div', { class: 'toolbar tight in-tools' },
      asking ? btn('继续等', 'wait', 'primary', `再等 ${askMin} 分钟，到时候还没拉完再问`) : null,
      j.action
        ? btn(`不备份了，直接${j.action_name}`, 'skip', 'danger', `停止拉取，不备份 input，马上${j.action_name}。实例上新增的图片 / 视频不会进备份`)
        : null,
      btn(j.action ? `取消${j.action_name}` : '放弃备份', 'cancel', '', j.action ? `停止拉取，也不${j.action_name}了，实例保持原样` : '停止拉取，本地备份保持原样'),
      h('span', { class: 'hint' }, [then, '关掉页面也会继续', asking ? '' : `超过 ${askMin} 分钟会问你要不要继续等`].filter(Boolean).join('；'))));
}
