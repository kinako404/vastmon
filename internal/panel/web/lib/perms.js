import { h, mount, errorBox } from './dom.js';
import { openDialog } from './dialog.js';

// Permission helpers for the file browser: rwx text, and the dialog that
// edits the nine permission bits of a file or folder.

export const octal = (perm) => (perm & 0o777).toString(8).padStart(3, '0');

export function rwx(perm) {
  let s = '';
  for (let bit = 8; bit >= 0; bit--) s += perm & (1 << bit) ? 'rwx'[(8 - bit) % 3] : '-';
  return s;
}

const WHO = ['所有者', '同组', '其他人'];
const WHAT = ['读', '写', '执行'];
const PRESETS = [
  [0o644, '普通文件'],
  [0o755, '可执行 / 目录'],
  [0o600, '只有自己能读写'],
  [0o700, '只有自己能用'],
  [0o666, '所有人可读写'],
  [0o777, '所有人全部权限'],
];

// openPermDialog edits entry e's permissions; apply(perm) does the change
// and may throw.
export function openPermDialog(e, path, apply) {
  const d = openDialog();
  let perm = e.perm & 0o777;
  const num = h('input', { class: 'mono perm-num', inputmode: 'numeric', maxlength: '3', spellcheck: 'false', autocomplete: 'off' });
  const text = h('span', { class: 'mono' });
  const boxes = [];
  const msg = h('div');
  const ok = h('button', { type: 'submit', class: 'primary' }, '应用');
  const cancel = h('button', { type: 'button', onclick: d.close }, '取消');

  function show(from) {
    boxes.forEach((b, i) => { b.checked = Boolean(perm & (1 << (8 - i))); });
    if (from !== num) num.value = octal(perm);
    text.textContent = rwx(perm);
    ok.disabled = perm === (e.perm & 0o777);
  }
  for (let i = 0; i < 9; i++) {
    const b = h('input', { type: 'checkbox' });
    b.addEventListener('change', () => {
      perm = b.checked ? perm | (1 << (8 - i)) : perm & ~(1 << (8 - i));
      show();
    });
    boxes.push(b);
  }
  num.addEventListener('input', () => {
    if (!/^[0-7]{3}$/.test(num.value)) {
      ok.disabled = true;
      return;
    }
    perm = parseInt(num.value, 8);
    show(num);
  });

  mount(d.body, h('form', {
    onsubmit: async (ev) => {
      ev.preventDefault();
      d.setBusy(true);
      ok.disabled = true;
      cancel.disabled = true;
      try {
        await apply(perm);
        d.setBusy(false);
        d.close();
      } catch (err) {
        d.setBusy(false);
        cancel.disabled = false;
        ok.disabled = false;
        mount(msg, errorBox(err));
      }
    },
  },
  h('h2', null, '权限'),
  h('p', { class: 'mono perm-path' }, path),
  h('p', { class: 'hint' }, `现在是 ${octal(e.perm)}（${rwx(e.perm)}）`,
    e.link ? '。这是一个链接，改的是它指向的文件。' : '', e.dir ? '。只改这个文件夹本身，不改里面的文件。' : ''),
  h('table', { class: 'perm-grid' },
    h('thead', null, h('tr', null, h('th'), WHAT.map((w) => h('th', null, w)))),
    h('tbody', null, WHO.map((who, r) => h('tr', null, h('th', null, who),
      [0, 1, 2].map((c) => h('td', null, boxes[r * 3 + c])))))),
  h('div', { class: 'perm-line' }, h('label', null, '数字 ', num), text),
  h('div', { class: 'perm-presets' }, PRESETS.map(([p, tip]) => h('button', {
    type: 'button', class: 'small mono', title: `${tip}（${rwx(p)}）`,
    onclick: () => { perm = p; show(); },
  }, octal(p)))),
  msg,
  h('div', { class: 'toolbar end' }, cancel, ok)));
  show();
  num.focus();
  num.select();
  return d;
}
