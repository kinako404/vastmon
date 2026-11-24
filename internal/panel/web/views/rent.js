import { h, mount, field, errorBox, kv } from '../lib/dom.js';
import { get, post } from '../lib/api.js';
import { fixed, gb, costClass, shortCPU, cores } from '../lib/format.js';
import { fmtGeo } from '../lib/geo.js';
import { bytes, enabledSummary } from '../lib/downloads.js';

const CUSTOM = '__custom__';
const HOURS_PER_MONTH = 720;

// templatesOnce loads templates and the remembered choices once per page
// load; a failed load is retried the next time the dialog opens.
let templatesLoad = null;
function templatesOnce() {
  if (!templatesLoad) {
    templatesLoad = get('templates');
    templatesLoad.catch(() => { templatesLoad = null; });
  }
  return templatesLoad;
}

// price is the hourly price of m with disk GB: vast's total at the priced
// disk size, with that disk part swapped for the chosen size.
function price(m, disk) {
  const gpu = m.dph_total - m.storage_total_cost;
  const diskH = m.storage_cost * disk / HOURS_PER_MONTH;
  return { gpu, diskH, total: gpu + diskH };
}

function templateLabel(t) {
  return `${t.name}（${t.image}${t.tag ? ':' + t.tag : ''}）`;
}

