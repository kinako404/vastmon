import { h, mount, errorBox } from './dom.js';
import { get, del, post } from './api.js';
import { fmtClock } from './format.js';
import { confirmDialog, copyText } from './dialog.js';
import { openViewer, mediaKind } from './viewer.js';
import { createDirTree, join, parent } from './dirtree.js';
import { createTextEditor, openEditorDialog, TEXT_LIMIT } from './texteditor.js';
import { openPermDialog, octal, rwx } from './perms.js';

// createFileBrowser lists files on an instance over SFTP: click a folder to
// open it, double-click a picture or video to view it and any other file to
// edit it as text in a popup, right-click for the rest (download, edit in a
// new tab, permissions, delete, copy the path, cd the terminal there). Files
// dropped on it or picked with 上传 go to the current folder.
//
// Files opened for editing stay open as tabs above the list after their
// popup is closed, with their text (saved or not); a tab brings its popup
// back.
//
// There is a folder tree for jumping around, which follows the list. When
// the browser is tall (beside the terminal) the tree sits below the list,
// resized by dragging its title bar and hideable; when it is wide (under
// the terminal) the tree sits to the left of the list, as in FinalShell.
// opts.onCd(dir), when given, adds “在终端 cd 到这里”.

// TREE_KEY is the localStorage key for the tree's share of the height and
// whether it is hidden (below the list), and its width (beside the list),
// shared by every file browser.
const TREE_KEY = 'vastmon.fileTree';

function treePrefs() {
  let p = null;
  try {
    p = JSON.parse(localStorage.getItem(TREE_KEY));
  } catch {
    p = null;
  }
  const size = Number(p && p.size);
  const width = Number(p && p.width);
  return { size: size >= 0.1 && size <= 0.8 ? size : 0.4, hidden: Boolean(p && p.hidden), width: width >= 120 && width <= 1200 ? width : 220 };
}

// allDocs are the files open for editing in every file browser; leaving the
// page with unsaved text in one of them asks first.
const allDocs = new Set();
window.addEventListener('beforeunload', (e) => {
  if ([...allDocs].some((d) => d.ed.dirty())) {
    e.preventDefault();
    e.returnValue = '';
  }
});

export function fmtBytes(n) {
  if (!Number.isFinite(n)) return '—';
  const u = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0;
  let v = n;
  while (v >= 1024 && i < u.length - 1) {
    v /= 1024;
    i++;
  }
  return `${i ? v.toFixed(v < 10 ? 1 : 0) : v} ${u[i]}`;
}

function icon(e) {
  if (e.dir) return '📁';
  const k = mediaKind(e.name);
  if (k === 'image') return '🖼';
  if (k === 'video') return '🎬';
  return '📄';
}

let menuEl = null;
function closeMenu() {
  if (menuEl) menuEl.remove();
  menuEl = null;
}
document.addEventListener('click', closeMenu);
document.addEventListener('keydown', (e) => { if (e.key === 'Escape') closeMenu(); });
window.addEventListener('blur', closeMenu);

function showMenu(ev, items) {
  ev.preventDefault();
  ev.stopPropagation();
  closeMenu();
  menuEl = h('div', { class: 'ctx-menu', role: 'menu' },
    items.filter(Boolean).map(([text, fn, cls]) => (text === '-'
      ? h('div', { class: 'ctx-sep' })
      : h('button', { type: 'button', class: 'ctx-item' + (cls ? ' ' + cls : ''), onclick: () => { closeMenu(); fn(); } }, text))));
  document.body.appendChild(menuEl);
  const r = menuEl.getBoundingClientRect();
  menuEl.style.left = `${Math.min(ev.clientX, window.innerWidth - r.width - 4)}px`;
  menuEl.style.top = `${Math.min(ev.clientY, window.innerHeight - r.height - 4)}px`;
}

