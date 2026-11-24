import { h, mount, table, empty, errorBox, loading, field } from '../lib/dom.js';
import { get, put, del, currentSession, isAbort } from '../lib/api.js';
import { fmtTime, fmtAgo, shortCPU } from '../lib/format.js';
import { fmtGeo } from '../lib/geo.js';
import { openDialog } from '../lib/dialog.js';

export async function blacklistView(ctx) {
  const { signal } = ctx;
  const message = h('div');
  const box = h('div', null, loading());
  let data = null;

  function noteDialog(b) {
    const d = openDialog();
    const input = h('input', { value: b.note || '', maxlength: '200' });
    const msg = h('div');
    const ok = h('button', { type: 'submit', class: 'primary' }, '保存');
    mount(d.body, h('form', {
      onsubmit: async (e) => {
        e.preventDefault();
        d.setBusy(true);
        ok.disabled = true;
        try {
          data = await put(`blacklist/${b.machine_id}/note`, { note: input.value.trim() });
          d.setBusy(false);
          d.close();
          render();
        } catch (err) {
          d.setBusy(false);
          ok.disabled = false;
          mount(msg, errorBox(err));
        }
      },
    }, h('h2', null, `机器 ${b.machine_id} 的拉黑原因`), field('拉黑原因', input, '比如“下载慢、经常掉线”'), msg,
    h('div', { class: 'toolbar end' }, h('button', { type: 'button', onclick: d.close }, '取消'), ok)));
    input.focus();
  }

  async function remove(b, btn) {
    if (!window.confirm(`把机器 ${b.machine_id} 移出黑名单？${b.note ? `\n拉黑原因“${b.note}”也会删掉。` : ''}`)) return;
    btn.disabled = true;
    try {
      data = await del(`blacklist/${b.machine_id}`);
      render();
    } catch (err) {
      btn.disabled = false;
      mount(message, errorBox(err));
    }
  }

  function render() {
    const list = data.blacklist;
    const logged = currentSession().logged_in;
    mount(box,
      h('p', { class: 'hint' }, list.length
        ? `${list.length} 台机器在黑名单里。机器列表里它们标红，开机弹窗会提醒；收藏过的机器拉黑后会移出收藏。`
        : ''),
      h('section', { class: 'card' }, list.length ? table([
        {
          title: '机器',
          class: 'nowrap',
          cell: (b) => [h('a', { class: 'mid', href: `#/machines/${b.machine_id}/${b.num_gpus}` }, String(b.machine_id)),
            b.source ? h('div', { class: 'hint' }, b.source) : null],
        },
        { title: 'GPU', class: 'nowrap', cell: (b) => `${b.num_gpus}× ${b.gpu_name || '—'}` },
        { title: 'CPU', cell: (b) => h('span', { title: b.cpu_name || '' }, shortCPU(b.cpu_name)) },
        { title: '位置', class: 'nowrap', cell: (b) => h('span', { title: b.geolocation || '' }, fmtGeo(b.geolocation)) },
        { title: '拉黑原因', cell: (b) => (b.note ? h('span', { class: 'cost-bad' }, b.note) : h('span', { class: 'muted' }, '没写')) },
        { title: '拉黑于', class: 'nowrap', cell: (b) => [fmtTime(b.created).slice(0, 16), h('div', { class: 'hint' }, fmtAgo(b.created))] },
        {
          title: '',
          class: 'nowrap',
          cell: (b) => (logged ? [
            h('button', { type: 'button', class: 'small', onclick: () => noteDialog(b) }, '改原因'), ' ',
            h('button', { type: 'button', class: 'small', onclick: (e) => remove(b, e.currentTarget) }, '移出黑名单'),
          ] : null),
        },
      ], list, () => 'row-black') : empty('黑名单是空的。在“实例”页的实例卡片、开机记录或机器详情页点“⛔ 拉黑本机”。')));
  }

  ctx.show(h('h1', null, '黑名单'), message, box);
  try {
    data = await get('blacklist', signal);
    render();
  } catch (err) {
    if (!isAbort(err)) mount(message, errorBox(err));
  }
}
