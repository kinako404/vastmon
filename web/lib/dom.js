// DOM helpers (from the ConLoom panel). Strings passed as children always
// become text nodes, so data from the server is never parsed as HTML.

const PROPS = new Set(['value', 'checked', 'disabled', 'selected', 'multiple', 'open']);

function apply(el, attrs) {
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k.startsWith('on') && typeof v === 'function') {
      el.addEventListener(k.slice(2).toLowerCase(), v);
    } else if (PROPS.has(k)) {
      el[k] = v;
    } else if (k === 'dataset') {
      Object.assign(el.dataset, v);
    } else {
      el.setAttribute(k, v === true ? '' : String(v));
    }
  }
}

function append(el, children) {
  for (const c of children) {
    if (c === undefined || c === null || c === false || c === true) continue;
    if (Array.isArray(c)) append(el, c);
    else if (c instanceof Node) el.appendChild(c);
    else el.appendChild(document.createTextNode(String(c)));
  }
}

// h creates an HTML element. attrs: on* functions become event listeners,
// value/checked/disabled/selected/multiple/open are set as properties, other
// keys as attributes (true = present, false/null/undefined = absent).
export function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  apply(el, attrs);
  append(el, children);
  return el;
}

// mount replaces el's children.
export function mount(el, ...children) {
  el.replaceChildren();
  append(el, children);
  return el;
}

export function errorBox(err) {
  return h('div', { class: 'error', role: 'alert' }, err && err.message ? err.message : String(err));
}

export function loading() {
  return h('p', { class: 'muted' }, '加载中…');
}

export function empty(text) {
  return h('p', { class: 'muted' }, text || '暂无数据');
}

// table builds a table from column specs {title, cell(row) => node|string,
// class, head(title) => node}; rowClass(row) gives a row its class.
export function table(columns, rows, rowClass) {
  return h('div', { class: 'table-wrap' },
    h('table', null,
      h('thead', null, h('tr', null, columns.map((c) => h('th', { class: c.class }, c.head ? c.head(c.title) : c.title)))),
      h('tbody', null, rows.map((r) => h('tr', { class: rowClass ? rowClass(r) : null },
        columns.map((c) => h('td', { class: c.class }, c.cell(r))))))));
}

// kv builds a definition list from [label, value] pairs.
export function kv(pairs) {
  return h('dl', { class: 'kv' }, pairs.filter(Boolean).map(([k, v]) => [h('dt', null, k), h('dd', null, v)]));
}

// field wraps a labelled form control.
export function field(label, control, hint) {
  return h('label', { class: 'field' }, h('span', { class: 'field-label' }, label), control,
    hint ? h('span', { class: 'hint' }, hint) : null);
}
