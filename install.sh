#!/usr/bin/env bash
# vastmon one-click installer / 一键安装脚本
#
#   curl -fsSL https://raw.githubusercontent.com/kinako404/vastmon/main/install.sh | sudo bash
#
#   sudo bash install.sh [install]          install or upgrade (default) / 安装或升级（默认）
#   sudo bash install.sh upgrade            upgrade the binary only / 只升级程序
#   sudo bash install.sh passwd             change the panel password / 修改面板密码
#   sudo bash install.sh uninstall [--purge]  remove (--purge also deletes data) / 卸载（--purge 连数据一起删）
#
# Non-interactive / 无人值守：
#   VAST_API_KEY=...  VASTMON_PASSWORD=...  VASTMON_ADDR=0.0.0.0:8090  VASTMON_VERSION=v0.1.0

set -euo pipefail

REPO="${VASTMON_REPO:-kinako404/vastmon}"
SERVICE="vastmon"
APP_USER="vastmon"
BIN_DIR="/opt/vastmon"
BIN="$BIN_DIR/vastmon"
ENV_FILE="/etc/vastmon/vastmon.env"
DATA_DIR="/var/lib/vastmon"
DB="$DATA_DIR/vastmon.db"
UNIT="/etc/systemd/system/$SERVICE.service"

if [ -t 1 ]; then
  C_OK=$'\033[32m' C_WARN=$'\033[33m' C_ERR=$'\033[31m' C_DIM=$'\033[2m' C_END=$'\033[0m'
else
  C_OK='' C_WARN='' C_ERR='' C_DIM='' C_END=''
fi
info() { printf '%s==>%s %s\n' "$C_OK" "$C_END" "$*"; }
warn() { printf '%s[!]%s %s\n' "$C_WARN" "$C_END" "$*" >&2; }
die()  { printf '%s[x]%s %s\n' "$C_ERR" "$C_END" "$*" >&2; exit 1; }

# The script may be piped into bash, so questions read from the terminal.
# 脚本可能是通过管道交给 bash 的，所以从终端读输入。
have_tty() { [ -r /dev/tty ] && [ -w /dev/tty ] && (: < /dev/tty) 2>/dev/null; }
ask() {        # ask "prompt" default -> echo answer
  local reply=""
  have_tty || { printf '%s' "$2"; return; }
  printf '%s' "$1" > /dev/tty
  IFS= read -r reply < /dev/tty || true
  printf '%s' "${reply:-$2}"
}
ask_secret() { # ask_secret "prompt" -> echo answer (not echoed on screen)
  local reply=""
  have_tty || return 0
  printf '%s' "$1" > /dev/tty
  IFS= read -rs reply < /dev/tty || true
  printf '\n' > /dev/tty
  printf '%s' "$reply"
}

need_root() { [ "$(id -u)" -eq 0 ] || die "请用 root 运行 / please run as root (sudo)"; }

check_system() {
  [ "$(uname -s)" = "Linux" ] || die "只支持 Linux / Linux only"
  command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ] || die "需要 systemd / systemd is required"
  case "$(uname -m)" in
    x86_64|amd64) ARCH=amd64 ;;
    aarch64|arm64) ARCH=arm64 ;;
    *) die "不支持的架构 / unsupported architecture: $(uname -m)" ;;
  esac
  command -v curl >/dev/null 2>&1 || die "需要 curl / curl is required"
  command -v sha256sum >/dev/null 2>&1 || die "需要 sha256sum (coreutils)"
}

release_url() {
  if [ -n "${VASTMON_VERSION:-}" ]; then
    printf 'https://github.com/%s/releases/download/%s' "$REPO" "$VASTMON_VERSION"
  else
    printf 'https://github.com/%s/releases/latest/download' "$REPO"
  fi
}

# download_binary fetches the release binary for this machine and checks its
# SHA-256 against the release's SHA256SUMS.
download_binary() {
  local url tmp name
  url="$(release_url)"
  name="vastmon-linux-$ARCH"
  tmp="$(mktemp -d)"
  info "下载 / downloading $name ($url)"
  if ! curl -fL --retry 3 --progress-bar -o "$tmp/$name" "$url/$name"; then
    rm -rf "$tmp"; die "下载失败 / download failed: $url/$name"
  fi
  if ! curl -fsSL --retry 3 -o "$tmp/SHA256SUMS" "$url/SHA256SUMS"; then
    rm -rf "$tmp"; die "下载校验文件失败 / cannot fetch SHA256SUMS"
  fi
  if ! (cd "$tmp" && grep " $name\$" SHA256SUMS | sha256sum -c --status); then
    rm -rf "$tmp"; die "SHA-256 校验失败 / checksum mismatch"
  fi
  install -d -m 755 "$BIN_DIR"
  install -m 755 "$tmp/$name" "$BIN.new"
  mv -f "$BIN.new" "$BIN"
  rm -rf "$tmp"
  info "已安装 / installed: $("$BIN" version)"
}

