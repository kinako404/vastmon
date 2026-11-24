import { h, mount, errorBox, loading, empty, field, table } from '../lib/dom.js';
import { get, put, post, del } from '../lib/api.js';
import { fmtTime, fmtAgo } from '../lib/format.js';

// Settings → 站点 / 浏览器通知. Browsers only allow notifications on HTTPS
// (or localhost), so the panel is meant to sit behind the user's CDN /
// reverse proxy at the site URL. Binding a browser = a Web Push
// subscription sent to the server; the server pushes events to every bound
// browser even when no panel tab is open.

const KIND = {
  instance: '实例', 'instance-bad': '实例异常', download: '下载器', balance: '余额', spend: '今日花费', script: '开机脚本', daily: '每日消费', test: '测试',
};

function b64uToBytes(s) {
  const pad = '='.repeat((4 - (s.length % 4)) % 4);
  const raw = atob((s + pad).replace(/-/g, '+').replace(/_/g, '/'));
  return Uint8Array.from(raw, (c) => c.charCodeAt(0));
}

function bytesToB64u(buf) {
  return btoa(String.fromCharCode(...new Uint8Array(buf))).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

async function endpointKey(endpoint) {
  const sum = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(endpoint));
  return [...new Uint8Array(sum).slice(0, 8)].map((b) => b.toString(16).padStart(2, '0')).join('');
}

// browserName is a short label for this browser, e.g. "Chrome · Windows".
function browserName() {
  const ua = navigator.userAgent;
  const b = /Edg\//.test(ua) ? 'Edge' : /OPR\//.test(ua) ? 'Opera' : /Firefox\//.test(ua) ? 'Firefox'
    : /Chrome\//.test(ua) ? 'Chrome' : /Safari\//.test(ua) ? 'Safari' : '浏览器';
  const os = /Windows/.test(ua) ? 'Windows' : /Android/.test(ua) ? 'Android' : /iPhone|iPad/.test(ua) ? 'iOS'
    : /Mac OS X/.test(ua) ? 'macOS' : /Linux/.test(ua) ? 'Linux' : '';
  return os ? `${b} · ${os}` : b;
}

const pushSupported = () => 'serviceWorker' in navigator && 'PushManager' in window && 'Notification' in window;

async function currentSubscription() {
  if (!pushSupported()) return null;
  const reg = await navigator.serviceWorker.getRegistration();
  return reg ? reg.pushManager.getSubscription() : null;
}

function sameKey(sub, vapid) {
  const k = sub && sub.options && sub.options.applicationServerKey;
  return !k || bytesToB64u(k) === vapid;
}

