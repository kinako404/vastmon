import { h, mount, field, errorBox } from '../lib/dom.js';
import { currentSession, login } from '../lib/api.js';
import { go } from '../lib/router.js';

// afterLogin is the page to return to: next when it is a panel route.
function afterLogin(next) {
  return next && next.startsWith('#/') && !next.startsWith('#/login') ? next : '#/';
}

export function loginView(ctx) {
  const back = afterLogin(ctx.query.get('next'));
  if (currentSession().logged_in) {
    go(back);
    return;
  }
  const password = h('input', { type: 'password', name: 'password', autocomplete: 'current-password', required: true });
  const msg = h('div');
  const submit = h('button', { type: 'submit', class: 'primary' }, '登录');
  const form = h('form', {
    class: 'card login',
    onsubmit: async (e) => {
      e.preventDefault();
      submit.disabled = true;
      mount(msg);
      try {
        await login(password.value);
        go(back);
      } catch (err) {
        password.value = '';
        mount(msg, errorBox(err));
        password.focus();
      } finally {
        submit.disabled = false;
      }
    },
  },
  h('h1', null, '登录 vastmon'),
  h('p', { class: 'hint' }, '面板所有内容都要登录后才能看。'),
  currentSession().password_set ? null
    : h('div', { class: 'error' }, '还没有设置密码：在服务器的 vastmon 目录运行 ./vastmon passwd'),
  field('密码', password),
  msg,
  submit);
  ctx.show(form);
  password.focus();
}
