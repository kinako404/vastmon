import { h, mount, table, empty, errorBox, loading, field } from '../lib/dom.js';
import { get, post, put, currentSession, loginHash, isAbort } from '../lib/api.js';
import { fmtTime, fmtAgo, fixed, shortCPU, cores, costClass } from '../lib/format.js';
import { fmtGeo } from '../lib/geo.js';
import { openDialog, confirmDialog, copyText } from '../lib/dialog.js';
import { marksFrom, emptyMarks, markButtons, openMarkDialog } from '../lib/marks.js';
import { summaryLine, report, powerBadge, asBaseline } from '../lib/bench.js';
import { jobPanel, latestJobs, isActive, openDownloadDialog, openURLDialog } from '../lib/downloads.js';
import { inputJobPanel, inputJobTitle, latestInputJobs, inputActive, createRateMeter } from '../lib/inputjob.js';
import { scriptJobPanel, latestScriptJobs, isActive as scriptActive, openScriptsDialog } from '../lib/scripts.js';

const POLL_MS = 20000;
const DL_POLL_MS = 2000;
// PORTAL_PORT is the container port of vast's Instance Portal (the template's
// OPEN_BUTTON_PORT).
const PORTAL_PORT = '1111/tcp';

const STATUS = {
  running: ['运行中', 'on'],
  loading: ['启动中', 'warn'],
  created: ['已创建', 'warn'],
  scheduling: ['排队中', 'warn'],
  rebooting: ['重启中', 'warn'],
  recycling: ['重建中', 'warn'],
  stopped: ['已停止', ''],
  exited: ['已退出', 'bad'],
  offline: ['离线', 'bad'],
};

function statusBadge(x) {
  const s = x.actual_status || x.cur_state || 'unknown';
  const [text, cls] = STATUS[s] || [s, ''];
  return h('span', { class: 'badge ' + cls }, text);
}

function fmtSpan(sec) {
  if (!Number.isFinite(sec) || sec < 0) return '—';
  const d = Math.floor(sec / 86400);
  const hh = Math.floor((sec % 86400) / 3600);
  const m = Math.floor((sec % 3600) / 60);
  if (d > 0) return `${d}天${hh}小时`;
  if (hh > 0) return `${hh}小时${m}分`;
  return `${m}分${Math.floor(sec % 60)}秒`;
}

// meter is a small usage bar; frac outside [0, 1] or missing shows empty.
function meter(frac) {
  const f = Number.isFinite(frac) ? Math.max(0, Math.min(1, frac)) : 0;
  return h('span', { class: 'meter' }, h('span', { class: f > 0.85 ? 'meter-fill hot' : 'meter-fill', style: `width:${(f * 100).toFixed(1)}%` }));
}

const num = (v) => (typeof v === 'number' && Number.isFinite(v) && v >= 0 ? v : null);

// hourly is what the instance costs now: vast bills only disk while it is
// loading or stopped, and instance.totalHour follows that.
function hourly(x) {
  const inst = x.instance || {};
  return num(inst.totalHour) ?? x.dph_total;
}

function hostPort(x, containerPort) {
  const p = x.ports && x.ports[containerPort];
  return Array.isArray(p) && p.length ? p[0].HostPort : '';
}

function portalURL(x) {
  const port = hostPort(x, PORTAL_PORT);
  if (!port || !x.public_ipaddr) return '';
  const token = x.jupyter_token ? `?token=${encodeURIComponent(x.jupyter_token)}` : '';
  return `http://${x.public_ipaddr}:${port}/${token}`;
}

function sshCommand(x) {
  return x.ssh_host && x.ssh_port ? `ssh -p ${x.ssh_port} root@${x.ssh_host}` : '';
}

// GB pulls the traffic volume out of a charge description such as
// "7.0 GB Downloaded at $0.003/GB".
function chargeGB(item) {
  const m = item && item.desc ? item.desc.match(/([\d.]+)\s*GB/) : null;
  return m ? Number(m[1]) : null;
}

function money(v, d = 3) {
  return Number.isFinite(v) ? '$' + fixed(v, d) : '—';
}

function spendStat(k, v, sub, extra, cls) {
  return h('div', { class: cls ? `stat ${cls}` : 'stat' }, extra, h('div', { class: 'k' }, k), h('div', { class: 'v' }, v), sub ? h('div', { class: 'sub' }, sub) : null);
}

// spendCol is an instance's billed cost so far, split like vast's bill.
function spendCol(sp, x) {
  const it = (sp && sp.items) || {};
  const gb = (t) => { const g = chargeGB(it[t]); return g === null ? '' : `${fixed(g, 1)}GB `; };
  const rate = x.actual_status === 'running' ? x.dph_total : hourly(x);
  return col('费用（vast 已记账）',
    h('span', { class: 'strong' }, sp ? money(sp.amount) : '还没出账'),
    sp ? `GPU ${money((it.gpu || {}).amount)} · 磁盘 ${money((it.disk || {}).amount)}` : null,
    sp ? `下行 ${gb('bwd')}${money((it.bwd || {}).amount)}` : null,
    sp ? `上行 ${gb('bwu')}${money((it.bwu || {}).amount)}` : null,
    h('span', { class: 'hint' }, `现价 ${money(rate)}/h ≈ ${money(rate * 24, 2)}/天`));
}

function col(title, ...rows) {
  return h('div', { class: 'inst-col' }, h('div', { class: 'inst-col-title' }, title), rows.map((r) => h('div', null, r)));
}

