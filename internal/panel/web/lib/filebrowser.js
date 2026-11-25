import { h, mount, errorBox } from './dom.js';
import { get, del } from './api.js';
import { fmtClock } from './format.js';
import { confirmDialog, copyText } from './dialog.js';
import { openViewer, mediaKind } from './viewer.js';

// createFileBrowser lists files on an instance over SFTP: click a folder to
// open it, double-click a picture or video to view it, right-click for the
// rest (download, open in the text editor, delete, copy the path, cd the
// terminal there). Files dropped on it or picked with 上传 go to the current folder.
// opts.onCd(dir), when given, adds “在终端 cd 到这里”.

const join = (dir, name) => (dir === '/' ? '' : dir) + '/' + name;
const parent = (dir) => (dir === '/' ? '/' : dir.replace(/\/[^/]*$/, '') || '/');

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
  const pathInput = h('input', { class: 'mono fb-path', spellcheck: 'false', title: '输入路径后回车' });
  const fileInput = h('input', { type: 'file', multiple: true, hidden: true });
  const status = h('div', { class: 'fb-status' });
  const tbody = h('tbody');
  const listWrap = h('div', { class: 'fb-list' },
    h('table', null, h('thead', null, h('tr', null, h('th', null, '名称'), h('th', { class: 'num' }, '大小'), h('th', { class: 'num' }, '修改时间'))), tbody));
  const el = h('div', { class: 'fb' },
    h('div', { class: 'fb-toolbar' },
      h('button', { type: 'button', class: 'small', title: '上一级', onclick: () => load(parent(cwd)) }, '↑'),
      pathInput,
      h('button', { type: 'button', class: 'small', title: '刷新', onclick: () => load(cwd) }, '刷新'),
      h('button', { type: 'button', class: 'small primary', onclick: () => fileInput.click() }, '上传')),
    listWrap, status, fileInput);

  const setStatus = (...nodes) => mount(status, ...nodes);
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

  function view(e) {
    const media = entries.filter((x) => !x.dir && mediaKind(x.name));
    openViewer(instanceId, cwd, media, Math.max(0, media.indexOf(e)), rawURL);
  }

  async function copyPath(p) {
    try {
      await copyText(p);
      setStatus(`已复制 ${p}`);
    } catch (err) {
      setStatus(errorBox(err));
    }
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
      showMenu(ev, [['打开', () => load(p)], cd ? ['在终端 cd 到这里', () => cd(p)] : null, ['复制路径', () => copyPath(p)]]);
    } else {
      const k = mediaKind(e.name);
      showMenu(ev, [
        k ? [k === 'image' ? '查看图片' : '播放视频', () => view(e)] : null,
        ['用文本编辑器打开（新网页）', () => openEditor(e)],
        ['下载', () => download(e)],
        ['-'],
        ['复制路径', () => copyPath(p)],
        cd ? ['在终端 cd 到所在目录', () => cd(cwd)] : null,
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
      title: `${e.mode}${e.link ? ' · 链接' : ''}${e.broken ? '（目标不存在）' : ''}`,
      ondblclick: () => {
        if (e.dir) load(join(cwd, e.name));
        else if (mediaKind(e.name)) view(e);
      },
      oncontextmenu: (ev) => menuFor(ev, e),
    },
    h('td', null, e.dir
      ? h('button', { type: 'button', class: 'link fb-name dir', onclick: () => load(join(cwd, e.name)) }, `${icon(e)} ${e.name}${e.link ? ' ↪' : ''}`)
      : h('span', { class: 'fb-name' }, `${icon(e)} ${e.name}${e.link ? ' ↪' : ''}`)),
    h('td', { class: 'num' }, e.dir ? '' : fmtBytes(e.size)),
    h('td', { class: 'num' }, fmtClock(e.mtime)))));
    const files = entries.filter((e) => !e.dir).length;
    setStatus(`${entries.length - files} 个文件夹，${files} 个文件 · 双击图片 / 视频查看，右键看更多操作`);
  }

  function load(p) {
    inflight = doLoad(p);
    return inflight;
  }

  async function doLoad(p) {
    setStatus('读取中…');
    try {
      const res = await get(`files/${instanceId}/list${p ? '?path=' + encodeURIComponent(p) : ''}`);
      cwd = res.path;
      entries = res.entries;
      render();
      listWrap.scrollTop = 0;
    } catch (err) {
      pathInput.value = cwd || p || '';
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

  return { el, load };
}
