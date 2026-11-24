import { h, mount, errorBox, loading, empty, field, table } from '../lib/dom.js';
import { get, put, post } from '../lib/api.js';
import { openDialog } from '../lib/dialog.js';
import { fmtTime } from '../lib/format.js';
import { COMMON_DIRS, bytes, enabledSummary } from '../lib/downloads.js';

// Settings → 启动下载配置: named sets of files the startup downloader
// fetches and where they go, plus the shared Hugging Face / Civitai tokens
// (kept on the server; the page only sees the last 4 characters).

function hostOf(u) {
  try {
    return new URL(u).hostname;
  } catch {
    return u;
  }
}

export function downloadsSection() {
  const box = h('div', null, loading());
  const msg = h('div');
  const head = h('div');
  const tabs = h('div', { class: 'inst-tabs' });
  let cfg = null;
  let cur = 0;
  const profile = () => cfg.profiles.find((p) => p.id === cur) || cfg.profiles[0];
  const items = () => profile().items;

  const root = h('input', { class: 'mono', spellcheck: 'false' });
  const conns = h('input', { type: 'number', min: '1', max: '16', step: '1' });
  const syncInput = h('input', { type: 'checkbox' });
  const hf = h('input', { type: 'password', autocomplete: 'new-password', class: 'mono' });
  const civ = h('input', { type: 'password', autocomplete: 'new-password', class: 'mono' });
  const clearHF = h('input', { type: 'checkbox' });
  const clearCiv = h('input', { type: 'checkbox' });
  const saveBtn = h('button', { type: 'submit', class: 'primary' }, '保存');
  const datalist = h('datalist', { id: 'dl-dirs' }, COMMON_DIRS.map((d) => h('option', { value: d })));

  function show(next, note) {
    cfg = next;
    if (!cfg.profiles.some((p) => p.id === cur)) cur = cfg.default;
    root.value = cfg.root;
    conns.value = String(cfg.connections);
    syncInput.checked = cfg.sync_input;
    hf.value = '';
    civ.value = '';
    clearHF.checked = false;
    clearCiv.checked = false;
    hf.placeholder = cfg.hf_token ? `已设置 ${cfg.hf_token}，留空不改` : '没设置（公开模型不需要）';
    civ.placeholder = cfg.civitai_token ? `已设置 ${cfg.civitai_token}，留空不改` : '没设置（大部分 Civitai 模型要登录才能下）';
    const a = cfg.input_archive || {};
    importBtn.hidden = !cfg.import_available;
    mount(head,
      h('p', null, a.exists ? `ComfyUI input 备份（上传的图片 / 视频）：${bytes(a.size)}，${fmtTime(a.mtime)} 更新。` : '还没有 ComfyUI input 备份。'),
      note ? h('div', { class: 'notice' }, note) : null);
    render();
  }

  async function save(body, btn, note) {
    if (btn) btn.disabled = true;
    mount(msg);
    try {
      show(await put('downloads/config', body), note);
      return true;
    } catch (err) {
      mount(msg, errorBox(err));
      return false;
    } finally {
      if (btn) btn.disabled = false;
    }
  }

  const saveProfiles = (profiles, extra = {}, note) => save({ profiles, ...extra }, null, note);
  const saveItems = (list, note) => saveProfiles(cfg.profiles.map((p) => (p.id === profile().id ? { ...p, items: list } : p)), {}, note);

  function nameDialog(title, initial, run) {
    const d = openDialog();
    const input = h('input', { value: initial, maxlength: '40' });
    const err = h('div');
    const ok = h('button', { type: 'submit', class: 'primary' }, '确定');
    mount(d.body, h('form', {
      onsubmit: async (e) => {
        e.preventDefault();
        const name = input.value.trim();
        if (!name) return;
        ok.disabled = true;
        if (await run(name)) d.close();
        else {
          ok.disabled = false;
          mount(err, msg.firstChild ? msg.firstChild.cloneNode(true) : null);
        }
      },
    }, h('h2', null, title), field('配置名', input, '比如 MiniMax 全套、SDXL、只要 LoRA'), err,
    h('div', { class: 'toolbar end' }, h('button', { type: 'button', onclick: d.close }, '取消'), ok)));
    input.focus();
    input.select();
  }

  function newProfile(copy) {
    const base = profile();
    nameDialog(copy ? `复制“${base.name}”` : '新建下载配置', copy ? `${base.name} 副本` : '', async (name) => {
      const items = copy ? base.items.map((it) => ({ ...it, id: 0 })) : [];
      const before = new Set(cfg.profiles.map((p) => p.id));
      const ok = await saveProfiles([...cfg.profiles, { id: 0, name, items }]);
      if (ok) {
        const added = cfg.profiles.find((p) => !before.has(p.id));
        if (added) cur = added.id;
        render();
      }
      return ok;
    });
  }

  function renameProfile() {
    const p = profile();
    nameDialog(`改名：${p.name}`, p.name, (name) => saveProfiles(cfg.profiles.map((x) => (x.id === p.id ? { ...x, name } : x))));
  }

  function removeProfile() {
    const p = profile();
    if (cfg.profiles.length === 1) return;
    if (!window.confirm(`删除下载配置“${p.name}”和它的 ${p.items.length} 个下载项？`)) return;
    saveProfiles(cfg.profiles.filter((x) => x.id !== p.id));
  }

  function editDialog(item) {
    const d = openDialog({ wide: true });
    const url = h('input', { class: 'mono', value: item.url || '', spellcheck: 'false', placeholder: 'https://huggingface.co/…/resolve/main/x.safetensors 或 https://civitai.com/models/…' });
    const dir = h('input', { class: 'mono', value: item.dir || 'models/diffusion_models', list: 'dl-dirs', spellcheck: 'false' });
    const name = h('input', { class: 'mono', value: item.name || '', spellcheck: 'false', placeholder: '留空 = 用链接给的文件名' });
    const note = h('input', { value: item.note || '', maxlength: '200' });
    const enabled = h('input', { type: 'checkbox', checked: item.id ? item.enabled : true });
    let size = item.size || 0;
    const info = h('div');
    const err = h('div');
    const check = h('button', { type: 'button' }, '解析链接');
    const ok = h('button', { type: 'submit', class: 'primary' }, '保存');
    check.addEventListener('click', async () => {
      if (!url.value.trim()) return;
      check.disabled = true;
      mount(info, h('p', { class: 'muted' }, '解析中…'));
      try {
        const r = await post('downloads/resolve', { url: url.value.trim() });
        url.value = r.url;
        if (r.size > 0) size = r.size;
        if (!name.value.trim() && r.name) name.placeholder = `留空 = ${r.name}`;
        mount(info, h('div', { class: 'notice' }, `能下载：${r.name || '（链接没给文件名，请填上）'} · ${bytes(r.size)} · 实际从 ${r.host} 下载`));
      } catch (ex) {
        mount(info, errorBox(ex));
      } finally {
        check.disabled = false;
      }
    });
    mount(d.body, datalist.cloneNode(true), h('form', {
      onsubmit: async (e) => {
        e.preventDefault();
        ok.disabled = true;
        const next = { ...item, url: url.value.trim(), dir: dir.value.trim(), name: name.value.trim(), note: note.value.trim(), enabled: enabled.checked, size };
        if (next.url !== item.url && size === item.size) next.size = 0;
        const list = item.id ? items().map((it) => (it.id === item.id ? next : it)) : [...items(), { ...next, id: 0 }];
        const profiles = cfg.profiles.map((p) => (p.id === profile().id ? { ...p, items: list } : p));
        try {
          show(await put('downloads/config', { profiles }));
          d.close();
        } catch (ex) {
          ok.disabled = false;
          mount(err, errorBox(ex));
        }
      },
    },
    h('h2', null, `${item.id ? '编辑下载项' : '新增下载项'}（${profile().name}）`),
    field('下载链接', url, 'Hugging Face 的 /blob/ 链接会自动换成 /resolve/；Civitai 可以填模型页（…/models/123?modelVersionId=456）或 /api/download/models/456，链接里的 token 会挪到上面的 token 设置里'),
    h('div', { class: 'toolbar' }, check),
    info,
    h('div', { class: 'form-grid wide' },
      field('目标目录', dir, `相对 ${cfg.root}，也可以写绝对路径`),
      field('文件名（可选）', name, '工作流里引用的名字和链接给的不一样时填'),
      field('备注', note)),
    h('label', { class: 'field inline' }, enabled, '启用（开机自动下载和“启动下载器”默认勾选）'),
    err,
    h('div', { class: 'toolbar end' }, h('button', { type: 'button', onclick: d.close }, '取消'), ok)));
    url.focus();
  }

  function move(i, delta) {
    const list = [...items()];
    const j = i + delta;
    if (j < 0 || j >= list.length) return;
    [list[i], list[j]] = [list[j], list[i]];
    saveItems(list);
  }

  function renderTabs() {
    const p = profile();
    const s = enabledSummary(p);
    mount(tabs,
      cfg.profiles.map((x) => h('button', {
        type: 'button', class: x.id === p.id ? 'tab active' : 'tab',
        title: x.id === cfg.default ? '默认配置：开机弹窗和“启动下载器”默认选它' : '',
        onclick: () => { cur = x.id; render(); },
      }, `${x.name}（${x.items.length}）`, x.id === cfg.default ? h('span', { class: 'badge on' }, '默认') : null)),
      h('span', { class: 'spacer' }),
      h('button', { type: 'button', class: 'small', onclick: () => newProfile(false) }, '新建配置'),
      h('button', { type: 'button', class: 'small', onclick: () => newProfile(true) }, '复制这套'),
      h('button', { type: 'button', class: 'small', onclick: renameProfile }, '改名'),
      p.id !== cfg.default ? h('button', { type: 'button', class: 'small', onclick: () => saveProfiles(cfg.profiles, { default: p.id }) }, '设为默认') : null,
      cfg.profiles.length > 1 ? h('button', { type: 'button', class: 'small danger', onclick: removeProfile }, '删除这套') : null);
    return s;
  }

  function render() {
    const s = renderTabs();
    mount(sumLine, `“${profile().name}”已启用 ${s.count} 个文件，共 ${bytes(s.size)}${s.unknown ? '（有的大小未知）' : ''}。`);
    if (!items().length) {
      mount(box, empty('这套还没有下载项：点“新增”，或从 comfy_kit 的 sources.json 导入'));
      return;
    }
    mount(box, table([
      { title: '启用', cell: (it) => h('input', { type: 'checkbox', checked: it.enabled, onchange: (e) => saveItems(items().map((x) => (x.id === it.id ? { ...x, enabled: e.target.checked } : x))) }) },
      { title: '文件', cell: (it) => [h('span', { class: 'mono strong' }, it.name || '（用链接给的文件名）'), it.note ? h('div', { class: 'hint' }, it.note) : null] },
      { title: '目录', class: 'mono', cell: (it) => it.dir },
      { title: '大小', class: 'num nowrap', cell: (it) => bytes(it.size) },
      { title: '来源', cell: (it) => h('a', { href: it.url, target: '_blank', rel: 'noopener noreferrer', title: it.url }, hostOf(it.url)) },
      { title: '', class: 'nowrap', cell: (it) => {
        const i = items().indexOf(it);
        return [
          h('button', { type: 'button', class: 'small', title: '上移（按列表顺序下载）', disabled: i === 0, onclick: () => move(i, -1) }, '↑'), ' ',
          h('button', { type: 'button', class: 'small', title: '下移', disabled: i === items().length - 1, onclick: () => move(i, 1) }, '↓'), ' ',
          h('button', { type: 'button', class: 'small', onclick: () => editDialog(it) }, '编辑'), ' ',
          h('button', {
            type: 'button', class: 'small danger',
            onclick: () => { if (window.confirm(`删除下载项 ${it.name || it.url}？`)) saveItems(items().filter((x) => x.id !== it.id)); },
          }, '删除')];
      } },
    ], items(), (it) => (it.enabled ? null : 'muted')));
  }

  const importBtn = h('button', {
    type: 'button',
    title: '把 backups/sources.json 里的模型加进当前这套（同名的跳过），没设置 token 时从 d.sh 读',
    onclick: async (e) => {
      const btn = e.currentTarget;
      btn.disabled = true;
      try {
        const r = await post('downloads/import', { profile_id: profile().id });
        show(r, r.notes.join('；'));
      } catch (err) {
        mount(msg, errorBox(err));
      } finally {
        btn.disabled = false;
      }
    },
  }, '从 comfy_kit 导入');

  const sumLine = h('p');

  (async () => {
    try {
      show(await get('downloads/config'));
    } catch (err) {
      mount(box, errorBox(err));
    }
  })();

  return h('section', { class: 'card' },
    datalist,
    h('h2', null, '启动下载配置'),
    h('p', { class: 'hint' },
      '实例卡片上的“启动下载器”和开机时勾的“开机后自动启动下载器”按这里的列表下载（可以建好几套，开机 / 启动时选用哪套）：实例上用 aria2 多线程下到工作目录下的各个目录，',
      '实例上已有的同名同大小文件跳过，下了一半的续传。面板先用 token 解析出真正的下载地址再交给实例，token 一般不会出现在实例上。'),
    head,
    h('form', {
      onsubmit: (e) => {
        e.preventDefault();
        save({
          root: root.value.trim(), connections: Number(conns.value), sync_input: syncInput.checked,
          hf_token: hf.value.trim(), civitai_token: civ.value.trim(), clear_hf_token: clearHF.checked, clear_civitai_token: clearCiv.checked,
        }, saveBtn, '已保存');
      },
    },
    h('div', { class: 'form-grid wide' },
      field('工作目录（实例上）', root, '下载项的相对目录以它为基准，ComfyUI input 同步到它下面的 input/；用 vast 的 ComfyUI 模板时是 /workspace/ComfyUI'),
      field('aria2 线程数', conns, '1–16，每个文件分几段同时下'),
      field('Hugging Face token', hf, h('label', { class: 'inline' }, clearHF, ' 清除')),
      field('Civitai token', civ, h('label', { class: 'inline' }, clearCiv, ' 清除'))),
    h('label', { class: 'field inline' }, syncInput, '启动下载器时默认先同步 ComfyUI 的 input（把 backups/input.tar.gz 解到工作目录的 input/，只对 ComfyUI 有用）'),
    h('div', { class: 'toolbar' }, saveBtn)),
    h('h3', null, '下载列表'),
    tabs,
    sumLine,
    h('div', { class: 'toolbar' },
      h('button', { type: 'button', class: 'primary', onclick: () => editDialog({}) }, '新增'),
      importBtn),
    msg, box);
}
