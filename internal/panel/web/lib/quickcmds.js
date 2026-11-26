import { h, mount, errorBox, field } from './dom.js';
import { get, put } from './api.js';
import { openDialog } from './dialog.js';

// Quick commands (终端底栏的“命令”, as in FinalShell): commands in
// categories, kept on the server so every browser has the same list. A strip
// between the terminal and its status bar shows the current category; a
// click sends the command to the tab's shell (and presses Enter unless the
// command says not to), ⚙ edits it, dragging reorders it or, dropped on a
// category, moves it there. Every tab shows the same list and category.

const OPEN_KEY = 'vastmon.quickCmds';
const GROUP_KEY = 'vastmon.quickCmdsGroup';

export const quickOpen = () => localStorage.getItem(OPEN_KEY) === '1';
export function setQuickOpen(open) {
  localStorage.setItem(OPEN_KEY, open ? '1' : '0');
}

let groups = null;
let loadErr = null;
let loading = null;
let currentGroup = Number(localStorage.getItem(GROUP_KEY)) || 0;
let dragging = null;
const panels = new Set();

function renderAll() {
  for (const p of panels) p.render();
}

function load() {
  if (!loading) {
    loading = get('quickcmds').then((r) => {
      groups = r.groups;
      loadErr = null;
    }, (err) => {
      loadErr = err;
    }).finally(() => {
      loading = null;
      renderAll();
    });
  }
  return loading;
}

async function save(next) {
  groups = (await put('quickcmds', { groups: next })).groups;
  renderAll();
}

function pickGroup(id) {
  currentGroup = id;
  localStorage.setItem(GROUP_KEY, String(id));
  renderAll();
}

// resetQuickCmds forgets the loaded list, as on logout.
export function resetQuickCmds() {
  groups = null;
  loadErr = null;
}

function groupDialog(g) {
  const d = openDialog();
  const name = h('input', { value: g ? g.name : '', maxlength: '30', required: true, placeholder: '比如 ComfyUI、网络、下载' });
  const msg = h('div');
  const ok = h('button', { type: 'submit', class: 'primary' }, '保存');
  const n = g ? g.cmds.length : 0;
  const remove = g ? h('button', {
    type: 'button', class: 'danger',
    onclick: async () => {
      if (n && !window.confirm(`删除分类“${g.name}”和里面的 ${n} 条命令？`)) return;
      try {
        await save(groups.filter((x) => x.id !== g.id));
        d.close();
      } catch (err) {
        mount(msg, errorBox(err));
      }
    },
  }, '删除分类') : null;
  mount(d.body, h('form', {
    onsubmit: async (e) => {
      e.preventDefault();
      ok.disabled = true;
      try {
        const next = g
          ? groups.map((x) => (x.id === g.id ? { ...x, name: name.value } : x))
          : [...groups, { id: 0, name: name.value, cmds: [] }];
        await save(next);
        if (!g) pickGroup(groups[groups.length - 1].id);
        d.close();
      } catch (err) {
        ok.disabled = false;
        mount(msg, errorBox(err));
      }
    },
  },
  h('h2', null, g ? `编辑分类 - ${g.name}` : '新建分类'),
  field('名称', name),
  msg,
  h('div', { class: 'toolbar end' }, remove, h('span', { class: 'spacer' }),
    h('button', { type: 'button', onclick: d.close }, '取消'), ok)));
  name.focus();
}

function cmdDialog(g, c) {
  const d = openDialog({ wide: true });
  const name = h('input', { value: c ? c.name : '', maxlength: '40', required: true, placeholder: '按钮上显示的名字' });
  const cmd = h('textarea', { class: 'mono qc-text', rows: '7', spellcheck: 'false', required: true, placeholder: '比如 tail -n 200 -f /var/log/portal/comfyui.log' });
  cmd.value = c ? c.cmd : '';
  const group = h('select', null, groups.map((x) => h('option', { value: String(x.id), selected: x.id === g.id }, x.name)));
  const enter = h('input', { type: 'checkbox', checked: c ? c.enter : true });
  const msg = h('div');
  const ok = h('button', { type: 'submit', class: 'primary' }, '保存');
  const remove = c ? h('button', {
    type: 'button', class: 'danger',
    onclick: async () => {
      if (!window.confirm(`删除命令“${c.name}”？`)) return;
      try {
        await save(groups.map((x) => ({ ...x, cmds: x.cmds.filter((y) => y.id !== c.id) })));
        d.close();
      } catch (err) {
        mount(msg, errorBox(err));
      }
    },
  }, '删除') : null;
  const form = h('form', {
    onsubmit: async (e) => {
      e.preventDefault();
      ok.disabled = true;
      const item = { id: c ? c.id : 0, name: name.value, cmd: cmd.value, enter: enter.checked };
      const to = Number(group.value);
      const next = groups.map((x) => {
        const at = c ? x.cmds.findIndex((y) => y.id === c.id) : -1;
        if (x.id === to && at >= 0) return { ...x, cmds: x.cmds.map((y) => (y.id === c.id ? item : y)) };
        const rest = at >= 0 ? x.cmds.filter((y) => y.id !== c.id) : x.cmds;
        return x.id === to ? { ...x, cmds: [...rest, item] } : { ...x, cmds: rest };
      });
      try {
        await save(next);
        d.close();
      } catch (err) {
        ok.disabled = false;
        mount(msg, errorBox(err));
      }
    },
  },
  h('h2', null, c ? `编辑命令 - ${c.name}` : '新建命令'),
  field('名称', name),
  field('命令', cmd, '多行也可以，整段一次粘贴进终端。Ctrl+Enter 保存。'),
  field('分类', group),
  h('label', { class: 'field inline' }, enter, '点击后直接执行（发完再按回车）；不勾就只填进命令行，自己改了再回车'),
  msg,
  h('div', { class: 'toolbar end' }, remove, h('span', { class: 'spacer' }),
    h('button', { type: 'button', onclick: d.close }, '取消'), ok));
  cmd.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) {
      e.preventDefault();
      form.requestSubmit();
    }
  });
  mount(d.body, form);
  (c ? cmd : name).focus();
}

