// Client for the JSON API under /api/. Errors come back as
// {"error": {"code", "message"}}. Every request carries X-Requested-With,
// which the server requires on writes so other sites cannot post forms here.

const BASE = 'api/';

export class ApiError extends Error {
  constructor(status, code, message) {
    super(message);
    this.status = status;
    this.code = code;
  }
}

export function isAbort(err) {
  return err && err.name === 'AbortError';
}

let session = { logged_in: false, password_set: false };
const listeners = new Set();

export function currentSession() {
  return session;
}

// onSession calls fn whenever the login state changes.
export function onSession(fn) {
  listeners.add(fn);
}

function setSession(next) {
  const changed = next.logged_in !== session.logged_in;
  session = next;
  if (changed) for (const fn of listeners) fn(session);
}

async function request(method, path, { body, signal } = {}) {
  const headers = { Accept: 'application/json', 'X-Requested-With': 'vastmon' };
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  let res;
  try {
    res = await fetch(BASE + path, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
      credentials: 'same-origin',
      cache: 'no-store',
      signal,
    });
  } catch (err) {
    if (isAbort(err)) throw err;
    throw new ApiError(0, 'NETWORK', '无法连接服务器：' + err.message);
  }
  const text = await res.text();
  let data = null;
  if (text) {
    try {
      data = JSON.parse(text);
    } catch {
      data = null;
    }
  }
  if (!res.ok) {
    const e = (data && data.error) || {};
    if (res.status === 401 && e.code === 'UNAUTHORIZED') setSession({ ...session, logged_in: false });
    throw new ApiError(res.status, e.code || '', e.message || `请求失败（HTTP ${res.status}）`);
  }
  return data;
}

export const get = (path, signal) => request('GET', path, { signal });
export const post = (path, body, signal) => request('POST', path, { body, signal });
export const put = (path, body, signal) => request('PUT', path, { body, signal });
export const del = (path, signal) => request('DELETE', path, { signal });

export async function loadSession() {
  setSession(await get('session'));
  return session;
}

export async function login(password) {
  setSession(await post('login', { password }));
}

export async function logout() {
  setSession(await post('logout'));
}

// loginHash is the login route that returns to next afterwards.
export function loginHash(next) {
  if (!next || next.startsWith('#/login')) return '#/login';
  return '#/login?next=' + encodeURIComponent(next);
}
