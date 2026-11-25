import { h, mount, errorBox, loading } from './dom.js';
import { get, put } from './api.js';
import { openDialog } from './dialog.js';

// Text editor for one file on an instance: a textarea with save / reload,
// Tab inserting a tab, and a guard against overwriting a file that changed
// on the instance after it was loaded. The page at #/edit/… and the popup
// opened from the file browser are both built from its parts.

export const TEXT_LIMIT = 5 << 20;

function fmtSize(n) {
  if (n < 1024) return `${n} B`;
  if (n < 1 << 20) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / (1 << 20)).toFixed(1)} MB`;
}

// createTextEditor: onState() runs whenever the saved / unsaved state may
// have changed.
export function createTextEditor(instanceId, path, { onState, signal } = {}) {
  const name = path.split('/').pop() || path;
  const status = h('span', { class: 'hint' });
  const msg = h('div');
  const area = h('textarea', { class: 'editor-area mono', spellcheck: 'false', wrap: 'off' });
  const wrapBox = h('input', { type: 'checkbox' });
  const wrapLabel = h('label', { class: 'field inline' }, wrapBox, '自动换行');
  const saveBtn = h('button', { type: 'button', class: 'primary', title: 'Ctrl+S' }, '保存');
  const reloadBtn = h('button', { type: 'button' }, '重新载入');
  let mtime = 0;
  let saved = '';
  let loaded = false;
  const dirty = () => loaded && area.value !== saved;
  const showState = (text) => {
    mount(status, dirty() ? h('span', { class: 'cost-warn' }, '● 未保存') : '已保存', text ? ` · ${text}` : '');
    if (onState) onState();
  };

  // load reads the file; it resolves to the error when that fails.
  async function load() {
    if (dirty() && !window.confirm('有没保存的修改，重新载入会丢掉，继续？')) return null;
    mount(msg, loading());
    try {
      const r = await get(`files/${instanceId}/text?path=${encodeURIComponent(path)}`, signal);
      area.value = r.content;
      saved = r.content;
      mtime = r.mtime;
      loaded = true;
      mount(msg);
      showState(`${fmtSize(r.size)} · ${r.mode}`);
      return null;
    } catch (err) {
      mount(msg, errorBox(err));
      return err;
    }
  }

  async function save(force = false) {
    if (!loaded) return;
    saveBtn.disabled = true;
    try {
      const text = area.value;
      const r = await put(`files/${instanceId}/text`, { path, content: text, mtime, force });
      saved = text;
      mtime = r.mtime;
      mount(msg);
      showState(`${fmtSize(r.size)} · ${new Date().toLocaleTimeString()} 保存`);
    } catch (err) {
      if (err.code === 'CHANGED' && window.confirm(`${err.message}，还是用你的版本覆盖吗？`)) {
        saveBtn.disabled = false;
        await save(true);
        return;
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
  // onKey is the Ctrl+S handler; the owner decides where to listen.
  const onKey = (e) => {
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 's') {
      e.preventDefault();
      save();
    }
  };

  return { instanceId, path, name, status, msg, area, wrapLabel, saveBtn, reloadBtn, load, save, dirty, onKey, isLoaded: () => loaded };
}

// openEditorDialog shows editor ed in a popup over the current page. Closing
// it (×, Esc) only puts it away: ed keeps its text, saved or not, and can be
// shown again. onClosed runs after it is put away.
export function openEditorDialog(ed, { onClosed } = {}) {
  const d = openDialog();
  d.dialog.classList.add('editor-dialog');
  d.dialog.addEventListener('keydown', ed.onKey);
  d.dialog.addEventListener('close', () => {
    d.dialog.removeEventListener('keydown', ed.onKey);
    if (onClosed) onClosed();
  });
  mount(d.body,
    h('div', { class: 'title-row' },
      h('h2', null, ed.name),
      h('span', { class: 'mono hint editor-path', title: ed.path }, ed.path),
      h('span', { class: 'spacer' }),
      ed.status, ed.wrapLabel, ed.reloadBtn, ed.saveBtn,
      h('button', {
        type: 'button', title: '在新标签页里打开这个文件（没保存的修改不会带过去）',
        onclick: () => window.open(`#/edit/${ed.instanceId}?path=${encodeURIComponent(ed.path)}`, '_blank'),
      }, '新标签页 ↗'),
      h('button', { type: 'button', title: '收起窗口（Esc）。内容留着，点文件管理器上方的标签再打开', onclick: d.close }, '×')),
    ed.msg, ed.area);
  ed.area.focus();
  return d;
}