// portList shows the port mappings; clicking one copies its public
// IP:port.
function portList(x, copy) {
  const entries = Object.entries(x.ports || {})
    .map(([k, v]) => [k, Array.isArray(v) && v.length ? v[0].HostPort : ''])
    .filter(([, v]) => v)
    .sort((a, b) => parseInt(a[0], 10) - parseInt(b[0], 10));
  if (!entries.length) return null;
  return h('div', { class: 'inst-ports' }, h('span', { class: 'hint' }, '端口映射：'),
    entries.map(([k, v]) => {
      const addr = `${x.public_ipaddr}:${v}`;
      return h('button', {
        type: 'button', class: 'port mono',
        title: `容器 ${k} → 公网 ${addr}\n点击复制 ${addr}`,
        onclick: (e) => copy(e.currentTarget, addr),
      }, `${parseInt(k, 10)}→${v}`);
    }));
}

export async function instancesView(ctx) {
  const { signal } = ctx;
  if (!currentSession().logged_in) {
    ctx.show(h('h1', null, '实例'),
      h('p', null, '查看实例需要先', h('a', { href: loginHash('#/instances') }, '登录'), '。'));
    return;
  }
  let data = null;
  let bill = null;
  let marks = emptyMarks();
  let benches = [];
  let baselines = [];
  let setups = new Map();
  // download jobs refresh every 2 s while one is active; each instance keeps
  // one box so a refresh only redraws the progress, not the whole card.
  let jobs = new Map();
  const dlBoxes = new Map();
  const dlFolds = new Set();
  let dlTimer = 0;
  let scJobs = new Map();
  const scBoxes = new Map();
  const scFolds = new Set();
  const scOpen = new Set();
  // input backup jobs (maybe followed by stop / recycle / destroy)
  let inJobs = new Map();
  let inClock = { now: 0, askMin: 3 };
  const inBoxes = new Map();
  const inRate = createRateMeter();
  const inputNote = h('div');
  const onMarks = (res) => { marks = marksFrom(res); render(); };
  let billErr = null;
  let tab = '';
  const collapsed = new Set();
  const message = h('div');
  const spendBox = h('div');
  const tabsBox = h('div', { class: 'inst-tabs' });
  const cardsBox = h('div', null, loading());
  const historyBox = h('div');
  const note = h('p', { class: 'hint' });

  let timer = 0;
  async function refresh() {
    clearTimeout(timer);
    try {
      const [d, b, fv, bm, bl, dj, sj, ij] = await Promise.all([
        get('instances', signal),
        get('billing', signal).then((v) => [v, null], (e) => (isAbort(e) ? Promise.reject(e) : [null, e])),
        get('blacklist', signal).catch((e) => (isAbort(e) ? Promise.reject(e) : null)),
        get('benchmarks', signal).catch((e) => (isAbort(e) ? Promise.reject(e) : null)),
        get('baselines', signal).catch((e) => (isAbort(e) ? Promise.reject(e) : null)),
        get('downloads/jobs', signal).catch((e) => (isAbort(e) ? Promise.reject(e) : null)),
        get('scripts/jobs', signal).catch((e) => (isAbort(e) ? Promise.reject(e) : null)),
        get('input/jobs', signal).catch((e) => (isAbort(e) ? Promise.reject(e) : null)),
      ]);
      if (dj) jobs = latestJobs(dj.jobs);
      if (sj) scJobs = latestScriptJobs(sj.jobs);
      if (ij) setInputJobs(ij);
      if (fv) marks = marksFrom(fv);
      if (bm) benches = bm.benchmarks;
      if (bl) baselines = bl.baselines;
      setups = new Map((d.setups || []).map((x) => [x.instance_id, x]));
      data = d;
      [bill, billErr] = b[0] ? [b[0], null] : [bill, b[1]];
      mount(message, data.error ? errorBox({ message: data.error }) : null);
      render();
      mount(note, `每 ${POLL_MS / 1000} 秒自动刷新，更新于 ${fmtTime(Math.round(Date.now() / 1000))}`);
    } catch (err) {
      if (!isAbort(err)) mount(message, errorBox(err));
    }
    if (!signal.aborted) timer = setTimeout(refresh, POLL_MS);
  }
  signal.addEventListener('abort', () => { clearTimeout(timer); clearTimeout(dlTimer); });

  function scBox(id) {
    if (!scBoxes.has(id)) scBoxes.set(id, h('div', { class: 'inst-dl' }));
    return scBoxes.get(id);
  }

  async function cancelScripts(j) {
    if (!window.confirm(`取消实例 ${j.instance_id} 正在跑的开机脚本？`)) return;
    try {
      await post(`scripts/jobs/${j.id}/cancel`, {});
    } catch (err) {
      mount(message, errorBox(err));
    }
    pollDownloads();
  }

  function dlBox(id) {
    if (!dlBoxes.has(id)) dlBoxes.set(id, h('div', { class: 'inst-dl' }));
    return dlBoxes.get(id);
  }

  function inBox(id) {
    if (!inBoxes.has(id)) inBoxes.set(id, h('div', { class: 'inst-dl' }));
    return inBoxes.get(id);
  }

  // setInputJobs takes a fresh job list. A job that has just finished is
  // announced above the cards, where it stays until dismissed: after a
  // destroy the instance's own card is gone.
  function setInputJobs(res) {
    const was = inJobs;
    inJobs = latestInputJobs(res.jobs);
    inClock = { now: res.now, askMin: res.ask_min };
    for (const j of inJobs.values()) {
      const old = was.get(j.instance_id);
      if (!old || old.id !== j.id || !inputActive(old) || inputActive(j)) continue;
      const close = h('button', { type: 'button', class: 'link small', onclick: () => mount(inputNote) }, '知道了');
      const text = `实例 ${j.instance_id} · ${inputJobTitle(j)}：${[j.err, j.text].filter(Boolean).join('，')}`;
      mount(inputNote, h('div', { class: j.status === 'failed' ? 'error' : 'notice' }, text, ' ', close));
      refreshSoon();
    }
  }

  async function decideInput(j, decision) {
    const name = j.action_name;
    if (decision === 'skip' && !window.confirm(`不备份 ComfyUI input，直接${name}实例 ${j.instance_id}？\n实例上新增的图片 / 视频不会进备份${j.action === 'stop' ? '' : '，之后就拿不回来了'}。`)) return;
    if (decision === 'cancel' && !window.confirm(j.action ? `取消${name}实例 ${j.instance_id}？备份也会停下，实例保持原样。` : `放弃备份实例 ${j.instance_id} 的 ComfyUI input？`)) return;
    try {
      await post(`input/jobs/${j.id}/decide`, { decision });
    } catch (err) {
      mount(message, errorBox(err));
    }
    pollDownloads();
  }

  async function cancelDownload(j) {
    if (!window.confirm(`取消实例 ${j.instance_id} 的下载？下了一半的文件留在实例上，下次启动下载器会续传。`)) return;
    try {
      await post(`downloads/jobs/${j.id}/cancel`, {});
    } catch (err) {
      mount(message, errorBox(err));
    }
    pollDownloads();
  }

  function renderDownloads() {
    for (const [id, el] of dlBoxes) {
      const j = jobs.get(id);
      mount(el, j ? jobPanel(j, { onCancel: cancelDownload, folds: dlFolds }) : null);
    }
    const now = Math.round(Date.now() / 1000);
    for (const [id, el] of scBoxes) {
      const j = scJobs.get(id);
      mount(el, j ? scriptJobPanel(j, { onCancel: cancelScripts, folds: scFolds, open: scOpen, now }) : null);
    }
    for (const [id, el] of inBoxes) {
      const j = inJobs.get(id);
      mount(el, j ? inputJobPanel(j, { now: inClock.now, rate: inRate(j), askMin: inClock.askMin, onDecide: decideInput }) : null);
    }
    clearTimeout(dlTimer);
    const busy = [...jobs.values()].some(isActive) || [...scJobs.values()].some(scriptActive) || [...inJobs.values()].some(inputActive);
    if (busy && !signal.aborted) dlTimer = setTimeout(pollDownloads, DL_POLL_MS);
  }

  async function pollDownloads() {
    clearTimeout(dlTimer);
    try {
      const was = new Map([...jobs].map(([id, j]) => [id, j.status]));
      const [dj, sj, ij] = await Promise.all([get('downloads/jobs', signal), get('scripts/jobs', signal), get('input/jobs', signal)]);
      jobs = latestJobs(dj.jobs);
      scJobs = latestScriptJobs(sj.jobs);
      setInputJobs(ij);
      // a job that just finished may have changed the disk usage: refresh cards
      if ([...jobs].some(([id, j]) => was.has(id) && was.get(id) !== j.status && !isActive(j))) refreshSoon();
    } catch (err) {
      if (isAbort(err)) return;
    }
    renderDownloads();
  }

  // backupInput starts the backup job; its progress shows on the card.
  async function backupInput(x, btn) {
    btn.disabled = true;
    try {
      await post(`instances/${x.id}/backup-input`, {});
      mount(inputNote);
      pollDownloads();
    } catch (err) {
      mount(message, errorBox(err));
    } finally {
      btn.disabled = false;
    }
  }
  // after an action, look again soon: vast changes state within seconds.
  const refreshSoon = () => {
    refresh();
    setTimeout(() => { if (!signal.aborted) refresh(); }, 4000);
  };

  // act runs an action. With backup_input the server only starts a job
  // (backup first, then the action) and the card shows how it is going.
  async function act(x, action, extra) {
    const res = await post(`instances/${x.id}/action`, { action, ...extra });
    if (res.job) {
      mount(inputNote);
      pollDownloads();
    } else {
      refreshSoon();
    }
  }

  function confirmAction(x, action) {
    const ssh = sshCommand(x);
    const price = `$${fixed(x.dph_total, 3)}/h`;
    const disk = `$${fixed(x.storage_total_cost, 3)}/h`;
    const specs = {
      stop: {
        title: `停止实例 ${x.id}？`, confirmText: '停止',
        message: [h('p', null, `停止后 GPU 不再计费，但磁盘照常收费（${disk}）。`),
          h('p', null, '这张卡可能被别人租走，之后再启动可能要排队或起不来。容器里的数据保留。')],
      },
      start: {
        title: `启动实例 ${x.id}？`, confirmText: '启动',
        message: h('p', null, `启动后按 ${price} 计费。如果这张卡被别人占用，会一直排队到空出来。`),
      },
      reboot: {
        title: `重启实例 ${x.id}？`, confirmText: '重启',
        message: h('p', null, '停止再启动容器，GPU 不会丢，容器里的数据保留。运行中的程序会被中断。'),
      },
      recycle: {
        title: `重建实例 ${x.id}？`, confirmText: '重建', danger: true,
        message: [h('p', null, '删掉容器，重新拉取模板镜像再创建，GPU 不会丢。'),
          h('p', { class: 'cost-bad' }, '容器里的所有文件（包括下好的模型、装的节点）都会丢失。')],
      },
      destroy: {
        title: `销毁实例 ${x.id}？`, confirmText: '永久销毁', danger: true, requireText: String(x.id),
        message: [h('p', { class: 'cost-bad' }, '永久删除这个实例和它的所有数据，不能恢复。'),
          h('p', null, `${x.num_gpus}× ${x.gpu_name} · ${fmtGeo(x.geolocation)}${x.label ? ' · ' + x.label : ''}${ssh ? ' · ' + ssh : ''}`)],
      },
    };
    const spec = specs[action];
    const canBackup = x.actual_status === 'running' && ['stop', 'recycle', 'destroy'].includes(action);
    const backup = h('input', { type: 'checkbox', checked: true });
    if (canBackup) {
      spec.message = [spec.message, h('label', { class: 'field inline backup-opt' }, backup,
        '先备份 ComfyUI 的 input 目录（在 ComfyUI 里上传的图片 / 视频）（推荐）'),
      h('p', { class: 'hint tight' }, '拉回面板机合并进 backups/input.tar.gz：只增加 / 更新文件，不会删本地已有的。备份在后台进行，进度显示在实例卡片上，备份成功后才执行；拉得太慢时可以在卡片上选继续等、放弃备份直接执行，或者取消。实例上没有 ComfyUI（没有 input 目录）或者不想备份时取消勾选。')];
    }
    confirmDialog({
      ...spec,
      run: (typed) => act(x, action, {
        ...(action === 'destroy' ? { confirm: typed } : {}),
        ...(canBackup && backup.checked ? { backup_input: true } : {}),
      }),
    });
  }

  function labelDialog(x) {
    const d = openDialog();
    const input = h('input', { value: x.label || '', maxlength: '128' });
    const msg = h('div');
    const ok = h('button', { type: 'submit', class: 'primary' }, '保存');
    mount(d.body, h('form', {
      onsubmit: async (e) => {
        e.preventDefault();
        d.setBusy(true);
        ok.disabled = true;
        try {
          await put(`instances/${x.id}/label`, { label: input.value.trim() });
          d.setBusy(false);
          d.close();
          refreshSoon();
        } catch (err) {
          d.setBusy(false);
          ok.disabled = false;
          mount(msg, errorBox(err));
        }
      },
    }, h('h2', null, `实例 ${x.id} 改名`), field('名称', input, '留空即清除名称；同名的实例会归到一个标签页'), msg,
    h('div', { class: 'toolbar end' }, h('button', { type: 'button', onclick: d.close }, '取消'), ok)));
    input.focus();
    input.select();
  }

  function logsDialog(x) {
    const d = openDialog({ wide: true });
    d.dialog.classList.add('logs');
    const tail = h('select', null, ['200', '1000', '5000', '20000'].map((n) => h('option', { value: n, selected: n === '1000' }, `最后 ${n} 行`)));
    const filter = h('input', { placeholder: '过滤（grep）', spellcheck: 'false' });
    const daemon = h('input', { type: 'checkbox' });
    const out = h('pre', { class: 'code logs-out' });
    const msg = h('div');
    const link = h('span');
    const load = h('button', { type: 'submit', class: 'primary' }, '读取');
    async function fetchLogs() {
      d.setBusy(true);
      load.disabled = true;
      mount(msg, h('p', { class: 'muted' }, '正在向 vast 请求日志，可能要十几秒…'));
      try {
        const r = await post(`instances/${x.id}/logs`, { tail: Number(tail.value), filter: filter.value.trim(), daemon: daemon.checked });
        out.textContent = r.text || '（空）';
        out.scrollTop = out.scrollHeight;
        mount(msg);
        mount(link, h('a', { href: r.url, target: '_blank', rel: 'noopener noreferrer' }, '原始日志文件'));
      } catch (err) {
        mount(msg, errorBox(err));
      } finally {
        d.setBusy(false);
        load.disabled = false;
      }
    }
    mount(d.body, h('form', { onsubmit: (e) => { e.preventDefault(); fetchLogs(); } },
      h('h2', null, `实例 ${x.id} 日志`),
      h('div', { class: 'toolbar' }, tail, filter, h('label', { class: 'field inline' }, daemon, '看 vast 守护进程日志'), load,
        h('span', { class: 'spacer' }), link),
      msg, out,
      h('div', { class: 'toolbar end' }, h('button', { type: 'button', onclick: d.close }, '关闭'))));
    fetchLogs();
  }

  async function sshDialog(x) {
    const d = openDialog({ wide: true });
    const msg = h('div');
    const list = h('div', null, loading());
    const paste = h('textarea', { class: 'mono', rows: '3', placeholder: 'ssh-ed25519 AAAA… user@host', spellcheck: 'false' });
    async function attach(key, btn) {
      d.setBusy(true);
      btn.disabled = true;
      mount(msg, h('p', { class: 'muted' }, '正在添加…'));
      try {
        await post(`instances/${x.id}/ssh`, { ssh_key: key });
        mount(msg, h('div', { class: 'notice' }, '已添加。实例运行中时几秒内生效。'));
        refreshSoon();
      } catch (err) {
        mount(msg, errorBox(err));
      } finally {
        d.setBusy(false);
        btn.disabled = false;
      }
    }
    const addBtn = h('button', { type: 'button', class: 'primary', onclick: (e) => attach(paste.value.trim(), e.currentTarget) }, '添加这个公钥');
    const ssh = sshCommand(x);
    mount(d.body,
      h('h2', null, `实例 ${x.id} 的 SSH 密钥`),
      ssh ? h('p', null, '连接命令：', h('code', { class: 'mono' }, ssh)) : null,
      h('h3', null, '账户里的密钥'),
      h('p', { class: 'hint' }, '开机时 vast 会自动放进账户里的密钥；后来新加到账户的，可以在这里补到这台实例上。'),
      list,
      h('h3', null, '粘贴公钥'),
      paste,
      h('div', { class: 'toolbar' }, addBtn),
      msg,
      h('div', { class: 'toolbar end' }, h('button', { type: 'button', onclick: d.close }, '关闭')));
    try {
      const r = await get('ssh-keys');
      mount(list, r.keys.length ? h('ul', { class: 'plain key-list' }, r.keys.map((k) => {
        const parts = (k.public_key || '').trim().split(/\s+/);
        const short = `${parts[0] || ''} …${(parts[1] || '').slice(-16)}${parts[2] ? ' ' + parts.slice(2).join(' ') : ''}`;
        return h('li', null, h('span', { class: 'mono', title: k.public_key }, short), ' ',
          h('button', { type: 'button', class: 'small', onclick: (e) => attach(k.public_key, e.currentTarget) }, '添加到这台实例'));
      })) : empty('账户里没有 SSH 密钥'));
    } catch (err) {
      mount(list, errorBox(err));
    }
  }

  // lastBench is the newest benchmark of this instance, else of its machine.
  function lastBench(x) {
    const b = benches.find((v) => v.instance_id === x.id) || benches.find((v) => v.machine_id === x.machine_id);
    if (!b) return null;
    return h('div', { class: 'inst-bench' }, h('span', { class: 'hint' }, b.instance_id === x.id ? '算力检测：' : '这台机器以前测过：'),
      h('button', { type: 'button', class: 'link', title: '看完整结果', onclick: () => benchDialog(x, b) }, summaryLine(b, benches, baselines)));
  }

  // benchDialog runs the quick benchmark on x; shown, when given, is an
  // earlier result to show first.
  function benchDialog(x, shown) {
    const d = openDialog({ wide: true });
    d.dialog.classList.add('bench');
    const force = h('input', { type: 'checkbox' });
    const start = h('button', { type: 'button', class: 'primary' }, shown ? '再测一次' : '开始检测');
    const out = h('div');
    const actions = h('div', { class: 'toolbar end' });
    const showResult = (b) => {
      const { ev, node } = report(b, benches, baselines);
      mount(out, node);
      const bad = ev.verdict === 'bad' && !marks.black.has(x.machine_id);
      mount(actions,
        bad ? h('button', {
          type: 'button', class: 'danger-solid',
          onclick: () => {
            d.close();
            openMarkDialog('black', x, marks, `实例 ${x.id} 算力检测`, onMarks,
              `${ev.broken ? '坏卡' : '疑似假卡'}：${ev.problems.join('；')}`.slice(0, 200));
          },
        }, '拉黑这台机器') : null,
        ev.verdict === 'ok' ? h('button', {
          type: 'button', title: '把这次成绩记进“设置 → 性能基准数据库”，以后同型号同功耗的卡拿它比',
          onclick: async (e) => {
            const btn = e.currentTarget;
            btn.disabled = true;
            try {
              baselines = (await put('baselines', asBaseline(b, ev))).baselines;
              btn.textContent = '已存为基准';
            } catch (err) {
              btn.disabled = false;
              mount(out, errorBox(err), node);
            }
          },
        }, `存为基准（${b.gpu_name}${ev.pw.limit ? ' ' + Math.round(ev.pw.limit) + 'W' : ''}）`) : null,
        h('button', { type: 'button', onclick: d.close }, '关闭'));
    };
    start.addEventListener('click', async () => {
      start.disabled = true;
      mount(out, h('p', { class: 'muted' }, '检测中，大约一分钟（连 SSH、测算力、写满显存再校验）…'));
      mount(actions, h('button', { type: 'button', onclick: d.close }, '关到后台'));
      try {
        const b = await post(`instances/${x.id}/bench`, { force: force.checked });
        benches = [b, ...benches];
        if (d.dialog.isConnected) showResult(b);
        render();
      } catch (err) {
        if (d.dialog.isConnected) mount(out, errorBox(err));
      } finally {
        start.disabled = false;
      }
    });
    mount(d.body,
      h('h2', null, `算力检测：实例 ${x.id} · ${x.num_gpus}× ${x.gpu_name}`),
      h('p', { class: 'hint' }, '通过 SSH 在实例上跑：FP16 / BF16 / FP32 矩阵乘、显存拷贝、写满显存再校验、计算结果校验、型号 / SM 数 / 显存容量核对、坏页和 ECC 检查。',
        '会短暂占满空闲显存，ComfyUI 正在出图时别测。'),
      h('div', { class: 'toolbar' }, start, h('label', { class: 'field inline' }, force, 'GPU 正忙也测')),
      out, actions);
    if (shown) showResult(shown);
  }

  // tmuxNote shows how turning off auto-tmux after boot is going.
  function tmuxNote(x) {
    const st = setups.get(x.id);
    if (!st || !st.no_tmux) return null;
    if (st.done_at) return h('span', { class: 'badge on', title: `${fmtTime(st.done_at)} 关闭` }, '自动 tmux 已关');
    if (st.err === '超时没做成' || st.err === '实例已经不在了') return h('span', { class: 'badge bad', title: st.err }, '没能关自动 tmux');
    return h('span', { class: 'badge warn', title: st.tries ? `试了 ${st.tries} 次：${st.err}` : '等实例跑起来' }, '待关自动 tmux');
  }

  async function noTmux(x) {
    try {
      await post(`instances/${x.id}/notmux`, {});
      refreshSoon();
    } catch (err) {
      mount(message, errorBox(err));
    }
  }

  async function copy(btn, text) {
    try {
      await copyText(text);
      const old = btn.textContent;
      btn.textContent = '已复制';
      setTimeout(() => { btn.textContent = old; }, 1500);
    } catch (err) {
      mount(message, errorBox(err));
    }
  }

  function actionBar(x) {
    const running = (x.intended_status || x.cur_state) === 'running';
    const url = portalURL(x);
    const ssh = sshCommand(x);
    const btn = (text, onclick, cls, title) => h('button', { type: 'button', class: 'small' + (cls ? ' ' + cls : ''), onclick, title }, text);
    return h('div', { class: 'inst-actions' },
      url ? h('a', { class: 'button primary', href: url, target: '_blank', rel: 'noopener noreferrer', title: 'vast Instance Portal' }, '打开')
        : h('button', { type: 'button', class: 'primary', disabled: true, title: '运行起来、端口映射出来后才能打开' },
          x.actual_status === 'running' ? '打开' : '启动中…'),
      h('span', { class: 'sep' }),
      running ? btn('停止', () => confirmAction(x, 'stop'), null, '停止实例：GPU 不计费，磁盘照常计费')
        : btn('启动', () => confirmAction(x, 'start'), 'ok', '启动实例'),
      btn('重启', () => confirmAction(x, 'reboot'), null, '重启容器，数据保留'),
      btn('重建', () => confirmAction(x, 'recycle'), 'warn', '重新拉镜像重建容器，数据丢失'),
      btn('销毁', () => confirmAction(x, 'destroy'), 'danger', '永久删除实例'),
      h('span', { class: 'sep' }),
      x.actual_status === 'running' ? h('a', { class: 'button small', href: `#/terminal?instance=${x.id}`, title: '打开网页终端和文件浏览器' }, '终端') : null,
      x.actual_status === 'running' ? btn('下载', () => openURLDialog(x, isActive(jobs.get(x.id)), (j) => { jobs.set(x.id, j); dlFolds.delete(j.id); renderDownloads(); }),
        null, '贴链接直接下到实例上（Hugging Face / Civitai 自动带 token，其它 http(s) 链接也行），不用先加进下载配置') : null,
      x.actual_status === 'running' ? btn('启动下载器', () => openDownloadDialog(x, (j) => { jobs.set(x.id, j); dlFolds.delete(j.id); renderDownloads(); }),
        isActive(jobs.get(x.id)) ? null : 'ok', '用 aria2 把“设置 → 启动下载配置”里的文件下到实例上，并同步 ComfyUI 的 input') : null,
      x.actual_status === 'running' ? btn('运行脚本', () => openScriptsDialog(x, (j) => { scJobs.set(x.id, j); scFolds.delete(j.id); renderDownloads(); }),
        null, '在实例上按顺序运行“设置 → 开机脚本”里的脚本') : null,
      x.actual_status === 'running' ? btn('备份 ComfyUI input', (e) => backupInput(x, e.currentTarget), null, '把实例上 ComfyUI 的 input 目录（上传的图片 / 视频）拉回面板机，合并进 backups/input.tar.gz') : null,
      btn('算力检测', () => benchDialog(x), null, '跑一分钟左右的算力 / 显存检测，查假卡、坏卡'),
      btn('日志', () => logsDialog(x)),
      btn('改名', () => labelDialog(x)),
      btn('SSH 密钥', () => sshDialog(x)),
      x.actual_status === 'running' && !(setups.get(x.id) || {}).done_at
        ? btn('关自动 tmux', () => noTmux(x), null, 'vast 镜像默认 SSH 一登录就进 tmux；建 ~/.no_auto_tmux 关掉它') : null,
      markButtons(x, marks, `实例 ${x.id}${x.label ? ' · ' + x.label : ''}`, onMarks),
      ssh ? btn('复制 SSH 命令', (e) => copy(e.currentTarget, ssh), null, ssh) : null,
      h('span', { class: 'spacer' }),
      x.label ? h('span', { class: 'badge label-tag', title: '实例名称' }, x.label) : null);
  }

  function card(x, now) {
    const open = !collapsed.has(x.id);
    const vramTotal = (x.gpu_totalram || x.gpu_ram || 0) / 1024;
    const vram = num(x.vmem_usage);
    const memLimit = num(x.mem_limit) ?? ((x.cpu_ram || 0) * (x.gpu_frac || 1)) / 1000;
    const mem = num(x.mem_usage);
    const disk = num(x.disk_usage);
    const running = x.actual_status === 'running';
    const up = running && x.start_date ? now - x.start_date : NaN;
    const left = x.end_date ? x.end_date - now : NaN;
    const price = hourly(x);
    const toggle = h('button', {
      type: 'button', class: 'link chevron', title: open ? '收起' : '展开', 'aria-expanded': String(open),
      onclick: () => {
        if (open) collapsed.add(x.id);
        else collapsed.delete(x.id);
        render();
      },
    }, open ? '▾' : '▸');
    return h('section', { class: 'card inst-card' },
      h('div', { class: 'inst-head' },
        toggle,
        h('span', { class: 'inst-title' }, `${x.num_gpus}× ${x.gpu_name}`),
        powerBadge(benches.find((v) => v.instance_id === x.id && v.result) || benches.find((v) => v.machine_id === x.machine_id && v.result)),
        statusBadge(x),
        h('span', { class: 'spacer' }),
        h('span', { class: 'inst-meta', title: '可靠性' }, `可靠 ${fixed((x.reliability2 ?? x.reliability ?? 0) * 100, 2)}%`),
        x.verification === 'verified' ? h('span', { class: 'badge on' }, '已认证') : h('span', { class: 'badge' }, x.verification || '未认证'),
        h('span', { class: 'inst-meta', title: x.geolocation || '' }, fmtGeo(x.geolocation)),
        x.public_ipaddr ? h('span', { class: 'inst-box mono', title: '公网 IP' }, x.public_ipaddr) : null,
        h('span', { class: 'inst-meta', title: '已运行' }, `⏱ ${running ? fmtSpan(up) : '—'}`),
        h('span', { class: 'inst-meta', title: `可租到 ${fmtTime(Math.round(x.end_date || 0))}` }, `剩 ${Number.isFinite(left) ? fmtSpan(left) : '—'}`),
        h('span', {
          class: 'inst-box strong',
          title: `运行时 $${fixed(x.dph_total, 3)}/h（GPU $${fixed(x.dph_base, 3)} + 磁盘 $${fixed(x.storage_total_cost, 3)}）；启动中、停止时只收磁盘费`,
        }, `$${fixed(price, 3)}/h`)),
      open ? h('div', { class: 'inst-body' },
        h('div', { class: 'inst-grid' },
          col('实例',
            ['ID：', h('span', { class: 'mono' }, String(x.id))],
            `主机：${x.host_id ?? '—'}`,
            ['机器：', h('a', { href: `#/machines/${x.machine_id}/${x.num_gpus}` }, String(x.machine_id ?? '—'))],
            h('span', { class: 'mono hint', title: x.image_uuid || '' }, x.template_name ? `${x.template_name} 模板` : (x.image_uuid || '—'))),
          col('GPU',
            `CUDA ${fixed(x.cuda_max_good, 1)} · ${fixed(x.total_flops, 1)} TFLOPS`,
            [`显存 ${vram === null ? 0 : fixed(vram, 1)}/${fixed(vramTotal, 1)} GB `, meter(vram === null ? 0 : vram / vramTotal)],
            `显存带宽 ${fixed(x.gpu_mem_bw, 1)} GB/s`,
            num(x.gpu_util) !== null ? [`GPU 占用 ${fixed(x.gpu_util, 0)}% `, meter(x.gpu_util / 100)] : null,
            num(x.gpu_temp) > 0 ? `温度 ${fixed(x.gpu_temp, 0)}°C` : null),
          col('DLPerf', fixed(x.dlperf, 1), `${fixed(x.dlperf_per_dphtotal, 1)} DLP/$/h`),
          col('网络',
            `端口：${x.direct_port_count ?? '—'}`,
            `↑ ${fixed(x.inet_up, 0)} Mbps · ↓ ${fixed(x.inet_down, 0)} Mbps`,
            ['流量费 ', h('span', { class: costClass(x.internet_down_cost_per_tb) }, '↓$' + fixed(x.internet_down_cost_per_tb, 1)),
              ' / ', h('span', { class: costClass(x.internet_up_cost_per_tb) }, '↑$' + fixed(x.internet_up_cost_per_tb, 1)), ' 每 TB']),
          col('主板',
            h('span', { title: x.mobo_name || '' }, x.mobo_name || '—'),
            `PCIe ${fixed(x.pci_gen, 1)} ×${x.gpu_lanes ?? '?'} · ${fixed(x.pcie_bw, 1)} GB/s`),
          col('CPU',
            h('span', { title: x.cpu_name || '' }, shortCPU(x.cpu_name)),
            [`${cores(x.cpu_cores_effective)}/${cores(x.cpu_cores)} 核 `, meter(x.cpu_cores ? x.cpu_cores_effective / x.cpu_cores : 0)],
            [`内存 ${mem === null ? 0 : fixed(mem, 1)}/${fixed(memLimit, 1)} GB `, meter(mem === null ? 0 : mem / memLimit)]),
          spendCol(spendOf(x.id), x),
          col('磁盘',
            h('span', { title: x.disk_name || '' }, x.disk_name || '—'),
            `${fixed(x.disk_bw, 1)} MB/s`,
            [`${disk === null ? 0 : fixed(disk, 1)}/${fixed(x.disk_space, 1)} GB `, meter(disk === null ? 0 : disk / x.disk_space)])),
        actionBar(x),
        inBox(x.id),
        dlBox(x.id),
        scBox(x.id),
        lastBench(x),
        portList(x, copy)) : null,
      h('div', { class: 'inst-status' },
        tmuxNote(x),
        h('span', { class: 'hint' }, '状态：'),
        h('span', { class: 'mono' }, (x.status_msg || '').trim() || '—'),
        h('span', { class: 'spacer' }),
        sshCommand(x) ? h('span', { class: 'mono hint' }, sshCommand(x)) : null));
  }

  function spendOf(id) {
    return bill ? bill.instances.find((sp) => sp.id === id) : null;
  }

  function renderSpend() {
    if (!bill) {
      mount(spendBox, billErr ? errorBox(billErr) : null);
      return;
    }
    mount(spendBox,
      billErr ? errorBox(billErr) : null,
      h('div', { class: 'cards' },
        spendStat('账户余额', money(bill.credit, 2), 'vast 账户 credit',
          h('a', { class: 'stat-link', href: 'https://cloud.vast.ai/billing/', target: '_blank', rel: 'noopener noreferrer', title: '打开 vast 账单 / 充值页面' }, '充值 ↗')),
        spendStat('今天花费', money(bill.today, 2),
          bill.today_alert ? `按 UTC 日 · 超过 ${money(bill.today_alert, 2)} 提醒${bill.today > bill.today_alert ? '（已超）' : ''}` : '按 UTC 日',
          null, bill.today_alert && bill.today > bill.today_alert ? 'bad' : ''),
        spendStat('近 7 天', money(bill.week, 2)),
        spendStat('近 30 天', money(bill.month, 2), `${bill.instances.length} 个实例`)),
      h('p', { class: 'hint' }, `费用来自 vast 账单，会比实际慢几分钟到一小时；更新于 ${fmtTime(bill.updated_at)}。`));
  }

  function render() {
    if (!data) return;
    renderSpend();
    const all = data.instances;
    const labels = {};
    for (const x of all) if (x.label) labels[x.label] = (labels[x.label] || 0) + 1;
    if (tab && !labels[tab]) tab = '';
    const shown = tab ? all.filter((x) => x.label === tab) : all;
    const tabBtn = (key, text) => h('button', {
      type: 'button', class: key === tab ? 'tab active' : 'tab',
      onclick: () => { tab = key; render(); },
    }, text);
    const allOpen = shown.every((x) => !collapsed.has(x.id));
    mount(tabsBox,
      tabBtn('', `全部（${all.length}）`),
      Object.keys(labels).sort().map((l) => tabBtn(l, `${l}（${labels[l]}）`)),
      h('span', { class: 'spacer' }),
      shown.length ? h('button', {
        type: 'button', class: 'small',
        onclick: () => {
          for (const x of shown) {
            if (allOpen) collapsed.add(x.id);
            else collapsed.delete(x.id);
          }
          render();
        },
      }, allOpen ? '全部收起' : '全部展开') : null);
    const now = data.now || Date.now() / 1000;
    mount(cardsBox, shown.length ? shown.map((x) => card(x, now)) : h('section', { class: 'card' }, empty('账户下现在没有实例')));
    renderDownloads();
    mount(historyBox,
      bill ? h('details', { class: 'card' }, h('summary', null, `费用明细（近 30 天，${bill.instances.length} 个实例）`),
        spendTable(bill.instances, new Set(all.map((x) => x.id)))) : null,
      h('details', { class: 'card' }, h('summary', null, `实例操作记录（${data.actions.length}）`), actionsTable(data.actions)),
      h('details', { class: 'card' }, h('summary', null, `开机记录（${data.rentals.length}）`), rentalsTable(data.rentals, marks, onMarks)));
  }

  ctx.show(
    h('div', { class: 'title-row' }, h('h1', null, '实例'), h('span', { class: 'spacer' }),
      h('button', { type: 'button', onclick: refresh }, '刷新')),
    message, inputNote, spendBox, tabsBox, cardsBox, historyBox, note);
  await refresh();
}

