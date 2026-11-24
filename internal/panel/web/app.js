import { h, mount } from './lib/dom.js';
import { route, start, render, go } from './lib/router.js';
import { currentSession, loadSession, logout, onSession, loginHash } from './lib/api.js';
import { machinesView } from './views/machines.js';
import { machineView } from './views/machine.js';
import { pollsView } from './views/polls.js';
import { instancesView } from './views/instances.js';
import { loginView } from './views/login.js';
import { favoritesView } from './views/favorites.js';
import { blacklistView } from './views/blacklist.js';
import { settingsView } from './views/settings.js';
import { terminalView, closeAllTerminals } from './views/terminal.js';
import { editorView } from './views/editor.js';

const SECTIONS = [
  ['', '机器'],
  ['instances', '实例'],
  ['terminal', '终端'],
  ['favorites', '收藏'],
  ['blacklist', '黑名单'],
  ['polls', '抓取记录'],
  ['settings', '设置'],
];

// Every page but the login page needs a login; without one it goes to the
// login page and comes back afterwards.
const guarded = (view) => (ctx) => {
  if (currentSession().logged_in) return view(ctx);
  go(loginHash(location.hash || '#/'));
  return undefined;
};

route('/', guarded(machinesView));
route('/machines/:mid/:n', guarded(machineView));
route('/instances', guarded(instancesView));
route('/polls', guarded(pollsView));
route('/favorites', guarded(favoritesView));
route('/blacklist', guarded(blacklistView));
route('/settings', guarded(settingsView));
route('/terminal', guarded(terminalView));
route('/edit/:id', guarded(editorView));
route('/login', loginView);

const links = SECTIONS.map(([section, title]) => {
  const a = h('a', { href: '#/' + section }, title);
  a.dataset.section = section;
  return a;
});
mount(document.getElementById('nav'), links);
const main = document.getElementById('main');
const who = document.getElementById('who');

// renderWho shows the login state at the right of the top bar.
function renderWho() {
  document.getElementById('nav').hidden = !currentSession().logged_in;
  if (currentSession().logged_in) {
    mount(who, h('span', { class: 'badge on' }, '已登录'), h('button', {
      type: 'button',
      class: 'link',
      onclick: async () => {
        try {
          await logout();
        } catch {
          // the session ends either way
        }
      },
    }, '退出'));
  } else {
    mount(who);
  }
}

// beforeRender marks the section on show; machine details belong to 机器.
function beforeRender(loc) {
  const first = loc.segments[0] || '';
  const section = first === 'machines' ? '' : first;
  for (const a of links) {
    const on = a.dataset.section === section;
    a.classList.toggle('active', on);
    if (on) a.setAttribute('aria-current', 'page');
    else a.removeAttribute('aria-current');
  }
  renderWho();
  document.title = 'vastmon';
}

function notFound(ctx) {
  ctx.show(h('h1', null, '页面不存在'), h('p', null, h('a', { href: '#/' }, '返回机器列表')));
}

// A logout (or an expired session) goes to the login page; a login redraws.
let started = false;

onSession((s) => {
  if (!started) return;
  const hash = location.hash || '';
  if (hash.startsWith('#/login')) return;
  if (!s.logged_in) {
    closeAllTerminals();
    go(loginHash(hash || '#/'));
  } else render();
});

async function boot() {
  try {
    await loadSession();
  } catch {
    // the login page shows the problem
  }
  started = true;
  start(main, notFound, beforeRender);
}

boot();
