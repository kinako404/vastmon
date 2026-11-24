import { h, mount, errorBox, loading } from '../lib/dom.js';
import { get, put, currentSession, loginHash } from '../lib/api.js';
import { fmtBytes } from '../lib/filebrowser.js';

// Text editor for one file on an instance, opened in its own browser tab
// from the file browser's right-click menu. Ctrl+S saves; saving refuses if
// the file changed on the instance since it was loaded, unless confirmed.

export async function editorView(ctx) {
  const id = Number(ctx.params.id);
  const path = ctx.query.get('path') || '';
  const name = path.split('/').pop() || path;
  document.title = `${name} · 编辑`;
  if (!currentSession().logged_in) {
    ctx.show(h('h1', null, name), h('p', null, '需要先', h('a', { href: loginHash(location.hash) }, '登录'), '。'));
    return;
  }
  const status = h('span', { class: 'hint' });
  const msg = h('div');
  const area = h('textarea', { class: 'editor-area mono', spellcheck: 'false', wrap: 'off' });
  const wrapBox = h('input', { type: 'checkbox' });
  const saveBtn = h('button', { type: 'button', class: 'primary' }, '保存');
  const reloadBtn = h('button', { type: 'button' }, '重新载入');
  let mtime = 0;
  let saved = '';
  const dirty = () => area.value !== saved;
  const showState = (text) => {
    mount(status, dirty() ? h('span', { class: 'cost-warn' }, '● 未保存') : '已保存', text ? ` · ${text}` : '');
    document.title = `${dirty() ? '● ' : ''}${name} · 编辑`;
  };

  async function load() {
    if (dirty() && !window.confirm('有没保存的修改，重新载入会丢掉，继续？')) return;
    mount(msg, loading());
    try {
      const r = await get(`files/${id}/text?path=${encodeURIComponent(path)}`, ctx.signal);
      area.value = r.content;
      saved = r.content;
      mtime = r.mtime;
      mount(msg);
      showState(`${fmtBytes(r.size)} · ${r.mode}`);
    } catch (err) {
      mount(msg, errorBox(err));
    }
  }

  async function save(force = false) {
    saveBtn.disabled = true;
    try {
      const text = area.value;
      const r = await put(`files/${id}/text`, { path, content: text, mtime, force });
      saved = text;
      mtime = r.mtime;
      mount(msg);
      showState(`${fmtBytes(r.size)} · ${new Date().toLocaleTimeString()} 保存`);
    } catch (err) {
      if (err.code === 'CHANGED' && window.confirm(`${err.message}，还是用你的版本覆盖吗？`)) {
        saveBtn.disabled = false;
        return save(true);
      }
      mount(msg, errorBox(err));
    } finally {
      saveBtn.disabled = false;
    }
  }

  saveBtn.addEventListener('click', () => save());
  reloadBtn.addEventListener('click', load);
  area.addEventListener('input', () => showState());
  wrapBox.addEventListener('change', () => area.setAttribute('wrap', wrapBox.checked ? 'soft' : 'off'));
  area.addEventListener('keydown', (e) => {
    if (e.key === 'Tab' && !e.ctrlKey && !e.altKey && !e.metaKey) {
      e.preventDefault();
      const { selectionStart: a, selectionEnd: b } = area;
      area.setRangeText('\t', a, b, 'end');
      showState();
    }
  });
  const onKey = (e) => {
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 's') {
      e.preventDefault();
      save();
    }
  };
  const onLeave = (e) => {
    if (dirty()) {
      e.preventDefault();
      e.returnValue = '';
    }
  };
  document.addEventListener('keydown', onKey);
  window.addEventListener('beforeunload', onLeave);
  ctx.signal.addEventListener('abort', () => {
    document.removeEventListener('keydown', onKey);
    window.removeEventListener('beforeunload', onLeave);
    document.body.classList.remove('full-page');
  });
  document.body.classList.add('full-page');

  ctx.show(h('div', { class: 'editor-page' },
    h('div', { class: 'title-row' },
      h('h1', null, name),
      h('span', { class: 'mono hint', title: path }, `实例 ${id} · ${path}`),
      h('span', { class: 'spacer' }),
      status,
      h('label', { class: 'field inline' }, wrapBox, '自动换行'),
      reloadBtn, saveBtn),
    msg, area));
  await load();
  area.focus();
}
