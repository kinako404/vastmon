// Formatting helpers. Times from the API are Unix seconds.

const pad = (n) => String(n).padStart(2, '0');

export function fmtTime(sec) {
  if (!sec) return '—';
  const d = new Date(sec * 1000);
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ` +
    `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}

// fmtClock is the clock time on today's date and month-day plus time before.
export function fmtClock(sec) {
  if (!sec) return '—';
  const d = new Date(sec * 1000);
  const hm = `${pad(d.getHours())}:${pad(d.getMinutes())}`;
  if (d.toDateString() === new Date().toDateString()) return hm;
  return `${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${hm}`;
}

export function fmtAgo(sec) {
  if (!sec) return '从未';
  const s = Math.round(Date.now() / 1000 - sec);
  if (s < 5) return '刚刚';
  if (s < 60) return `${s} 秒前`;
  if (s < 3600) return `${Math.floor(s / 60)} 分钟前`;
  if (s < 86400) return `${Math.floor(s / 3600)} 小时前`;
  return `${Math.floor(s / 86400)} 天前`;
}

export function fmtDuration(sec) {
  if (sec % 3600 === 0) return `${sec / 3600} 小时`;
  if (sec % 60 === 0) return `${sec / 60} 分钟`;
  return `${sec} 秒`;
}

// fixed shows n with d decimals; missing values are '—'.
export function fixed(n, d) {
  if (n === null || n === undefined || !Number.isFinite(Number(n))) return '—';
  return Number(n).toFixed(d);
}

export function gb(mb) {
  return fixed(mb / 1024, 0);
}

export function days(sec) {
  return fixed(sec / 86400, 1);
}

// costClass grades a traffic price in $/TB.
export function costClass(perTB) {
  if (perTB >= 20) return 'cost-bad';
  if (perTB >= 8) return 'cost-warn';
  return 'cost-ok';
}

// shortCPU trims a CPU model for a table cell: "AMD EPYC 7Y83 64-Core
// Processor" becomes "EPYC 7Y83", "13th Gen Core™ i5-13600KF" "Core i5-13600KF".
export function shortCPU(name) {
  if (!name) return '—';
  return name
    .replace(/[™®]|\((?:R|TM)\)/g, '')
    .replace(/^(AMD|Intel)\s+/i, '')
    .replace(/^\d+(st|nd|rd|th) Gen\s+/i, '')
    .replace(/\s+\d+-Cores?\b/i, '')
    .replace(/\s+(Processor|CPU)\b.*$/i, '')
    .replace(/\s+/g, ' ')
    .trim() || name;
}

// cores shows a core count, with one decimal only when it is fractional.
export function cores(n) {
  return Number.isInteger(n) ? String(n) : fixed(n, 1);
}
