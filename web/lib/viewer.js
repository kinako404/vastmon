import { h, mount } from './dom.js';
import { openDialog } from './dialog.js';

// Picture and video viewer for files on an instance. ←/→ step through the
// pictures and videos of the folder, Esc closes.

const IMAGE = ['png', 'jpg', 'jpeg', 'gif', 'webp', 'bmp', 'avif'];
const VIDEO = ['mp4', 'webm', 'mov', 'm4v', 'mkv', 'ogv'];

export function mediaKind(name) {
  const ext = (name.match(/\.([^.]+)$/) || [])[1];
  if (!ext) return '';
  const e = ext.toLowerCase();
  if (IMAGE.includes(e)) return 'image';
  if (VIDEO.includes(e)) return 'video';
  return '';
}

// openViewer shows items[index]; rawURL(path, download) builds file URLs.
export function openViewer(instanceId, dir, items, index, rawURL) {
  if (!items.length) return;
  const d = openDialog();
  d.dialog.classList.add('viewer');
  const stage = h('div', { class: 'viewer-stage' });
  const title = h('div', { class: 'viewer-title' });
  const dl = h('a', { class: 'button small' }, '下载');
  let i = index;
  const full = (name) => (dir === '/' ? '' : dir) + '/' + name;

  function show() {
    const e = items[i];
    const src = rawURL(full(e.name), false);
    mount(title, h('span', { class: 'mono' }, e.name), h('span', { class: 'hint' }, `  ${i + 1} / ${items.length}`));
    dl.href = rawURL(full(e.name), true);
    dl.setAttribute('download', e.name);
    mount(stage, mediaKind(e.name) === 'video'
      ? h('video', { src, controls: true, autoplay: true, playsinline: true, preload: 'metadata' })
      : h('img', { src, alt: e.name }));
  }
  const step = (n) => {
    i = (i + n + items.length) % items.length;
    show();
  };
  const onKey = (e) => {
    if (e.key === 'ArrowLeft') step(-1);
    else if (e.key === 'ArrowRight') step(1);
  };
  document.addEventListener('keydown', onKey);
  d.dialog.addEventListener('close', () => document.removeEventListener('keydown', onKey));
  mount(d.body,
    h('div', { class: 'viewer-head' }, title, h('span', { class: 'spacer' }),
      items.length > 1 ? h('button', { type: 'button', class: 'small', onclick: () => step(-1), title: '上一个（←）' }, '‹') : null,
      items.length > 1 ? h('button', { type: 'button', class: 'small', onclick: () => step(1), title: '下一个（→）' }, '›') : null,
      dl,
      h('button', { type: 'button', class: 'small', onclick: d.close, title: '关闭（Esc）' }, '×')),
    stage);
  show();
}