ensure_user() {
  if ! id "$APP_USER" >/dev/null 2>&1; then
    useradd --system --home-dir "$DATA_DIR" --shell /usr/sbin/nologin "$APP_USER" 2>/dev/null \
      || useradd -r -d "$DATA_DIR" -s /sbin/nologin "$APP_USER"
    info "已创建系统用户 / created system user: $APP_USER"
  fi
  install -d -m 750 -o "$APP_USER" -g "$APP_USER" "$DATA_DIR"
}

env_get() { [ -f "$ENV_FILE" ] && sed -n "s/^$1=//p" "$ENV_FILE" | tail -n1 | sed 's/^"//; s/"$//' || true; }

write_env() {
  local key="$1" addr="$2" args="$3"
  install -d -m 755 "$(dirname "$ENV_FILE")"
  umask 077
  cat > "$ENV_FILE" <<EOF
# vastmon settings. Restart after editing: systemctl restart $SERVICE
# vastmon 配置，改完重启：systemctl restart $SERVICE

# vast.ai API key (https://cloud.vast.ai/account/) — it can rent machines and spend credit, keep it secret.
# vast.ai API key（https://cloud.vast.ai/account/）——它能开机花钱，不要外传。
VAST_API_KEY=$key

# Listen address / 监听地址
VASTMON_ADDR=$addr

# Extra flags, e.g. "-interval 5m -disk 120" / 其它参数，例如 "-interval 5m -disk 120"
VASTMON_ARGS="$args"

# Where the SSH key (private + passwd.txt), backups/ (ComfyUI input) live / 项目目录
VASTMON_PROJECT=$DATA_DIR
EOF
  umask 022
  chmod 600 "$ENV_FILE"
}

configure() {
  local key addr args
  key="${VAST_API_KEY:-$(env_get VAST_API_KEY)}"
  addr="${VASTMON_ADDR:-$(env_get VASTMON_ADDR)}"
  args="${VASTMON_ARGS:-$(env_get VASTMON_ARGS)}"
  if [ -z "$key" ]; then
    printf '\n' > /dev/tty 2>/dev/null || true
    key="$(ask_secret '请输入 vast.ai API key / Enter your vast.ai API key (cloud.vast.ai → Account → API Keys): ')"
  fi
  [ -n "$key" ] || die "需要 vast.ai API key：设置 VAST_API_KEY 环境变量再运行 / set VAST_API_KEY and run again"
  if [ -z "$addr" ]; then
    addr="$(ask '监听地址 / listen address [0.0.0.0:8090]: ' '0.0.0.0:8090')"
  fi
  write_env "$key" "${addr:-0.0.0.0:8090}" "$args"
}

write_unit() {
  cat > "$UNIT" <<EOF
[Unit]
Description=vastmon - vast.ai GPU monitor & rental panel
Documentation=https://github.com/$REPO
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=$APP_USER
Group=$APP_USER
EnvironmentFile=$ENV_FILE
WorkingDirectory=$DATA_DIR
ExecStart=$BIN -addr \${VASTMON_ADDR} -db $DB \$VASTMON_ARGS
Restart=on-failure
RestartSec=5
TimeoutStopSec=15

# hardening / 加固
UMask=0077
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateDevices=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictSUIDSGID=true
LockPersonality=true
ReadWritePaths=$DATA_DIR

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
}

has_password() {
  [ -f "$DB" ] && runuser -u "$APP_USER" -- "$BIN" -db "$DB" password-set >/dev/null 2>&1
}

set_password() {
  local pw="${1:-}" again=""
  if [ -z "$pw" ]; then
    if have_tty; then
      pw="$(ask_secret '设置面板登录密码（至少 8 位，直接回车随机生成）/ panel password (8+ chars, Enter = random): ')"
      if [ -n "$pw" ]; then
        again="$(ask_secret '再输一次 / again: ')"
        [ "$pw" = "$again" ] || die "两次输入不一样 / passwords do not match"
      fi
    fi
    if [ -z "$pw" ]; then
      # every stage reads its whole input, so nothing gets SIGPIPE under pipefail
      # 每一段都读完输入，pipefail 下不会因为 SIGPIPE 退出
      pw="$(head -c 48 /dev/urandom | base64 | LC_ALL=C tr -dc 'A-Za-z0-9' | cut -c1-16)"
      GENERATED_PW="$pw"
    fi
  fi
  [ "${#pw}" -ge 8 ] || die "密码至少 8 位 / password must be at least 8 characters"
  printf '%s\n%s\n' "$pw" "$pw" | runuser -u "$APP_USER" -- "$BIN" -db "$DB" passwd >/dev/null 2>&1 \
    || die "设置密码失败 / failed to set the password"
}

