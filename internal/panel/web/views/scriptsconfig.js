import { h, mount, errorBox, loading, empty, field, table } from '../lib/dom.js';
import { get, put, post, del } from '../lib/api.js';
import { openDialog, copyText } from '../lib/dialog.js';
import { fmtTime } from '../lib/format.js';
import { fmtBytes } from '../lib/filebrowser.js';

// Settings → 开机脚本: bash scripts run on instances after boot (or from the
// instance card), the variables handed to them, and the file library whose
// files are pushed to the instance before a script runs.

const ENV_HELP = [
  ['VASTMON_FILES', '附带的文件所在目录（/opt/vastmon/files）'],
  ['VASTMON_WORKDIR', '工作目录（启动下载配置里设的，默认 /workspace/ComfyUI），脚本从这里开始运行'],
  ['VASTMON_INSTANCE_ID / VASTMON_LABEL / VASTMON_GPU', '实例 ID、名称、显卡'],
  ['VASTMON_SITE', '站点 URL（设置 → 站点）'],
];

export function scriptsSection() {
  const box = h('div', null, loading());
  const varsBox = h('div');
  const filesBox = h('div');
  const msg = h('div');
  let cfg = null;

  function show(next, note) {
    cfg = next;
    render();
    renderVars();
    renderFiles();
    mount(msg, note ? h('div', { class: 'notice' }, note) : null);
  }

  async function saveScripts(scripts, note) {
    try {
      show(await put('scripts', { scripts }), note);
      return true;
    } catch (err) {
      mount(msg, errorBox(err));
      return false;
    }
  }

  const linkOf = (f) => `${(cfg.site_url || location.origin).replace(/\/$/, '')}/f/${f.token}/${encodeURIComponent(f.name)}`;

  function editDialog(sc) {
    const d = openDialog({ wide: true });
    d.dialog.classList.add('sc-dialog');
    const name = h('input', { value: sc.name || '', maxlength: '60' });
    const note = h('input', { value: sc.note || '', maxlength: '200' });
    const content = h('textarea', { class: 'mono sc-editor', rows: '18', spellcheck: 'false', wrap: 'off', placeholder: '#!/bin/bash 不用写，直接写命令。\nset -e\necho hello' });
    content.value = sc.content || '';
    const auto = h('input', { type: 'checkbox', checked: sc.id ? sc.auto : true });
    const timeout = h('input', { type: 'number', min: '0', max: '1440', step: '1', value: sc.timeout_min ? String(sc.timeout_min) : '', placeholder: '30' });
    const fileChecks = cfg.files.map((f) => {
      const c = h('input', { type: 'checkbox', value: f.name, checked: (sc.files || []).includes(f.name) });
      return h('label', { class: 'field inline' }, c, `${f.name}（${fmtBytes(f.size)}）`);
    });
    const err = h('div');
    const ok = h('button', { type: 'submit', class: 'primary' }, '保存');
    content.addEventListener('keydown', (e) => {
      if (e.key === 'Tab') {
        e.preventDefault();
        const { selectionStart: a, selectionEnd: b, value } = content;
        content.value = value.slice(0, a) + '  ' + value.slice(b);
        content.selectionStart = content.selectionEnd = a + 2;
      }
    });
    mount(d.body, h('form', {
      onsubmit: async (e) => {
        e.preventDefault();
        ok.disabled = true;
        const next = {
          ...sc, name: name.value.trim(), note: note.value.trim(), content: content.value, auto: auto.checked,
          timeout_min: Number(timeout.value) || 0,
          files: fileChecks.map((l) => l.querySelector('input')).filter((c) => c.checked).map((c) => c.value),
        };
        const list = sc.id ? cfg.scripts.map((x) => (x.id === sc.id ? next : x)) : [...cfg.scripts, { ...next, id: 0 }];
        try {
          show(await put('scripts', { scripts: list }));
          d.close();
        } catch (ex) {
          ok.disabled = false;
          mount(err, errorBox(ex));
        }
      },
    },
    h('h2', null, sc.id ? `编辑脚本：${sc.name}` : '新建脚本'),
    h('div', { class: 'form-grid wide' }, field('名称', name), field('备注（可选）', note), field('超时（分钟）', timeout, '默认 30，超时会被停止并算失败')),
    field('脚本（bash）', content),
    h('details', { class: 'sc-help' }, h('summary', null, '脚本里能用的环境变量'),
      h('ul', { class: 'plain' }, ENV_HELP.map(([k, v]) => h('li', null, h('code', { class: 'mono' }, k), ' ', v)),
        h('li', null, '以及“变量”里设置的每一项（比如 ', h('code', { class: 'mono' }, '$TG_TOKEN'), '）。'))),
    h('div', { class: 'field' }, h('span', { class: 'field-label' }, '附带的文件（运行前传到实例的 /opt/vastmon/files/）'),
      fileChecks.length ? h('div', { class: 'sc-files' }, fileChecks) : h('span', { class: 'hint' }, '文件库是空的，先在下面上传')),
    h('label', { class: 'field inline' }, auto, '开机自动运行（开机弹窗勾“开机后自动运行脚本”时跑它；“运行脚本”里也默认勾上）'),
    err,
    h('div', { class: 'toolbar end' }, h('button', { type: 'button', onclick: d.close }, '取消'), ok)));
    name.focus();
  }

  function move(i, delta) {
    const list = [...cfg.scripts];
    const j = i + delta;
    if (j < 0 || j >= list.length) return;
    [list[i], list[j]] = [list[j], list[i]];
    saveScripts(list);
  }

  function render() {
    if (!cfg.scripts.length) {
      mount(box, empty('还没有脚本：点“新建脚本”'));
      return;
    }
    mount(box, table([
      { title: '开机自动', cell: (sc) => h('input', { type: 'checkbox', checked: sc.auto, title: '开机自动运行', onchange: (e) => saveScripts(cfg.scripts.map((x) => (x.id === sc.id ? { ...x, auto: e.target.checked } : x))) }) },
      { title: '脚本', cell: (sc) => [h('span', { class: 'strong' }, sc.name), sc.note ? h('div', { class: 'hint' }, sc.note) : null,
        h('div', { class: 'hint mono' }, `${sc.content.split('\n').length} 行`)] },
      { title: '附带文件', cell: (sc) => (sc.files.length ? sc.files.map((f) => h('div', { class: cfg.files.some((x) => x.name === f) ? 'mono' : 'mono cost-bad', title: cfg.files.some((x) => x.name === f) ? '' : '文件库里没有这个文件' }, f)) : '—') },
      { title: '超时', class: 'num nowrap', cell: (sc) => `${sc.timeout_min || 30} 分` },
      { title: '', class: 'nowrap', cell: (sc) => {
        const i = cfg.scripts.indexOf(sc);
        return [
          h('button', { type: 'button', class: 'small', title: '上移（按顺序运行）', disabled: i === 0, onclick: () => move(i, -1) }, '↑'), ' ',
          h('button', { type: 'button', class: 'small', title: '下移', disabled: i === cfg.scripts.length - 1, onclick: () => move(i, 1) }, '↓'), ' ',
          h('button', { type: 'button', class: 'small', onclick: () => editDialog(sc) }, '编辑'), ' ',
          h('button', { type: 'button', class: 'small', onclick: () => editDialog({ ...sc, id: 0, name: sc.name + ' 副本' }) }, '复制'), ' ',
          h('button', { type: 'button', class: 'small danger', onclick: () => { if (window.confirm(`删除脚本“${sc.name}”？`)) saveScripts(cfg.scripts.filter((x) => x.id !== sc.id)); } }, '删除')];
      } },
    ], cfg.scripts, (sc) => (sc.auto ? null : 'muted')));
  }

  function renderVars() {
    const rows = cfg.vars.map((v) => ({ key: v.key, hint: v.hint, value: '', keep: true }));
    const vmsg = h('div');
    const list = h('div');
    const draw = () => mount(list, rows.length ? rows.map((r, i) => h('div', { class: 'sc-var' },
      h('input', { class: 'mono', value: r.key, placeholder: '变量名，比如 TG_TOKEN', spellcheck: 'false', oninput: (e) => { r.key = e.target.value; } }),
      h('input', { class: 'mono', type: 'password', autocomplete: 'new-password', placeholder: !r.keep ? '值' : r.hint ? `已设置 ${r.hint}，留空不改` : '还没填值',
        oninput: (e) => { r.value = e.target.value; } }),
      h('button', { type: 'button', class: 'small danger', onclick: () => { rows.splice(i, 1); draw(); } }, '删除'))) : h('p', { class: 'hint' }, '还没有变量'));
    draw();
    const saveBtn = h('button', { type: 'button', class: 'primary' }, '保存变量');
    saveBtn.addEventListener('click', async () => {
      saveBtn.disabled = true;
      try {
        show(await put('scripts', { vars: rows.map((r) => ({ key: r.key.trim(), value: r.value, keep: r.keep && !r.value })) }), '变量已保存');
      } catch (err) {
        mount(vmsg, errorBox(err));
      } finally {
        saveBtn.disabled = false;
      }
    });
    mount(varsBox, list,
      h('div', { class: 'toolbar' },
        h('button', { type: 'button', onclick: () => { rows.push({ key: '', value: '', keep: false }); draw(); } }, '加一个变量'),
        saveBtn),
      vmsg);
  }

  function upload(fileList, overwrite) {
    const files = [...fileList];
    if (!files.length) return;
    const fd = new FormData();
    for (const f of files) fd.append('file', f, f.name);
    const bar = h('progress', { max: '100', value: '0' });
    mount(msg, h('div', { class: 'toolbar' }, `上传 ${files.map((f) => f.name).join('、')} `, bar));
    const xhr = new XMLHttpRequest();
    xhr.open('POST', `api/scripts/files${overwrite ? '?overwrite=1' : ''}`);
    xhr.setRequestHeader('X-Requested-With', 'vastmon');
    xhr.upload.onprogress = (e) => { if (e.lengthComputable) bar.value = (e.loaded / e.total) * 100; };
    xhr.onload = () => {
      let data = null;
      try {
        data = JSON.parse(xhr.responseText);
      } catch { /* shown below */ }
      if (xhr.status === 409 && !overwrite) {
        if (window.confirm(`${data.error.message}，覆盖吗？`)) upload(files, true);
        else mount(msg);
        return;
      }
      if (xhr.status >= 300) {
        mount(msg, errorBox(new Error((data && data.error && data.error.message) || `上传失败（HTTP ${xhr.status}）`)));
        return;
      }
      show(data, `已上传 ${files.map((f) => f.name).join('、')}`);
    };
    xhr.onerror = () => mount(msg, errorBox(new Error('上传失败：网络错误')));
    xhr.send(fd);
  }

  function renderFiles() {
    mount(filesBox, cfg.files.length ? table([
      { title: '文件', cell: (f) => [h('span', { class: 'mono strong' }, f.name), h('div', { class: 'hint mono', title: f.sha256 }, `sha256 ${f.sha256.slice(0, 16)}…`)] },
      { title: '大小', class: 'num nowrap', cell: (f) => fmtBytes(f.size) },
      { title: '上传时间', class: 'nowrap', cell: (f) => fmtTime(f.uploaded) },
      { title: '用到它的脚本', cell: (f) => cfg.scripts.filter((sc) => sc.files.includes(f.name)).map((sc) => sc.name).join('、') || '—' },
      { title: '', class: 'nowrap', cell: (f) => [
        h('button', {
          type: 'button', class: 'small', title: '不用登录就能下载的链接（脚本里可以 curl -fsSLO 它）；链接里有随机令牌，别公开',
          onclick: async (e) => { await copyText(linkOf(f)); e.currentTarget.textContent = '已复制'; },
        }, '复制下载链接'), ' ',
        h('button', {
          type: 'button', class: 'small', title: '换一个链接，旧链接马上失效',
          onclick: async () => { if (window.confirm(`给 ${f.name} 换一个下载链接？旧链接会失效。`)) show(await post(`scripts/files/${encodeURIComponent(f.name)}/token`)); },
        }, '换链接'), ' ',
        h('button', {
          type: 'button', class: 'small danger',
          onclick: async () => { if (window.confirm(`删除文件 ${f.name}？`)) show(await del(`scripts/files/${encodeURIComponent(f.name)}`)); },
        }, '删除')] },
    ], cfg.files) : empty('文件库是空的'));
  }

  const picker = h('input', { type: 'file', multiple: true, hidden: true, onchange: (e) => { upload(e.target.files, false); e.target.value = ''; } });

  (async () => {
    try {
      show(await get('scripts'));
    } catch (err) {
      mount(box, errorBox(err));
    }
  })();

  return h('section', { class: 'card' },
    h('h2', null, '开机脚本'),
    h('p', { class: 'hint' },
      '实例跑起来后由面板经 SSH 按顺序运行（开机弹窗勾“开机后自动运行脚本”，或实例卡片上点“运行脚本”）。',
      '脚本用 bash 运行，以 root 身份、在工作目录里；附带的文件先从面板传到实例。输出和退出码会显示在实例卡片上，非 0 退出算失败。'),
    msg,
    h('h3', null, '脚本'),
    h('div', { class: 'toolbar' }, h('button', { type: 'button', class: 'primary', onclick: () => editDialog({}) }, '新建脚本')),
    box,
    h('h3', null, '变量'),
    h('p', { class: 'hint' }, '运行时作为环境变量交给每个脚本，适合放 token、密码。值只存在服务器上，页面只显示末 4 位。'),
    varsBox,
    h('h3', null, '文件库'),
    h('p', { class: 'hint' }, '程序、配置之类的文件存在面板这里。脚本勾上“附带”就会在运行前传到实例的 /opt/vastmon/files/；也可以复制下载链接在别处用。'),
    h('div', { class: 'toolbar' }, h('button', { type: 'button', onclick: () => picker.click() }, '上传文件'), picker),
    filesBox);
}
