import { h } from '../lib/dom.js';
import { currentSession, loginHash } from '../lib/api.js';
import { createTextEditor } from '../lib/texteditor.js';

// Text editor for one file on an instance in its own browser tab (the file
// browser's “在新标签页编辑”). Ctrl+S saves; saving refuses if the file
// changed on the instance since it was loaded, unless confirmed.

export async function editorView(ctx) {
  const id = Number(ctx.params.id);
  const path = ctx.query.get('path') || '';
  const name = path.split('/').pop() || path;
  document.title = `${name} · 编辑`;
  if (!currentSession().logged_in) {
    ctx.show(h('h1', null, name), h('p', null, '需要先', h('a', { href: loginHash(location.hash) }, '登录'), '。'));
    return;
  }
  const ed = createTextEditor(id, path, {
    signal: ctx.signal,
    onState: () => { document.title = `${ed.dirty() ? '● ' : ''}${name} · 编辑`; },
  });
  const onLeave = (e) => {
    if (ed.dirty()) {
      e.preventDefault();
      e.returnValue = '';
    }
  };
  document.addEventListener('keydown', ed.onKey);
  window.addEventListener('beforeunload', onLeave);
  ctx.signal.addEventListener('abort', () => {
    document.removeEventListener('keydown', ed.onKey);
    window.removeEventListener('beforeunload', onLeave);
    document.body.classList.remove('full-page');
  });
  document.body.classList.add('full-page');

  ctx.show(h('div', { class: 'editor-page' },
    h('div', { class: 'title-row' },
      h('h1', null, name),
      h('span', { class: 'mono hint', title: path }, `实例 ${id} · ${path}`),
      h('span', { class: 'spacer' }),
      ed.status, ed.wrapLabel, ed.reloadBtn, ed.saveBtn),
    ed.msg, ed.area));
  await ed.load();
  ed.area.focus();
}