public_url() {
  local addr host port ip
  addr="$(env_get VASTMON_ADDR)"
  host="${addr%:*}"; port="${addr##*:}"
  if [ -z "$host" ] || [ "$host" = "0.0.0.0" ] || [ "$host" = "[::]" ]; then
    ip="$(curl -fsS -m 5 https://api.ipify.org 2>/dev/null || hostname -I 2>/dev/null | awk '{print $1}')"
    host="${ip:-<server-ip>}"
  fi
  printf 'http://%s:%s' "$host" "$port"
}

wait_up() {
  local _
  for _ in $(seq 1 20); do
    if systemctl is-active --quiet "$SERVICE"; then
      sleep 1
      systemctl is-active --quiet "$SERVICE" && return 0
    fi
    sleep 1
  done
  return 1
}

cmd_install() {
  need_root; check_system
  local fresh=1
  [ -x "$BIN" ] && fresh=0
  download_binary
  ensure_user
  configure
  write_unit
  if ! has_password || [ -n "${VASTMON_PASSWORD:-}" ]; then
    set_password "${VASTMON_PASSWORD:-}"
  fi
  systemctl enable "$SERVICE" >/dev/null 2>&1
  systemctl restart "$SERVICE"
  if ! wait_up; then
    journalctl -u "$SERVICE" -n 30 --no-pager >&2 || true
    die "服务没起来，看上面的日志 / the service did not start, see the log above"
  fi
  echo
  info "vastmon 已运行 / is running"
  printf '   %s面板 / panel:%s  %s\n' "$C_DIM" "$C_END" "$(public_url)"
  if [ -n "${GENERATED_PW:-}" ]; then
    printf '   %s密码 / password:%s %s   %s(请记下来 / write it down)%s\n' "$C_DIM" "$C_END" "$GENERATED_PW" "$C_WARN" "$C_END"
  fi
  printf '   %s配置 / config:%s %s\n' "$C_DIM" "$C_END" "$ENV_FILE"
  printf '   %s数据 / data:%s   %s\n' "$C_DIM" "$C_END" "$DATA_DIR"
  printf '   %s日志 / logs:%s   journalctl -u %s -f\n' "$C_DIM" "$C_END" "$SERVICE"
  [ "$fresh" = 1 ] && warn "面板是 http。浏览器通知、对外长期使用请在前面加 HTTPS 反代（见文档）/ put an HTTPS reverse proxy in front for notifications (see README)"
  return 0
}

cmd_upgrade() {
  need_root; check_system
  [ -f "$UNIT" ] || die "还没安装，先运行 install / not installed yet, run install first"
  download_binary
  write_unit
  systemctl restart "$SERVICE"
  wait_up || die "升级后服务没起来：journalctl -u $SERVICE -n 50 / service failed after upgrade"
  info "已升级并重启 / upgraded and restarted"
}

cmd_passwd() {
  need_root
  [ -x "$BIN" ] || die "还没安装 / not installed"
  set_password "${VASTMON_PASSWORD:-}"
  [ -n "${GENERATED_PW:-}" ] && info "新密码 / new password: $GENERATED_PW"
  info "密码已修改，所有已登录的会话都已失效 / password changed, all sessions signed out"
}

cmd_uninstall() {
  need_root
  local purge=0
  [ "${1:-}" = "--purge" ] && purge=1
  systemctl disable --now "$SERVICE" >/dev/null 2>&1 || true
  rm -f "$UNIT"
  systemctl daemon-reload
  rm -rf "$BIN_DIR"
  if [ "$purge" = 1 ]; then
    rm -rf "$DATA_DIR" "$(dirname "$ENV_FILE")"
    userdel "$APP_USER" >/dev/null 2>&1 || true
    info "已卸载，数据和配置也已删除 / uninstalled, data and config removed"
  else
    info "已卸载；数据还在 $DATA_DIR，配置在 $ENV_FILE（加 --purge 一起删）/ uninstalled; data and config kept (use --purge to delete)"
  fi
}

main() {
  local cmd="${1:-install}"
  shift || true
  case "$cmd" in
    install) cmd_install ;;
    upgrade|update) cmd_upgrade ;;
    passwd|password) cmd_passwd ;;
    uninstall|remove) cmd_uninstall "${1:-}" ;;
    -h|--help|help) sed -n '2,13p' "$0" 2>/dev/null || true ;;
    *) die "不认识的命令 / unknown command: $cmd (install | upgrade | passwd | uninstall [--purge])" ;;
  esac
}

main "$@"
