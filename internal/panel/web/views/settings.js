import { h, mount, errorBox, loading, field, kv } from '../lib/dom.js';
import { get, put, post, currentSession, loginHash } from '../lib/api.js';
import { copyText } from '../lib/dialog.js';
import { baselinesSection } from './baselines.js';
import { downloadsSection } from './dlconfig.js';
import { scriptsSection } from './scriptsconfig.js';
import { notifySections } from './notify.js';

// SSH key used by the panel to log in to instances (benchmarks). The key and
// its passphrase stay on the server; this page only sees whether they are
// set, the derived public key and its fingerprint.

function sshStatus(s) {
  return kv([
    ['私钥文件', h('span', { class: 'mono' }, s.key_path || '（没配置）')],
    ['来源', s.using_project ? '项目目录里的 private（密码取 passwd.txt），默认' : '自己配置的'],
    ['私钥密码', s.has_passphrase ? '已设置' : '没有'],
    s.fingerprint ? ['指纹', h('span', { class: 'mono' }, s.fingerprint)] : null,
    s.key_error ? ['问题', h('span', { class: 'cost-bad' }, s.key_error)] : null,
    s.public_key ? ['在 vast 账户里', s.in_vast_account === undefined ? '—'
      : s.in_vast_account ? h('span', { class: 'cost-ok' }, '是：新开的实例自动能用这把钥匙登录')
        : h('span', { class: 'cost-warn' }, '不在：新实例默认登不上，到实例卡片的“SSH 密钥”里加，或在 vast 网站 Keys 页面添加这个公钥')] : null,
  ]);
}

// passwordSection changes the panel password. Other logged-in browsers are
// signed out; this one keeps a fresh session.
function passwordSection() {
  const cur = h('input', { type: 'password', autocomplete: 'current-password', required: true });
  const next = h('input', { type: 'password', autocomplete: 'new-password', required: true, minlength: '8', maxlength: '128' });
  const again = h('input', { type: 'password', autocomplete: 'new-password', required: true });
  const msg = h('div');
  const ok = h('button', { type: 'submit', class: 'primary' }, '修改密码');
  return h('section', { class: 'card' },
    h('h2', null, '面板密码'),
    h('form', {
      onsubmit: async (e) => {
        e.preventDefault();
        if (next.value !== again.value) {
          mount(msg, errorBox(new Error('两次输入的新密码不一样')));
          return;
        }
        ok.disabled = true;
        mount(msg);
        try {
          await post('settings/password', { current: cur.value, new: next.value });
          cur.value = '';
          next.value = '';
          again.value = '';
          mount(msg, h('div', { class: 'notice' }, '密码已修改。其它浏览器 / 设备上的登录都已失效，这里继续登录着。'));
        } catch (err) {
          mount(msg, errorBox(err));
        } finally {
          ok.disabled = false;
        }
      },
    },
    h('div', { class: 'form-grid wide' },
      field('当前密码', cur),
      field('新密码', next, '至少 8 个字符'),
      field('再输一次新密码', again)),
    msg,
    h('div', { class: 'toolbar' }, ok)));
}