function spendTable(list, alive) {
  if (!list.length) return empty('近 30 天没有费用');
  const amt = (sp, t) => (sp.items[t] ? money(sp.items[t].amount) : '—');
  const traffic = (sp, t) => {
    const g = chargeGB(sp.items[t]);
    return sp.items[t] ? `${g === null ? '' : fixed(g, 1) + ' GB '}${money(sp.items[t].amount)}` : '—';
  };
  const day = (t) => fmtTime(t).slice(0, 10);
  return table([
    { title: '实例', class: 'mono', cell: (sp) => [String(sp.id), alive.has(sp.id) ? h('span', { class: 'badge on' }, '在用') : null] },
    { title: '名称', cell: (sp) => sp.label || '—' },
    { title: '日期（UTC）', class: 'nowrap', cell: (sp) => (sp.start === sp.end ? day(sp.start) : `${day(sp.start)} ~ ${day(sp.end)}`) },
    { title: 'GPU', class: 'num', cell: (sp) => amt(sp, 'gpu') },
    { title: '磁盘', class: 'num', cell: (sp) => amt(sp, 'disk') },
    { title: '下行流量', class: 'num', cell: (sp) => traffic(sp, 'bwd') },
    { title: '上行流量', class: 'num', cell: (sp) => traffic(sp, 'bwu') },
    { title: '合计', class: 'num strong', cell: (sp) => money(sp.amount) },
  ], list);
}