export function notifySections() {
  const siteBox = h('section', { class: 'card' }, h('h2', null, '站点'), loading());
  const box = h('section', { class: 'card' }, h('h2', null, '浏览器通知'), loading());
  let data = null;

  async function load() {
    try {
      data = await get('settings/notify');
      renderSite();
      await render();
    } catch (err) {
      mount(box, h('h2', null, '浏览器通知'), errorBox(err));
    }
  }

  // saveConfig saves the settings; both cards are redrawn, so the "saved"
  // note goes to the redrawn card the save came from.
  async function saveConfig(next, btn, msgBox, fromSite) {
    if (btn) btn.disabled = true;
    try {
      data = await put('settings/notify', next);
      const ok = h('div', { class: 'notice' }, '已保存');
      renderSite(fromSite ? ok : null);
      await render(null, fromSite ? null : ok);
    } catch (err) {
      mount(msgBox, errorBox(err));
    } finally {
      if (btn) btn.disabled = false;
    }
  }

  function renderSite(note) {
    const c = data.config;
    const input = h('input', { class: 'mono', value: c.site_url, placeholder: 'https://vast.example.com', spellcheck: 'false' });
    const msg = h('div', null, note);
    const save = h('button', { type: 'submit', class: 'primary' }, '保存');
    let originNote = null;
    if (c.site_url) {
      let site = '';
      try {
        site = new URL(c.site_url).origin;
      } catch { /* shown as is */ }
      originNote = site === location.origin
        ? h('p', { class: 'hint' }, '你现在就是通过站点 URL 打开的。')
        : h('p', { class: 'cost-warn' }, '你现在是通过 ', h('span', { class: 'mono' }, location.origin), ' 打开的，不是站点 URL。',
          h('a', { href: c.site_url + '/#/settings' }, '用站点 URL 打开'));
    }
    mount(siteBox,
      h('h2', null, '站点'),
      h('form', { onsubmit: (e) => { e.preventDefault(); saveConfig({ ...data.config, site_url: input.value.trim() }, save, msg, true); } },
        field('站点 URL', input,
          '你在面板前面套的 CDN / 反向代理的 HTTPS 地址（回源到面板的 http 端口，默认 8090）。浏览器只在 HTTPS 下允许通知；通知点开后打开这个地址。CDN 要放行 WebSocket（网页终端要用），不要缓存 /api/。'),
        originNote, msg,
        h('div', { class: 'toolbar' }, save)));
  }

  async function bind(btn, msgBox) {
    btn.disabled = true;
    mount(msgBox, h('p', { class: 'muted' }, '请求通知权限…'));
    try {
      const perm = await Notification.requestPermission();
      if (perm !== 'granted') throw new Error(perm === 'denied' ? '通知权限被拒绝了：到地址栏左边的网站设置里把“通知”改成允许，再点一次' : '没有允许通知');
      const reg = await navigator.serviceWorker.register('sw.js');
      await navigator.serviceWorker.ready;
      let sub = await reg.pushManager.getSubscription();
      if (sub && !sameKey(sub, data.vapid_public)) {
        await sub.unsubscribe();
        sub = null;
      }
      if (!sub) sub = await reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: b64uToBytes(data.vapid_public) });
      const j = sub.toJSON();
      const r = await post('push/subscribe', { endpoint: j.endpoint, keys: j.keys, name: browserName() });
      data.subs = r.subs;
      await render(r.sent ? h('div', { class: 'notice' }, '已开启，并发了一条测试通知。')
        : h('div', { class: 'error' }, `已绑定，但测试通知暂时没发出去（刚开的订阅有时要等一会儿才生效，过一分钟点“发测试通知”再试）：${(r.errors || []).join('；')}`));
    } catch (err) {
      mount(msgBox, errorBox(err));
      btn.disabled = false;
    }
  }

  async function unbind(btn, sub) {
    btn.disabled = true;
    try {
      const r = await post('push/unsubscribe', { endpoint: sub.endpoint });
      await sub.unsubscribe();
      data.subs = r.subs;
      await render(h('div', { class: 'notice' }, '这个浏览器不再收通知了。'));
    } catch (err) {
      btn.disabled = false;
      await render(errorBox(err));
    }
  }

  async function test(btn, msgBox) {
    btn.disabled = true;
    try {
      const r = await post('push/test', {});
      data.subs = r.subs;
      mount(msgBox, r.errors && r.errors.length
        ? h('div', { class: 'error' }, `发出 ${r.sent} 条；没发出去：${r.errors.join('；')}`)
        : h('div', { class: 'notice' }, r.sent ? `已发给 ${r.sent} 个浏览器` : '还没有绑定的浏览器'));
      renderSubs();
    } catch (err) {
      mount(msgBox, errorBox(err));
    } finally {
      btn.disabled = false;
    }
  }

  const subsBox = h('div');
  function renderSubs() {
    mount(subsBox, data.subs.length ? table([
      { title: '浏览器', cell: (s) => [h('span', { class: 'strong' }, s.name), s.key === thisKey ? h('span', { class: 'badge on' }, '就是这个') : null] },
      { title: '推送服务', class: 'mono', cell: (s) => s.host },
      { title: '绑定时间', class: 'nowrap', cell: (s) => fmtTime(s.created) },
      { title: '最近一次推送', cell: (s) => (!s.last_ts ? '—' : s.last_err
        ? [h('span', { class: 'badge bad' }, '失败'), ' ', fmtAgo(s.last_ts), h('div', { class: 'hint' }, s.last_err)]
        : [h('span', { class: 'badge on' }, '成功'), ' ', fmtAgo(s.last_ts)]) },
      { title: '', cell: (s) => h('button', {
        type: 'button', class: 'small danger',
        onclick: async () => {
          if (!window.confirm(`解绑 ${s.name}？它以后收不到通知。`)) return;
          data.subs = (await del(`push/subs/${s.id}`)).subs;
          await render();
        },
      }, '解绑') },
    ], data.subs) : empty('还没有绑定的浏览器'));
  }

  let thisKey = '';

  // render redraws the notification card; note, when given, is shown under
  // this browser's status, evNote under the event settings.
  async function render(note, evNote) {
    const c = data.config;
    const msg = h('div');
    const status = h('div', { class: 'toolbar' });
    let sub = null;
    thisKey = '';
    if (!window.isSecureContext) {
      mount(status, h('div', { class: 'error' }, `现在是 ${location.origin}（不是 HTTPS），浏览器不允许通知。`,
        c.site_url ? ['用站点 URL ', h('a', { href: c.site_url + '/#/settings' }, c.site_url), ' 打开面板再来开启。'] : '先在上面填好站点 URL（你套的 CDN 的 HTTPS 地址），再从那个地址打开面板。'));
    } else if (!pushSupported()) {
      mount(status, h('div', { class: 'error' }, '这个浏览器不支持网页推送通知。'));
    } else {
      sub = await currentSubscription().catch(() => null);
      if (sub) thisKey = await endpointKey(sub.endpoint);
      const bound = sub && sameKey(sub, data.vapid_public) && data.subs.some((s) => s.key === thisKey);
      const perm = Notification.permission;
      if (bound && perm === 'granted') {
        mount(status, h('span', { class: 'badge on' }, '这个浏览器已开启通知'),
          h('button', { type: 'button', class: 'small', onclick: (e) => unbind(e.currentTarget, sub) }, '在这个浏览器关闭通知'));
      } else {
        const btn = h('button', { type: 'button', class: 'primary' }, '在这个浏览器开启通知');
        btn.addEventListener('click', () => bind(btn, msg));
        mount(status, btn, perm === 'denied'
          ? h('span', { class: 'cost-bad' }, '通知权限被拒绝了：到地址栏左边的网站设置里把“通知”改成允许')
          : h('span', { class: 'hint' }, `会弹出浏览器的权限询问。绑定后就算没开着面板也能收到（${browserName()}）。`));
      }
    }
    if (note) mount(msg, note);
    renderSubs();

    const inst = h('input', { type: 'checkbox', checked: c.instance });
    const dl = h('input', { type: 'checkbox', checked: c.download });
    const sc = h('input', { type: 'checkbox', checked: c.scripts });
    const below = h('input', { type: 'number', min: '0', step: 'any', value: c.balance_below ? String(c.balance_below) : '', placeholder: '不提醒' });
    const hours = h('input', { type: 'number', min: '0', step: 'any', value: c.balance_hours ? String(c.balance_hours) : '', placeholder: '不提醒' });
    const spend = h('input', { type: 'number', min: '0', step: 'any', value: c.daily_spend ? String(c.daily_spend) : '', placeholder: '不提醒' });
    const evMsg = h('div', null, evNote);
    const save = h('button', { type: 'submit', class: 'primary' }, '保存');
    const testBtn = h('button', { type: 'button' }, '发测试通知');
    testBtn.addEventListener('click', () => test(testBtn, evMsg));

    mount(box,
      h('h2', null, '浏览器通知'),
      h('p', { class: 'hint' }, '面板每分钟检查一次，有事就推送到所有绑定的浏览器（Web Push）。Chrome 走 Google 的推送服务（FCM），国内网络可能收不到；Edge、Firefox 各走自己的。'),
      h('h3', null, '这个浏览器'),
      status, msg,
      h('h3', null, `已绑定的浏览器（${data.subs.length}）`),
      subsBox,
      h('h3', null, '通知哪些事'),
      h('form', {
        onsubmit: (e) => {
          e.preventDefault();
          saveConfig({
            ...data.config, instance: inst.checked, download: dl.checked, scripts: sc.checked, balance_below: Number(below.value) || 0,
            balance_hours: Number(hours.value) || 0, daily_spend: Number(spend.value) || 0,
          }, save, evMsg);
        },
      },
      h('label', { class: 'field inline' }, inst, '实例开机、运行起来、停止、离线 / 退出、销毁'),
      h('label', { class: 'field inline' }, dl, '启动下载器下完 / 失败'),
      h('label', { class: 'field inline' }, sc, '开机脚本跑完 / 失败'),
      h('div', { class: 'form-grid wide' },
        field('余额低于 $ 时提醒', below, '没恢复就每 6 小时再提醒一次'),
        field('按现在的花费撑不过几小时提醒', hours, '余额 ÷ 所有实例现在每小时的花费'),
        field('今天花费超过 $ 时提醒', spend, '和实例页“今天花费”同一个数（vast 账单，按 UTC 日，会慢几分钟到一小时）；每天最多提醒一次')),
      evMsg,
      h('div', { class: 'toolbar' }, save, testBtn)),
      h('details', null, h('summary', null, `通知记录（最近 ${data.events.length} 条）`),
        data.events.length ? table([
          { title: '时间', class: 'nowrap', cell: (e) => [fmtTime(e.ts), h('div', { class: 'hint' }, fmtAgo(e.ts))] },
          { title: '类型', class: 'nowrap', cell: (e) => KIND[e.kind] || e.kind },
          { title: '内容', cell: (e) => [h('div', { class: 'strong' }, e.title), h('div', { class: 'hint' }, e.body)] },
          { title: '送达', class: 'nowrap', cell: (e) => [`${e.sent} 个浏览器`, e.err ? h('div', { class: 'cost-bad' }, e.err) : null] },
        ], data.events) : empty('还没有通知')));
  }

  load();
  return [siteBox, box];
}