// move puts command cid before command beforeId in group gid (at the end
// when beforeId is 0), taking it out of whichever group holds it.
function move(cid, gid, beforeId) {
  let item = null;
  const rest = groups.map((x) => ({
    ...x,
    cmds: x.cmds.filter((y) => {
      if (y.id !== cid) return true;
      item = y;
      return false;
    }),
  }));
  if (!item) return null;
  return rest.map((x) => {
    if (x.id !== gid) return x;
    const at = beforeId ? x.cmds.findIndex((y) => y.id === beforeId) : -1;
    const cmds = [...x.cmds];
    cmds.splice(at >= 0 ? at : cmds.length, 0, item);
    return { ...x, cmds };
  });
}

// createQuickPanel makes one tab's strip. run(cmd) sends a command to the
// tab's shell and returns false when it is not connected.
export function createQuickPanel(run) {
  const el = h('div', { class: 'qc' });
  const note = h('span', { class: 'qc-note' });
  let noteTimer = 0;
  const say = (text, bad) => {
    note.className = bad ? 'qc-note bad' : 'qc-note';
    note.textContent = text;
    clearTimeout(noteTimer);
    noteTimer = setTimeout(() => { note.textContent = ''; }, 4000);
  };
  const drop = async (next) => {
    if (!next) return;
    try {
      await save(next);
    } catch (err) {
      say(`没保存上：${err.message}`, true);
    }
  };
  const dropTarget = (node, onDrop) => {
    node.addEventListener('dragover', (e) => {
      if (!dragging) return;
      e.preventDefault();
      node.classList.add('drop');
    });
    node.addEventListener('dragleave', () => node.classList.remove('drop'));
    node.addEventListener('drop', (e) => {
      e.preventDefault();
      node.classList.remove('drop');
      if (dragging) onDrop(dragging);
    });
  };

  function chip(g, c) {
    const node = h('span', {
      class: 'qc-cmd', draggable: 'true',
      ondragstart: (e) => {
        dragging = c.id;
        e.dataTransfer.effectAllowed = 'move';
        e.dataTransfer.setData('text/plain', c.cmd);
        node.classList.add('dragging');
      },
      ondragend: () => {
        dragging = null;
        node.classList.remove('dragging');
      },
    },
    h('button', {
      type: 'button', class: 'qc-run',
      title: `${c.cmd}\n\n${c.enter ? '点击执行' : '点击填进命令行（不执行）'}`,
      onclick: () => {
        if (!run(c)) say('终端没连上，在终端里按回车重连后再试', true);
      },
    }, c.name),
    h('button', { type: 'button', class: 'qc-edit', title: '编辑', onclick: () => cmdDialog(g, c) }, '⚙'));
    dropTarget(node, (cid) => {
      if (cid !== c.id) drop(move(cid, g.id, c.id));
    });
    return node;
  }

  function render() {
    if (el.hidden && !groups) return;
    if (!groups) {
      mount(el, h('div', { class: 'qc-groups' }, loadErr
        ? [h('span', { class: 'qc-note bad' }, `读取快捷命令失败：${loadErr.message}`),
          h('button', { type: 'button', class: 'qc-add', onclick: () => { loadErr = null; render(); } }, '重试')]
        : h('span', { class: 'qc-note' }, '加载中…')));
      if (!loadErr) load();
      return;
    }
    const g = groups.find((x) => x.id === currentGroup) || groups[0];
    const tabs = groups.map((x) => {
      const node = h('button', {
        type: 'button', class: x === g ? 'qc-group on' : 'qc-group',
        title: x === g ? '双击改名 / 删除分类；把命令拖到这里移进这个分类' : '把命令拖到这里移进这个分类',
        onclick: () => pickGroup(x.id),
        ondblclick: () => groupDialog(x),
      }, `📁 ${x.name}`);
      dropTarget(node, (cid) => drop(move(cid, x.id, 0)));
      return node;
    });
    const add = g ? h('button', { type: 'button', class: 'qc-add', onclick: () => cmdDialog(g, null) }, '+ 命令') : null;
    if (add) dropTarget(add, (cid) => drop(move(cid, g.id, 0)));
    mount(el,
      h('div', { class: 'qc-groups' }, tabs,
        h('button', { type: 'button', class: 'qc-add', title: '新建分类', onclick: () => groupDialog(null) }, '+ 分类'),
        h('span', { class: 'spacer' }), note,
        h('span', { class: 'qc-hint' }, '点击发到终端 · ⚙ 编辑 · 拖动排序')),
      h('div', { class: 'qc-cmds' },
        g ? g.cmds.map((c) => chip(g, c)) : h('span', { class: 'qc-hint' }, '还没有分类，先点“+ 分类”'),
        add));
  }

  const panel = {
    el,
    render,
    dispose: () => {
      clearTimeout(noteTimer);
      panels.delete(panel);
    },
  };
  panels.add(panel);
  return panel;
}
