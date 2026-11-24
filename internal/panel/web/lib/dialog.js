import { h, mount, errorBox } from './dom.js';

// openDialog shows a modal with body built by the caller. While busy, Esc
// and the close helpers do nothing, so a running request is not orphaned.
export function openDialog({ wide } = {}) {
  const body = h('div');
  const dialog = h('dialog', { class: wide ? 'dialog wide' : 'dialog' }, body);
  let busy = false;
  dialog.addEventListener('cancel', (e) => { if (busy) e.preventDefault(); });
  dialog.addEventListener('close', () => dialog.remove());
  document.body.appendChild(dialog);
  dialog.showModal();
  return {
    dialog,
    body,
    close: () => { if (!busy) dialog.close(); },
    setBusy: (b) => { busy = b; },
  };
}

// confirmDialog asks before run(). requireText, when set, must be typed
// exactly to enable the button. run's error is shown in the dialog.
export function confirmDialog({ title, message, confirmText, danger, requireText, run }) {
  const d = openDialog();
  const msg = h('div');
  const typed = requireText ? h('input', { class: 'mono', autocomplete: 'off', spellcheck: 'false' }) : null;
  const ok = h('button', { type: 'submit', class: danger ? 'danger-solid' : 'primary', disabled: Boolean(requireText) }, confirmText);
  const cancel = h('button', { type: 'button', onclick: d.close }, '取消');
  if (typed) typed.addEventListener('input', () => { ok.disabled = typed.value.trim() !== requireText; });
  mount(d.body, h('form', {
    onsubmit: async (e) => {
      e.preventDefault();
      d.setBusy(true);
      ok.disabled = true;
      cancel.disabled = true;
      mount(msg, h('p', { class: 'muted' }, '处理中…'));
      try {
        await run(typed ? typed.value.trim() : '');
        d.setBusy(false);
        d.close();
      } catch (err) {
        d.setBusy(false);
        cancel.disabled = false;
        ok.disabled = Boolean(typed) && typed.value.trim() !== requireText;
        mount(msg, errorBox(err));
      }
    },
  },
  h('h2', null, title),
  h('div', { class: 'confirm-text' }, message),
  typed ? h('label', { class: 'field' }, h('span', { class: 'field-label' }, `输入 ${requireText} 确认`), typed) : null,
  msg,
  h('div', { class: 'toolbar end' }, cancel, ok)));
  (typed || ok).focus();
  return d;
}

// copyText copies text; over plain HTTP the async clipboard API is missing,
// so it falls back to a hidden textarea.
export async function copyText(text) {
  if (navigator.clipboard && window.isSecureContext) {
    await navigator.clipboard.writeText(text);
    return;
  }
  const ta = h('textarea', { style: 'position:fixed;left:-9999px;top:0' });
  ta.value = text;
  document.body.appendChild(ta);
  ta.select();
  const ok = document.execCommand('copy');
  ta.remove();
  if (!ok) throw new Error('浏览器不让复制，请手动选中复制');
}