export async function settingsView(ctx) {
  if (!currentSession().logged_in) {
    ctx.show(h('h1', null, '设置'), h('p', null, '需要先', h('a', { href: loginHash('#/settings') }, '登录'), '。'));
    return;
  }
  const statusBox = h('div', null, loading());
  const pubBox = h('div');
  const formMsg = h('div');
  const testBox = h('div');
  let s = null;

  const path = h('input', { class: 'mono', spellcheck: 'false', autocomplete: 'off' });
  const pass = h('input', { type: 'password', autocomplete: 'new-password' });
  const clear = h('input', { type: 'checkbox' });
  const content = h('textarea', { class: 'mono', rows: '5', spellcheck: 'false', placeholder: '-----BEGIN OPENSSH PRIVATE KEY-----\n…\n-----END OPENSSH PRIVATE KEY-----' });
  const save = h('button', { type: 'submit', class: 'primary' }, '保存');
  const useProject = h('button', { type: 'button' }, '改回项目目录的 private');

  function show(next) {
    s = next;
    mount(statusBox, sshStatus(s));
    path.value = s.key_path || '';
    pass.value = '';
    pass.placeholder = s.has_passphrase ? '已设置，留空不改' : '私钥没有密码就留空';
    clear.checked = false;
    content.value = '';
    useProject.hidden = !s.project_available || s.using_project;
    mount(pubBox, s.public_key ? [
      h('h3', null, '公钥'),
      h('pre', { class: 'code' }, s.public_key),
      h('button', {
        type: 'button', class: 'small',
        onclick: async (e) => {
          await copyText(s.public_key);
          e.currentTarget.textContent = '已复制';
        },
      }, '复制公钥'),
    ] : null);
  }

  async function submit(body, btn) {
    btn.disabled = true;
    mount(formMsg, h('p', { class: 'muted' }, '保存中…'));
    try {
      show(await put('settings/ssh', body));
      mount(formMsg, h('div', { class: 'notice' }, '已保存，私钥和密码验证通过。'));
    } catch (err) {
      mount(formMsg, errorBox(err));
    } finally {
      btn.disabled = false;
    }
  }

  useProject.addEventListener('click', () => submit({ use_project: true }, useProject));

  const testSelect = h('select');
  const testBtn = h('button', { type: 'button' }, '测试连接');
  testBtn.addEventListener('click', async () => {
    if (!testSelect.value) return;
    testBtn.disabled = true;
    mount(testBox, h('p', { class: 'muted' }, '连接中…'));
    try {
      const r = await post('settings/ssh/test', { instance_id: Number(testSelect.value) });
      mount(testBox, h('div', { class: r.ok ? 'notice' : 'error' },
        r.ok ? `连上了 ${r.target.host}:${r.target.port}` : `没连上 ${r.target.host}:${r.target.port}`),
      h('pre', { class: 'code' }, r.output || '（没有输出）'));
    } catch (err) {
      mount(testBox, errorBox(err));
    } finally {
      testBtn.disabled = false;
    }
  });

  ctx.show(
    h('h1', null, '设置'),
    passwordSection(),
    notifySections(),
    h('section', { class: 'card' },
      h('h2', null, 'SSH 私钥'),
      h('p', { class: 'hint' }, '面板用这把私钥登录实例（算力检测等）。私钥和密码只保存在服务器上，不会发到浏览器。'),
      statusBox, pubBox,
      h('h3', null, '修改'),
      h('form', { onsubmit: (e) => { e.preventDefault(); submit({ key_path: content.value.trim() ? '' : path.value.trim(), passphrase: pass.value, clear_passphrase: clear.checked, key_content: content.value }, save); } },
        field('私钥文件路径（服务器上的）', path),
        field('私钥密码', pass),
        h('label', { class: 'field inline' }, clear, '清除密码（私钥没有密码时勾上）'),
        field('或者直接粘贴私钥内容', content, '粘贴后会存成 vastmon 目录里的 ssh_key（权限 600），并替换上面的路径'),
        formMsg,
        h('div', { class: 'toolbar' }, save, useProject))),
    h('section', { class: 'card' },
      h('h2', null, '测试连接'),
      h('div', { class: 'toolbar' }, testSelect, testBtn),
      testBox),
    downloadsSection(),
    scriptsSection(),
    baselinesSection());

  try {
    show(await get('settings/ssh', ctx.signal));
  } catch (err) {
    mount(statusBox, errorBox(err));
  }
  try {
    const d = await get('instances', ctx.signal);
    const running = d.instances.filter((x) => x.actual_status === 'running');
    mount(testSelect, running.length
      ? running.map((x) => h('option', { value: String(x.id) }, `实例 ${x.id} · ${x.num_gpus}× ${x.gpu_name}${x.label ? ' · ' + x.label : ''}`))
      : h('option', { value: '' }, '没有运行中的实例'));
    testBtn.disabled = !running.length;
  } catch (err) {
    mount(testBox, errorBox(err));
  }
}
