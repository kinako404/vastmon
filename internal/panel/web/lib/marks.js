import { h, mount, field, errorBox } from './dom.js';
import { post, del } from './api.js';
import { openDialog } from './dialog.js';

// Machine marks: favorites (green) and the blacklist (red). A machine is in
// at most one; the server moves it out of the other when it is added. Every
// change returns both lists, {favorites, blacklist}.

const SEP = '、';

export const KINDS = {
  fav: {
    endpoint: 'favorites',
    verb: '收藏',
    reasonLabel: '收藏原因',
    chips: ['下载快', '上传快', '价格便宜', '流量费低', 'CPU 强', '内存大', '磁盘快', '稳定不掉线', '开机快'],
    on: '★ 已收藏',
    off: '☆ 收藏本机',
    offTitle: '收藏这台机器，释放实例后还能在“收藏”里看它能不能再开',
  },
  black: {
    endpoint: 'blacklist',
    verb: '拉黑',
    reasonLabel: '拉黑原因',
    chips: ['下载慢', '上传慢', '掉线 / 不稳定', '开机失败', '卡在启动中', '流量费坑', '磁盘慢', 'GPU 有问题', '网络不通', '价格涨了'],
    on: '⛔ 已拉黑',
    off: '⛔ 拉黑本机',
    offTitle: '拉黑这台机器：机器列表里标红，开机时提醒',
  },
};

const OTHER = { fav: 'black', black: 'fav' };

// marksFrom turns a server reply into {fav, black} maps by machine id.
export function marksFrom(res) {
  return {
    fav: new Map(((res && res.favorites) || []).map((f) => [f.machine_id, f])),
    black: new Map(((res && res.blacklist) || []).map((f) => [f.machine_id, f])),
  };
}

export function emptyMarks() {
  return { fav: new Map(), black: new Map() };
}

// openMarkDialog asks why and adds machine m to kind's list; note pre-fills
// the reason.
export function openMarkDialog(kind, m, marks, source, onChange, note = '') {
  const k = KINDS[kind];
  const other = marks[OTHER[kind]].get(m.machine_id);
  const d = openDialog();
  const input = h('input', { maxlength: '200', placeholder: '可以不填', value: note });
  const parts = () => input.value.split(SEP).map((s) => s.trim()).filter(Boolean);
  const chips = k.chips.map((r) => {
    const chip = h('button', { type: 'button', class: 'chip' }, r);
    chip.addEventListener('click', () => {
      const cur = parts();
      input.value = (cur.includes(r) ? cur.filter((x) => x !== r) : [...cur, r]).join(SEP);
      sync();
      input.focus();
    });
    return chip;
  });
  const sync = () => {
    const cur = parts();
    chips.forEach((c) => c.classList.toggle('on', cur.includes(c.textContent)));
  };
  input.addEventListener('input', sync);
  sync();
  const msg = h('div');
  const ok = h('button', { type: 'submit', class: kind === 'black' ? 'danger-solid' : 'primary' }, k.verb);
  mount(d.body, h('form', {
    class: 'mark-' + kind,
    onsubmit: async (e) => {
      e.preventDefault();
      d.setBusy(true);
      ok.disabled = true;
      try {
        const res = await post(k.endpoint, {
          machine_id: m.machine_id,
          num_gpus: m.num_gpus,
          gpu_name: m.gpu_name || '',
          geolocation: m.geolocation || '',
          cpu_name: m.cpu_name || '',
          note: input.value.trim(),
          source: source || '',
        });
        d.setBusy(false);
        d.close();
        if (onChange) onChange(res);
      } catch (err) {
        d.setBusy(false);
        ok.disabled = false;
        mount(msg, errorBox(err));
      }
    },
  },
  h('h2', null, `${k.verb}机器 ${m.machine_id}`),
  h('p', { class: 'hint' }, `${m.num_gpus || 1}× ${m.gpu_name || ''}${source ? ' · ' + source : ''}`),
  other ? h('p', { class: 'warn-text' }, `这台机器现在在${kind === 'fav' ? '黑名单' : '收藏'}里，${k.verb}后会从${kind === 'fav' ? '黑名单' : '收藏'}移出。`) : null,
  field(k.reasonLabel, input, '点下面的标签快速填，也可以自己写'),
  h('div', { class: 'chips' }, chips),
  msg,
  h('div', { class: 'toolbar end' }, h('button', { type: 'button', onclick: d.close }, '取消'), ok)));
  input.focus();
}

// markButton adds machine m to kind's list (asking why) or, when it is
// there, takes it out. m needs machine_id and, for adding, num_gpus,
// gpu_name, geolocation and cpu_name. onChange gets the server's reply.
export function markButton(kind, m, marks, source, onChange) {
  const k = KINDS[kind];
  const cur = marks[kind].get(m.machine_id);
  const btn = h('button', {
    type: 'button',
    class: `small mark mark-${kind}${cur ? ' on' : ''}`,
    title: cur ? `${k.reasonLabel}：${cur.note || '（没写）'}\n点击取消${k.verb}` : k.offTitle,
  }, cur ? k.on : k.off);
  btn.addEventListener('click', async () => {
    if (!cur) {
      openMarkDialog(kind, m, marks, source, onChange);
      return;
    }
    const what = kind === 'fav' ? '取消收藏' : '移出黑名单';
    if (!window.confirm(`${what}：机器 ${m.machine_id}？${cur.note ? `\n${k.reasonLabel}“${cur.note}”也会删掉。` : ''}`)) return;
    btn.disabled = true;
    try {
      const res = await del(`${k.endpoint}/${m.machine_id}`);
      if (onChange) onChange(res);
    } catch (err) {
      btn.title = err.message;
      btn.textContent = '失败，重试';
    } finally {
      btn.disabled = false;
    }
  });
  return btn;
}

// markButtons is the favorite and blacklist buttons side by side.
export function markButtons(m, marks, source, onChange) {
  return [markButton('fav', m, marks, source, onChange), markButton('black', m, marks, source, onChange)];
}

// markBadge shows whether machine id is favorited or blacklisted, with the
// reason on hover; null when it is neither.
export function markBadge(marks, id) {
  for (const kind of ['black', 'fav']) {
    const cur = marks[kind].get(id);
    if (cur) {
      const k = KINDS[kind];
      return h('span', { class: `badge mark-badge mark-${kind}`, title: cur.note ? `${k.reasonLabel}：${cur.note}` : `${k.on.slice(2)}（没写原因）` }, k.on);
    }
  }
  return null;
}

// markKind is 'fav', 'black' or '' for machine id.
export function markKind(marks, id) {
  if (marks.black.has(id)) return 'black';
  if (marks.fav.has(id)) return 'fav';
  return '';
}