function actionsTable(list) {
  if (!list.length) return empty('还没有操作过实例');
  return table([
    { title: '时间', class: 'nowrap', cell: (a) => [fmtTime(a.ts), h('div', { class: 'hint' }, fmtAgo(a.ts))] },
    { title: '实例', class: 'mono', cell: (a) => String(a.instance_id) },
    { title: '操作', class: 'nowrap', cell: (a) => a.action },
    { title: '内容', cell: (a) => (a.detail ? h('span', { class: 'mono' }, a.detail) : '—') },
    { title: '结果', cell: (a) => (a.err ? [h('span', { class: 'badge bad' }, '失败'), h('div', { class: 'hint' }, a.err)] : h('span', { class: 'badge on' }, '成功')) },
  ], list);
}

function rentalsTable(list, marks, onMarks) {
  if (!list.length) return empty('还没有从面板开过机');
  return table([
    { title: '时间', class: 'nowrap', cell: (r) => [fmtTime(r.ts), h('div', { class: 'hint' }, fmtAgo(r.ts))] },
    { title: '结果', cell: (r) => (r.err ? [h('span', { class: 'badge bad' }, '失败'), h('div', { class: 'hint' }, r.err)] : h('span', { class: 'badge on' }, '成功')) },
    { title: '实例', class: 'mono', cell: (r) => (r.instance_id ? String(r.instance_id) : '—') },
    { title: '机器', class: 'nowrap', cell: (r) => h('a', { href: `#/machines/${r.machine_id}/${r.num_gpus}` }, `${r.machine_id} · ${r.num_gpus}× ${r.gpu_name}`) },
    { title: '收藏 / 拉黑', class: 'nowrap', cell: (r) => markButtons(r, marks, r.instance_id ? `实例 ${r.instance_id}` : '开机记录', onMarks) },
    { title: '模板', cell: (r) => r.template_name || h('span', { class: 'mono' }, r.template_hash_id) },
    { title: '磁盘 GB', class: 'num', cell: (r) => fixed(r.disk, 0) },
    { title: '$/h', class: 'num', cell: (r) => fixed(r.dph, 3) },
    { title: '名称', cell: (r) => r.label || '—' },
  ], list);
}