// openRentDialog asks for a template and disk size and rents machine m.
// onRented(result) is called after a successful rental. opts.blacklisted,
// when set, is the machine's blacklist entry, shown as a warning.
export function openRentDialog(m, onRented, opts = {}) {
  const body = h('div', null, h('p', { class: 'muted' }, '读取模板…'));
  const dialog = h('dialog', { class: 'dialog wide' }, body);
  let busy = false;
  dialog.addEventListener('cancel', (e) => { if (busy) e.preventDefault(); });
  dialog.addEventListener('close', () => dialog.remove());
  document.body.appendChild(dialog);
  dialog.showModal();
  const close = () => { if (!busy) dialog.close(); };

  templatesOnce().then((data) => render(data), (err) => {
    mount(body, errorBox(err), h('div', { class: 'toolbar end' }, h('button', { type: 'button', onclick: close }, '关闭')));
  });

  function render({ templates, defaults }) {
    const maxDisk = Math.floor(m.disk_space);
    const byHash = new Map(templates.map((t) => [t.hash_id, t]));
    const mine = templates.filter((t) => t.mine);
    const rec = templates.filter((t) => !t.mine);
    const known = byHash.has(defaults.template_hash_id);
    const select = h('select', null,
      mine.length ? h('optgroup', { label: '我的模板' }, mine.map((t) => h('option', { value: t.hash_id }, templateLabel(t)))) : null,
      h('optgroup', { label: 'vast 推荐模板' }, rec.map((t) => h('option', { value: t.hash_id }, templateLabel(t)))),
      h('option', { value: CUSTOM }, '自定义 hash_id…'));
    select.value = known ? defaults.template_hash_id : (defaults.template_hash_id ? CUSTOM : (rec[0] || {}).hash_id || CUSTOM);
    const custom = h('input', {
      class: 'mono', placeholder: '32 位模板 hash_id', value: known ? '' : (defaults.template_hash_id || ''),
      spellcheck: 'false', autocomplete: 'off',
    });
    const customField = field('模板 hash_id', custom, '在 vast 模板页的链接里能找到，例如 …?template_id=<hash_id>');
    const tplHint = h('span', { class: 'hint' });
    const disk = h('input', {
      type: 'number', min: '10', max: String(maxDisk), step: '1',
      value: String(Math.min(maxDisk, Math.max(10, Math.round(defaults.disk || 96)))),
    });
    const label = h('input', { value: defaults.label || 'vastmon', maxlength: '64' });
    const noTmux = h('input', { type: 'checkbox', checked: defaults.no_tmux !== false });
    const autoDL = h('input', { type: 'checkbox', checked: defaults.auto_download !== false });
    const autoHint = h('p', { class: 'hint tight' }, '读取下载配置…');
    const autoSc = h('input', { type: 'checkbox', checked: defaults.auto_scripts !== false });
    const scHint = h('p', { class: 'hint tight' }, '读取开机脚本…');
    get('scripts').then((sc) => {
      const on = sc.scripts.filter((x) => x.auto);
      mount(scHint, on.length
        ? `实例跑起来后按顺序运行：${on.map((x) => x.name).join('、')}。在“设置 → 开机脚本”里改。`
        : h('span', { class: 'cost-warn' }, '还没有勾“开机自动”的脚本，到“设置 → 开机脚本”添加。'));
    }, (err) => mount(scHint, h('span', { class: 'cost-bad' }, `读不到开机脚本：${err.message}`)));
    const dlSet = h('select', { hidden: true });
    let dlSize = 0;
    let dlCfg = null;
    const showSet = () => {
      if (!dlCfg) return;
      const set = dlCfg.profiles.find((p) => p.id === Number(dlSet.value)) || dlCfg.profiles[0];
      const s = enabledSummary(set);
      dlSize = s.size;
      mount(autoHint, s.count
        ? `实例跑起来、SSH 连得上后，用 aria2 下载“${set.name}”里启用的 ${s.count} 个文件（${bytes(s.size)}）${dlCfg.sync_input ? '，并先同步 ComfyUI 的 input' : ''}。在“设置 → 启动下载配置”里改。`
        : h('span', { class: 'cost-warn' }, `“${set.name}”里还没有启用的文件，到“设置 → 启动下载配置”添加。`));
      update();
    };
    dlSet.addEventListener('change', showSet);
    get('downloads/config').then((cfg) => {
      dlCfg = cfg;
      const want = cfg.profiles.some((p) => p.id === defaults.download_profile) ? defaults.download_profile : cfg.default;
      mount(dlSet, cfg.profiles.map((p) => h('option', { value: String(p.id), selected: p.id === want }, `${p.name}${p.id === cfg.default ? '（默认）' : ''}`)));
      dlSet.hidden = false;
      showSet();
    }, (err) => mount(autoHint, h('span', { class: 'cost-bad' }, `读不到下载配置：${err.message}`)));
    const priceBox = h('div', { class: 'price-box' });
    const msg = h('div');
    const confirm = h('button', { type: 'submit', class: 'primary' }, '确认开机');
    const cancel = h('button', { type: 'button', onclick: close }, '取消');

    const chosen = () => (select.value === CUSTOM ? null : byHash.get(select.value));
    const hash = () => (select.value === CUSTOM ? custom.value.trim().toLowerCase() : select.value);

    function update() {
      const t = chosen();
      customField.hidden = select.value !== CUSTOM;
      mount(tplHint, t
        ? `推荐磁盘 ${fixed(t.recommended_disk_space, 0)} GB${t.desc ? '。' + t.desc : ''}`
        : '自定义模板：镜像、启动方式和端口都按模板里的设置。');
      const d = Number(disk.value);
      let problem = '';
      if (!Number.isFinite(d) || d < 10) problem = '磁盘至少 10 GB';
      else if (d > maxDisk) problem = `这台机器最多 ${maxDisk} GB`;
      else if (!/^[0-9a-f]{32}$/.test(hash())) problem = '请选模板或填 32 位 hash_id';
      if (!problem && autoDL.checked && dlSize > d * 1e9) problem = `下载列表共 ${bytes(dlSize)}，比磁盘 ${d} GB 还大：放不下的会被跳过，可以加大磁盘或在设置里少启用几个`;
      if (problem && !problem.startsWith('下载列表')) {
        mount(priceBox, h('span', { class: 'cost-bad' }, problem));
        confirm.disabled = true;
        mount(confirm, '确认开机');
        return;
      }
      const p = price(m, d);
      mount(priceBox,
        problem ? h('div', { class: 'cost-warn' }, problem) : null,
        h('div', { class: 'price-total' }, `$${fixed(p.total, 3)}/h`),
        h('div', { class: 'hint' },
          `GPU $${fixed(p.gpu, 3)} + 磁盘 $${fixed(p.diskH, 3)}（$${fixed(m.storage_cost, 3)}/GB/月 × ${d} GB），`,
          `约 $${fixed(p.total * 24, 2)}/天，不含流量费。按秒计费，开着就扣钱，不用时记得在 vast 上停止或销毁。`));
      confirm.disabled = busy;
      mount(confirm, `确认开机 $${fixed(p.total, 3)}/h`);
    }

    select.addEventListener('change', () => {
      const t = chosen();
      if (t && Number(disk.value) < t.recommended_disk_space) disk.value = String(Math.min(maxDisk, Math.ceil(t.recommended_disk_space)));
      update();
      if (select.value === CUSTOM) custom.focus();
    });
    for (const el of [custom, disk]) el.addEventListener('input', update);
    autoDL.addEventListener('change', update);

    const form = h('form', {
      onsubmit: async (e) => {
        e.preventDefault();
        const d = Number(disk.value);
        const p = price(m, d);
        const t = chosen();
        busy = true;
        confirm.disabled = true;
        cancel.disabled = true;
        mount(msg, h('p', { class: 'muted' }, '正在开机…'));
        try {
          const res = await post('rent', {
            machine_id: m.machine_id,
            num_gpus: m.num_gpus,
            template_hash_id: hash(),
            template_name: t ? t.name : '',
            disk: d,
            label: label.value.trim(),
            no_tmux: noTmux.checked,
            auto_download: autoDL.checked,
            download_profile: Number(dlSet.value) || 0,
            auto_scripts: autoSc.checked,
            max_dph: p.total,
          });
          busy = false;
          templatesLoad = null;
          mount(body,
            h('h2', null, '已开机'),
            kv([
              ['实例 ID', h('span', { class: 'mono' }, String(res.instance_id))],
              ['价格', `$${fixed(res.dph, 3)}/h`],
              ['模板', t ? t.name : hash()],
              ['磁盘', `${d} GB`],
            ]),
            h('p', { class: 'hint' }, '实例要几分钟拉镜像、启动，状态在“实例”页查看。',
              noTmux.checked ? '跑起来后面板会自动关掉登录自动 tmux。' : '',
              autoDL.checked && !res.auto_download_error ? '跑起来后会自动启动下载器，进度在实例卡片上。' : '',
              autoSc.checked && !res.auto_download_error ? '开机脚本也会自动运行。' : ''),
            res.auto_download_error ? h('div', { class: 'error' }, res.auto_download_error) : null,
            h('div', { class: 'toolbar end' },
              h('a', { class: 'button primary', href: '#/instances', onclick: () => dialog.close() }, '查看实例'),
              h('button', { type: 'button', onclick: () => dialog.close() }, '关闭')));
          if (onRented) onRented(res);
        } catch (err) {
          busy = false;
          cancel.disabled = false;
          mount(msg, errorBox(err));
          update();
        }
      },
    },
    h('h2', null, `开机：机器 ${m.machine_id} · ${m.num_gpus}× ${m.gpu_name}`),
    opts.blacklisted ? h('div', { class: 'error' },
      `⛔ 这台机器在黑名单里${opts.blacklisted.note ? '：' + opts.blacklisted.note : ''}。确定还要开吗？`) : null,
    kv([
      ['位置', h('span', { title: m.geolocation || '' }, fmtGeo(m.geolocation))],
      ['CPU / 内存', `${shortCPU(m.cpu_name)} · ${cores(m.cpu_cores_effective)} 核 / ${gb(m.cpu_ram)} GB`],
      ['带宽', `下行 ${fixed(m.inet_down, 0)} / 上行 ${fixed(m.inet_up, 0)} Mbps`],
      ['流量费', [
        '下行 ', h('span', { class: costClass(m.internet_down_cost_per_tb) }, '$' + fixed(m.internet_down_cost_per_tb, 2)),
        ' / 上行 ', h('span', { class: costClass(m.internet_up_cost_per_tb) }, '$' + fixed(m.internet_up_cost_per_tb, 2)), ' 每 TB',
      ]],
    ]),
    h('div', { class: 'dialog-fields' },
      field('模板', select, tplHint),
      customField,
      field('磁盘 GB', disk, `这台机器最多可用 ${maxDisk} GB，开机后不能再改大`),
      field('名称（可选）', label),
      h('label', { class: 'field inline', title: 'vast 镜像默认一用 SSH 登录就自动进 tmux，网页终端和平时 ssh 都不好用' },
        noTmux, '关闭登录自动 tmux（推荐）'),
      h('p', { class: 'hint tight' }, '实例跑起来后面板通过 SSH 建 ~/.no_auto_tmux；想要 tmux 时自己输入 tmux 即可。'),
      h('div', { class: 'toolbar tight' }, h('label', { class: 'field inline' }, autoDL, '开机后自动启动下载器，用'), dlSet),
      autoHint,
      h('label', { class: 'field inline' }, autoSc, '开机后自动运行脚本'),
      scHint),
    priceBox,
    msg,
    h('div', { class: 'toolbar end' }, cancel, confirm));
    mount(body, form);
    update();
  }
}
