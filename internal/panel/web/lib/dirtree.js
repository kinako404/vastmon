import { h, mount } from './dom.js';

// createDirTree is the folder tree under the file list: folders only, one
// level fetched at a time. list(path) resolves to that folder's subfolders
// as [{name, link}]; onOpen(path) runs when a folder is clicked and
// onMenu(ev, path) when it is right-clicked.

export const join = (dir, name) => (dir === '/' ? '' : dir) + '/' + name;
export const parent = (dir) => (dir === '/' ? '/' : dir.replace(/\/[^/]*$/, '') || '/');

// ancestors('/a/b') is ['/', '/a', '/a/b'].
function ancestors(path) {
  const out = ['/'];
  let cur = '';
  for (const seg of path.split('/').filter(Boolean)) {
    cur += '/' + seg;
    out.push(cur);
  }
  return out;
}

export function createDirTree({ list, onOpen, onMenu }) {
  // nodes keeps every folder seen so far, so a folder stays expanded after
  // its parent is collapsed or reloaded.
  const nodes = new Map();
  let current = '';
  let revealSeq = 0;
  const el = h('div', { class: 'fb-tree', role: 'tree' });

  function node(path) {
    let n = nodes.get(path);
    if (!n) {
      n = { path, name: path === '/' ? '/' : path.slice(path.lastIndexOf('/') + 1), link: false, kids: null, open: false, pending: null, err: '' };
      nodes.set(path, n);
    }
    return n;
  }

  function setKids(n, dirs) {
    n.kids = dirs.map((d) => {
      const k = node(join(n.path, d.name));
      k.link = Boolean(d.link);
      return k;
    });
    n.err = '';
  }

  // fetchKids reloads n's subfolders; calls made while one is running share it.
  function fetchKids(n) {
    if (!n.pending) {
      n.pending = list(n.path)
        .then((dirs) => setKids(n, dirs), (err) => {
          n.kids = null;
          n.err = (err && err.message) || String(err);
        })
        .finally(() => { n.pending = null; });
    }
    return n.pending;
  }

  function note(depth, cls, text) {
    const d = h('div', { class: 'fb-tree-note ' + cls }, text);
    d.style.paddingLeft = `${24 + depth * 14}px`;
    return d;
  }

  function row(n, depth) {
    const leaf = Boolean(n.kids) && !n.kids.length;
    const r = h('div', {
      class: 'fb-tree-row' + (n.path === current ? ' current' : '') + (n.name.startsWith('.') ? ' fb-hidden' : ''),
      role: 'treeitem',
      'aria-expanded': leaf ? null : String(n.open),
      title: n.path,
      oncontextmenu: onMenu ? (ev) => onMenu(ev, n.path) : null,
    },
    h('button', {
      type: 'button', class: 'fb-tree-twist', disabled: leaf, 'aria-label': n.open ? '收起' : '展开',
      onclick: () => toggle(n),
    }, leaf ? '' : n.open ? '▾' : '▸'),
    h('button', { type: 'button', class: 'fb-tree-name', onclick: () => onOpen(n.path) },
      `${n.open && !leaf ? '📂' : '📁'} ${n.name}${n.link ? ' ↪' : ''}`));
    r.style.paddingLeft = `${4 + depth * 14}px`;
    return r;
  }

  function rows(n, depth, out) {
    out.push(row(n, depth));
    if (!n.open) return;
    if (n.kids) for (const k of n.kids) rows(k, depth + 1, out);
    else if (n.err) out.push(note(depth + 1, 'bad', n.err));
    else if (n.pending) out.push(note(depth + 1, '', '读取中…'));
  }

  function render() {
    const top = el.scrollTop;
    const left = el.scrollLeft;
    const out = [];
    if (current) rows(node('/'), 0, out);
    mount(el, out);
    el.scrollTop = top;
    el.scrollLeft = left;
  }

  // showCurrent scrolls the tree (and only the tree) until the current
  // folder's row is visible.
  function showCurrent() {
    const r = el.querySelector('.fb-tree-row.current');
    if (!r || !el.clientHeight) return;
    const a = r.getBoundingClientRect();
    const b = el.getBoundingClientRect();
    if (a.top < b.top) el.scrollTop -= b.top - a.top;
    else if (a.bottom > b.bottom) el.scrollTop += a.bottom - b.bottom;
    el.scrollLeft = 0;
  }

  // toggle expands or collapses n. Expanding shows what is cached at once
  // and reloads it, so folders made in the terminal turn up.
  function toggle(n) {
    n.open = !n.open;
    if (n.open) fetchKids(n).then(render);
    render();
  }

  // reveal marks path as the current folder and expands down to it. dirs,
  // when given, are path's subfolders (the file list has just loaded them).
  async function reveal(path, dirs) {
    const seq = ++revealSeq;
    current = path;
    const chain = ancestors(path).map(node);
    if (dirs) setKids(chain[chain.length - 1], dirs);
    const waits = [];
    chain.forEach((n, i) => {
      n.open = true;
      const next = chain[i + 1];
      if (n.pending) waits.push(n.pending);
      else if (!n.kids || (next && !n.kids.includes(next))) waits.push(fetchKids(n));
    });
    render();
    showCurrent();
    if (!waits.length) return;
    await Promise.all(waits);
    render();
    if (seq === revealSeq) showCurrent();
  }

  function refresh(path) {
    fetchKids(node(path)).then(render);
    render();
  }

  return { el, reveal, refresh, showCurrent };
}