export function createFileBrowser(instanceId, opts = {}) {
  let cwd = '';
  let entries = [];
  // inflight is the folder change in progress; uploads wait for it so files
  // land in the folder on show, not the one being left.
  let inflight = Promise.resolve();
  const base = `api/files/${instanceId}`;
  // loadSeq numbers folder changes, so a slow answer for a folder already
  // left is dropped.
  let loadSeq = 0;
  let wide = false;
  const pathInput = h('input', { class: 'mono fb-path', spellcheck: 'false', title: '输入路径后回车；点一下全选，方便复制或整段换掉' });
  const fileInput = h('input', { type: 'file', multiple: true, hidden: true });
  const status = h('div', { class: 'fb-status' });
  const tbody = h('tbody');
  const listWrap = h('div', { class: 'fb-list' },
    h('table', null, h('thead', null, h('tr', null, h('th', null, '名称'), h('th', { class: 'num' }, '大小'), h('th', { class: 'num' }, '修改时间'))), tbody));
  const setStatus = (...nodes) => mount(status, ...nodes);

  async function copyPath(p) {
    try {
      await copyText(p);
      setStatus(`已复制 ${p}`);
    } catch (err) {
      setStatus(errorBox(err));
    }
  }

  const tree = createDirTree({
    list: (p) => get(`files/${instanceId}/list?dirs=1&path=${encodeURIComponent(p)}`).then((res) => res.entries),
    onOpen: (p) => load(p),
    onMenu: (ev, p) => showMenu(ev, [
      ['打开', () => load(p)],
      opts.onCd ? ['在终端 cd 到这里', () => opts.onCd(p)] : null,
      ['复制路径', () => copyPath(p)],
      ['-'],
      ['刷新这一层', () => tree.refresh(p)],
    ]),
  });
  const prefs = treePrefs();
  const treeToggle = h('button', { type: 'button', class: 'link small', onclick: () => { prefs.hidden = !prefs.hidden; applyTree(true); } });
  const treeBar = h('div', { class: 'fb-tree-bar' }, h('span', null, '目录树'), treeToggle);
  const vsplit = h('div', { class: 'fb-vsplit', title: '左右拖动调整目录树的宽度' });
  const docBar = h('div', { class: 'fb-docs', hidden: true });
  const main = h('div', { class: 'fb-main' }, listWrap, treeBar, tree.el, vsplit);
  const el = h('div', { class: 'fb' },
    h('div', { class: 'fb-toolbar' },
      h('button', { type: 'button', class: 'small', title: '上一级', onclick: () => load(parent(cwd)) }, '↑'),
      pathInput,
      h('button', { type: 'button', class: 'small', title: '刷新', onclick: () => load(cwd) }, '刷新'),
      h('button', { type: 'button', class: 'small primary', onclick: () => fileInput.click() }, '上传')),
    docBar, main, status, fileInput);

  // applyTree lays the tree out: beside the list at its saved width when the
  // browser is wide; otherwise below it at its saved height, or hidden with
  // just its title bar left.
  function applyTree(save) {
    const hidden = prefs.hidden && !wide;
    el.classList.toggle('wide', wide);
    tree.el.hidden = hidden;
    tree.el.style.flexBasis = wide ? `${prefs.width}px` : `${prefs.size * 100}%`;
    treeBar.classList.toggle('closed', prefs.hidden);
    treeBar.title = prefs.hidden ? '' : '上下拖动调整高度';
    treeToggle.textContent = prefs.hidden ? '展开 ▴' : '收起 ▾';
    if (!hidden) tree.showCurrent();
    if (save) localStorage.setItem(TREE_KEY, JSON.stringify(prefs));
  }
  applyTree(false);
  new ResizeObserver(() => {
    const w = el.clientWidth;
    const next = w >= 640 && w >= el.clientHeight * 1.6;
    if (next === wide) return;
    wide = next;
    applyTree(false);
  }).observe(el);

  vsplit.addEventListener('pointerdown', (e) => {
    if (e.button !== 0) return;
    e.preventDefault();
    vsplit.setPointerCapture(e.pointerId);
    const box = main.getBoundingClientRect();
    const move = (ev) => {
      prefs.width = Math.round(Math.min(box.width - 240, Math.max(120, ev.clientX - box.left)));
      tree.el.style.flexBasis = `${prefs.width}px`;
    };
    const up = () => {
      vsplit.removeEventListener('pointermove', move);
      vsplit.removeEventListener('pointerup', up);
      vsplit.removeEventListener('pointercancel', up);
      applyTree(true);
    };
    vsplit.addEventListener('pointermove', move);
    vsplit.addEventListener('pointerup', up);
    vsplit.addEventListener('pointercancel', up);
  });

  treeBar.addEventListener('pointerdown', (e) => {
    if (prefs.hidden || e.button !== 0 || e.target.closest('button')) return;
    e.preventDefault();
    treeBar.setPointerCapture(e.pointerId);
    const grab = treeBar.getBoundingClientRect().bottom - e.clientY;
    const move = (ev) => {
      const frac = (tree.el.getBoundingClientRect().bottom - ev.clientY - grab) / main.getBoundingClientRect().height;
      prefs.size = Math.min(0.8, Math.max(0.1, frac));
      tree.el.style.flexBasis = `${prefs.size * 100}%`;
    };
    const up = () => {
      treeBar.removeEventListener('pointermove', move);
      treeBar.removeEventListener('pointerup', up);
      treeBar.removeEventListener('pointercancel', up);
      applyTree(true);
    };
    treeBar.addEventListener('pointermove', move);
    treeBar.addEventListener('pointerup', up);
    treeBar.addEventListener('pointercancel', up);
  });

  const rawURL = (p, dl) => `${base}/raw?path=${encodeURIComponent(p)}${dl ? '&dl=1' : ''}`;

  function download(e) {
    const a = h('a', { href: rawURL(join(cwd, e.name), true), download: e.name });
    document.body.appendChild(a);
    a.click();
    a.remove();
  }

  function openEditor(e) {
    window.open(`#/edit/${instanceId}?path=${encodeURIComponent(join(cwd, e.name))}`, '_blank');
  }

  // docs are the files open for editing, by path. Each keeps its editor
  // (and so its text) for as long as its tab is there.
  const docs = new Map();

  function renderDocs() {
    docBar.hidden = !docs.size;
    mount(docBar, [...docs.values()].map((doc) => h('span', {
      class: 'fb-doc' + (doc.dialog ? ' on' : ''),
      title: `${doc.ed.path}${doc.dirty ? '\n有没保存的修改' : ''}`,
    },
    h('button', { type: 'button', class: 'fb-doc-name', onclick: () => showDoc(doc) },
      doc.dirty ? h('span', { class: 'cost-warn' }, '● ') : null, `📝 ${doc.ed.name}`),
    h('button', { type: 'button', class: 'fb-doc-close', title: '关闭这个文档', onclick: () => closeDoc(doc) }, '×'))));
  }

  function showDoc(doc) {
    if (doc.dialog) return;
    doc.dialog = openEditorDialog(doc.ed, {
      onClosed: () => {
        doc.dialog = null;
        renderDocs();
      },
    });
    renderDocs();
  }

  function closeDoc(doc) {
    if (doc.ed.dirty() && !window.confirm(`${doc.ed.name} 有没保存的修改，关闭会丢掉，继续？`)) return;
    docs.delete(doc.ed.path);
    allDocs.delete(doc);
    if (doc.dialog) doc.dialog.close();
    renderDocs();
  }

  // openDoc edits file e as text in a popup. Whether it is text is the
  // server's call (it refuses files with NUL bytes), so anything can be
  // tried; a file that did not load gets no tab.
  function openDoc(e) {
    const p = join(cwd, e.name);
    if (docs.has(p)) {
      showDoc(docs.get(p));
      return;
    }
    if (e.size > TEXT_LIMIT) {
      setStatus(errorBox({ message: `${e.name} 有 ${fmtBytes(e.size)}，超过 5 MB 不在网页里编辑，可以右键下载` }));
      return;
    }
    const doc = { ed: null, dialog: null, dirty: false };
    doc.ed = createTextEditor(instanceId, p, {
      onState: () => {
        const dirty = doc.ed.dirty();
        const fresh = doc.ed.isLoaded() && !allDocs.has(doc);
        if (fresh) {
          docs.set(p, doc);
          allDocs.add(doc);
        }
        if (fresh || dirty !== doc.dirty) {
          doc.dirty = dirty;
          renderDocs();
        }
      },
    });
    showDoc(doc);
    doc.ed.load();
  }

  // chmod sets entry e's permission bits and shows the result in place.
  async function chmod(e, perm) {
    const dir = cwd;
    const r = await post(`files/${instanceId}/chmod`, { path: join(dir, e.name), mode: octal(perm) });
    e.perm = r.perm;
    if (!e.link) e.mode = r.mode;
    if (dir !== cwd) return;
    render();
    setStatus(`${e.name} 的权限改成了 ${octal(r.perm)}（${rwx(r.perm)}）`);
  }

  // permItems are the menu entries for permissions: one click for the
  // executable bit, a dialog for the rest.
  function permItems(e) {
    if (e.broken) return [];
    const quick = (perm) => () => chmod(e, perm).catch((err) => setStatus(errorBox(err)));
    return [
      ['-'],
      e.dir ? null : e.perm & 0o100
        ? ['去掉执行权限（chmod -x）', quick(e.perm & ~0o111)]
        : ['加执行权限（chmod +x）', quick(e.perm | 0o111)],
      [`权限…（现在 ${octal(e.perm)}）`, () => openPermDialog(e, join(cwd, e.name), (perm) => chmod(e, perm))],
    ];
  }

  function view(e) {
    const media = entries.filter((x) => !x.dir && mediaKind(x.name));
    openViewer(instanceId, cwd, media, Math.max(0, media.indexOf(e)), rawURL);
  }

  // remove deletes one file after a confirmation; the list is updated in
  // place so a long folder keeps its scroll position.
  function remove(e) {
    const dir = cwd;
    const p = join(dir, e.name);
    confirmDialog({
      title: '删除文件',
      message: [
        h('p', null, '要删除实例上的这个文件吗？删掉之后不能恢复。'),
        h('p', { class: 'mono fb-del-path' }, p),
        h('p', { class: 'muted' }, e.link ? '这是一个链接，只删链接本身，它指向的文件不动。' : `大小 ${fmtBytes(e.size)}`),
      ],
      confirmText: '删除',
      danger: true,
      run: async () => {
        await del(`files/${instanceId}/file?path=${encodeURIComponent(p)}`);
        if (dir !== cwd) return;
        entries = entries.filter((x) => x.name !== e.name);
        render();
        setStatus(`已删除 ${p}`);
      },
    });
  }

  function menuFor(ev, e) {
    const p = e ? join(cwd, e.name) : cwd;
    const cd = opts.onCd;
    if (!e) {
      showMenu(ev, [['上传文件到这里', () => fileInput.click()], ['刷新', () => load(cwd)],
        ['复制当前路径', () => copyPath(cwd)], cd ? ['在终端 cd 到这里', () => cd(cwd)] : null]);
    } else if (e.dir) {
      showMenu(ev, [['打开', () => load(p)], cd ? ['在终端 cd 到这里', () => cd(p)] : null, ['复制路径', () => copyPath(p)], ...permItems(e)]);
    } else {
      const k = mediaKind(e.name);
      showMenu(ev, [
        k ? [k === 'image' ? '查看图片' : '播放视频', () => view(e)] : null,
        ['编辑', () => openDoc(e)],
        ['在新标签页编辑', () => openEditor(e)],
        ['下载', () => download(e)],
        ['-'],
        ['复制路径', () => copyPath(p)],
        cd ? ['在终端 cd 到所在目录', () => cd(cwd)] : null,
        ...permItems(e),
        ['-'],
        ['删除', () => remove(e), 'danger'],
      ]);
    }
  }

  function render() {
    pathInput.value = cwd;
    mount(tbody, cwd !== '/' ? h('tr', { class: 'fb-row', ondblclick: () => load(parent(cwd)) },
      h('td', { colspan: '3' }, h('button', { type: 'button', class: 'link fb-name', onclick: () => load(parent(cwd)) }, '📁 ..'))) : null,
    entries.map((e) => h('tr', {
      class: 'fb-row' + (e.name.startsWith('.') ? ' fb-hidden' : ''),
      title: `${e.mode}${e.broken ? '' : ' · ' + octal(e.perm)}${e.link ? ' · 链接' : ''}${e.broken ? '（目标不存在）' : ''}`,
      ondblclick: () => {
        if (e.dir) load(join(cwd, e.name));
        else if (mediaKind(e.name)) view(e);
        else openDoc(e);
      },
      oncontextmenu: (ev) => menuFor(ev, e),
    },
    h('td', null, e.dir
      ? h('button', { type: 'button', class: 'link fb-name dir', onclick: () => load(join(cwd, e.name)) }, `${icon(e)} ${e.name}${e.link ? ' ↪' : ''}`)
      : h('span', { class: 'fb-name' }, `${icon(e)} ${e.name}${e.link ? ' ↪' : ''}`)),
    h('td', { class: 'num' }, e.dir ? '' : fmtBytes(e.size)),
    h('td', { class: 'num' }, fmtClock(e.mtime)))));
    const files = entries.filter((e) => !e.dir).length;
    setStatus(`${entries.length - files} 个文件夹，${files} 个文件 · 双击打开（图片 / 视频查看，其它当文本编辑），右键看更多操作`);
  }

  function load(p) {
    inflight = doLoad(p);
    return inflight;
  }

  // doLoad shows folder p. Going somewhere else switches at once (the path
  // and an empty list saying 加载中) and fills the list in when the answer
  // arrives; if it fails, the folder that was on show comes back.
  async function doLoad(p) {
    const seq = ++loadSeq;
    const moving = Boolean(p) && p !== cwd;
    if (moving) {
      pathInput.value = p;
      mount(tbody, h('tr', null, h('td', { colspan: '3', class: 'fb-loading' }, '加载中…')));
      listWrap.scrollTop = 0;
    }
    setStatus('读取中…');
    try {
      const res = await get(`files/${instanceId}/list${p ? '?path=' + encodeURIComponent(p) : ''}`);
      if (seq !== loadSeq) return;
      const changed = res.path !== cwd;
      cwd = res.path;
      entries = res.entries;
      render();
      if (changed) listWrap.scrollTop = 0;
      tree.reveal(cwd, entries.filter((e) => e.dir));
    } catch (err) {
      if (seq !== loadSeq) return;
      if (cwd) render();
      else pathInput.value = p || '';
      setStatus(errorBox(err));
    }
  }

  // upload sends files to the current folder with progress; an existing name
  // asks before overwriting.
  async function upload(files, overwrite = false) {
    await inflight;
    if (!files.length || !cwd) return;
    const dir = cwd;
    const fd = new FormData();
    let total = 0;
    for (const f of files) {
      fd.append('file', f, f.name);
      total += f.size;
    }
    const bar = h('progress', { max: '100', value: '0' });
    const label = h('span');
    setStatus(bar, ' ', label);
    const names = files.length === 1 ? files[0].name : `${files.length} 个文件`;
    const xhr = new XMLHttpRequest();
    xhr.open('POST', `${base}/upload?dir=${encodeURIComponent(dir)}${overwrite ? '&overwrite=1' : ''}`);
    xhr.setRequestHeader('X-Requested-With', 'vastmon');
    xhr.upload.onprogress = (e) => {
      if (!e.lengthComputable) return;
      bar.value = (e.loaded / e.total) * 100;
      label.textContent = ` 上传 ${names}：${fmtBytes(e.loaded)} / ${fmtBytes(total)}`;
    };
    xhr.onload = () => {
      let data = null;
      try {
        data = JSON.parse(xhr.responseText);
      } catch {
        data = null;
      }
      if (xhr.status === 409 && data && data.error && data.error.code === 'EXISTS') {
        if (window.confirm(`${data.error.message}，要覆盖吗？`)) upload(files, true);
        else setStatus('已取消上传');
        return;
      }
      if (xhr.status >= 400) {
        setStatus(errorBox({ message: (data && data.error && data.error.message) || `上传失败（HTTP ${xhr.status}）` }));
        return;
      }
      const done = `已上传 ${names}（${fmtBytes(total)}）到 ${dir}`;
      if (dir === cwd) load(cwd).then(() => setStatus(done));
      else setStatus(done);
    };
    xhr.onerror = () => setStatus(errorBox({ message: '上传失败：网络断了' }));
    xhr.send(fd);
  }

  pathInput.addEventListener('keydown', (e) => {
    if (e.key === 'Enter') load(pathInput.value.trim());
  });
  // The first click into the path selects all of it; a click while it
  // already has the focus places the cursor as usual.
  let clickedIn = false;
  pathInput.addEventListener('pointerdown', () => { clickedIn = document.activeElement !== pathInput; });
  pathInput.addEventListener('focus', () => pathInput.select());
  pathInput.addEventListener('mouseup', (e) => {
    if (!clickedIn) return;
    clickedIn = false;
    e.preventDefault();
    pathInput.select();
  });
  fileInput.addEventListener('change', () => {
    upload([...fileInput.files]);
    fileInput.value = '';
  });
  listWrap.addEventListener('contextmenu', (ev) => {
    if (ev.target.closest('tr.fb-row')) return;
    menuFor(ev, null);
  });
  let dragDepth = 0;
  el.addEventListener('dragenter', (e) => {
    if (!e.dataTransfer.types.includes('Files')) return;
    e.preventDefault();
    dragDepth++;
    el.classList.add('fb-drop');
  });
  el.addEventListener('dragover', (e) => {
    if (e.dataTransfer.types.includes('Files')) e.preventDefault();
  });
  el.addEventListener('dragleave', () => {
    dragDepth = Math.max(0, dragDepth - 1);
    if (!dragDepth) el.classList.remove('fb-drop');
  });
  el.addEventListener('drop', (e) => {
    e.preventDefault();
    dragDepth = 0;
    el.classList.remove('fb-drop');
    upload([...e.dataTransfer.files]);
  });

  return {
    el,
    load,
    // unsaved counts the open documents with changes not saved yet.
    unsaved: () => [...docs.values()].filter((d) => d.ed.dirty()).length,
    // dispose forgets the open documents, when the browser goes away.
    dispose: () => {
      for (const d of docs.values()) allDocs.delete(d);
      docs.clear();
    },
  };
}
