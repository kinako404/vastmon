# vastmon

[简体中文](README.md) | **English**

vastmon is a self-hosted monitoring and rental panel for [vast.ai](https://cloud.vast.ai/). It keeps an eye on the GPU market for you, folds bandwidth charges into the real cost so the genuinely cheap machines rise to the top, and — once a machine is rented — lets you check the card is real, open a terminal, move files, pull models automatically and get browser notifications when an instance misbehaves or your balance runs low, all from one web page.

It is a single Go binary and a single SQLite file: no database server, no Node, no Docker.

> The web interface is currently in Chinese only. This guide explains every screen, and browser translation works well on it.

---

## Why

The vast.ai search page filters by GPU, bandwidth and reliability, but **not by bandwidth price**. Two machines at the same hourly rate can charge under $1 or over $40 per TB of download; pulling a 30 GB model can quietly add a dollar or two to the bill, and nothing in the list tells you.

vastmon polls the offers every two minutes, applies your filters, and ranks machines by an **estimated daily cost** — hourly price × 24 plus your expected daily traffic × the machine's bandwidth price — so the expensive surprises are visible up front.

## Features

**Finding machines**
- Filter by model, GPU count, VRAM, CPU cores, RAM, disk, CUDA version, reliability, rental duration, open ports, up/down bandwidth, **up/down bandwidth price** and hourly price
- Estimated daily / weekly / monthly cost, sortable by any column; per-machine price history
- Save filter sets as presets and switch with one click
- Favorites and a blacklist (mutually exclusive), highlighted green / red with your notes; favorites are re-checked for availability on every poll

**Renting and managing instances**
- Rent with a template and disk size of your choice; the price is re-checked against the latest offer before renting
- Instance cards modeled after vast.ai's: status, usage, cost, port mappings (click to copy `IP:port`)
- Start, stop, reboot, recycle, destroy, rename, logs, SSH key management
- Account balance, spend today / last 7 days / last 30 days, per-instance GPU / disk / traffic breakdown
- Turns off the vast images' "jump into tmux on SSH login" behavior after boot

**Spotting fake or broken GPUs**
- One-click benchmark: FP16 / BF16 / FP32 matmul, memory bandwidth, fill-and-verify VRAM, GPU-vs-CPU result check, model / SM count / VRAM identity check, retired pages and ECC errors, sustained power draw and throttling
- Built-in performance baseline database per model and power tier; verdicts are *normal / low / likely fake / broken*, with one-click blacklisting

**Web terminal and files**
- Tabbed terminal (xterm.js) showing the panel-to-instance SSH latency, with a one-click clear
- File browser: upload, download, edit text files in the browser, preview images and videos

**Startup downloader**
- Define one or more download lists (URL + target directory) in the settings; Hugging Face and Civitai are supported, tokens included
- aria2 downloads on the instance with multiple connections; each file shows progress and speed; existing files are skipped, interrupted ones resume, and files that will not fit are flagged
- Tick "start the downloader after boot" when renting and the download begins as soon as the machine is up
- ComfyUI-specific: syncs the `input` folder (your uploaded images and videos) to new instances, and pulls it back into a local backup before stop / recycle / destroy

**Browser notifications** (HTTPS required)
- Instance created, running, stopped, offline, destroyed
- Startup downloader finished or failed
- Balance below a threshold, or not enough to last N hours at the current burn rate
- Today's spend above a threshold

## Quick start

You need a Linux server with systemd (x86_64 or arm64) and a vast.ai API key ([cloud.vast.ai](https://cloud.vast.ai/) → Account → API Keys).

```bash
curl -fsSL https://raw.githubusercontent.com/FishPort-Dev-Team/vastmon/main/install.sh | sudo bash
```

The script downloads the binary for your architecture, verifies its SHA-256, creates a `vastmon` system user and a systemd service, and asks three questions: your API key, the listen address (default `0.0.0.0:8090`) and a panel password (press Enter to have one generated and printed). Then open `http://<server-ip>:8090` and sign in.

For an unattended install, pass the answers as environment variables:

```bash
curl -fsSL https://raw.githubusercontent.com/FishPort-Dev-Team/vastmon/main/install.sh \
  | sudo VAST_API_KEY=your-key VASTMON_PASSWORD='at-least-8-chars' VASTMON_ADDR=0.0.0.0:8090 bash
```

Pin a release with `VASTMON_VERSION=v0.1.0`.

## After installing

1. **Sign in.** Everything in the panel requires a login.
2. **Add an SSH private key** (设置 / Settings → SSH 私钥 / SSH key). The benchmark, terminal, file browser and startup downloader all log in to instances over SSH. Paste the key; it stays on the server. The page shows the matching public key and whether it is in your vast account — if it is not, new instances will not accept it.
3. **Set up your filters**, save them as a preset, and start browsing machines.
4. For browser notifications, or for long-term use over the internet, **put HTTPS in front** (next section) and enter that address under Settings → 站点 (Site).

## Serving over HTTPS (recommended)

The panel itself speaks plain HTTP. Browsers only allow notifications on HTTPS, and a panel that can rent machines and spend credit deserves encryption anyway. Any reverse proxy or CDN works; two things matter:

- **Allow WebSocket.** The terminal uses `/api/term/<id>/ws`. If it is blocked, the terminal tells you so: "SSH from the panel to the instance is fine, the browser-to-panel WebSocket cannot connect".
- **Do not cache `/api/`.** Static files are already served with `Cache-Control: no-store`, so upgrades are never hidden behind a stale cache.

An Nginx example:

```nginx
server {
    listen 443 ssl http2;
    server_name vastmon.example.com;
    # ssl_certificate / ssl_certificate_key ...

    client_max_body_size 0;            # large uploads in the file browser

    location / {
        proxy_pass http://127.0.0.1:8090;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_read_timeout 1d;          # long-lived terminal sessions
        proxy_buffering off;
    }
}
```

On Cloudflare, make sure Network → WebSockets is on. When the panel sits directly behind Cloudflare it takes the visitor address from `CF-Connecting-IP` — only for requests coming from Cloudflare's published ranges — so login rate limiting counts real client IPs.

## Day-to-day operation

| Task | Command |
|---|---|
| Status | `systemctl status vastmon` |
| Logs | `journalctl -u vastmon -f` |
| Restart | `systemctl restart vastmon` |
| Upgrade to the latest release | `curl -fsSL https://raw.githubusercontent.com/FishPort-Dev-Team/vastmon/main/install.sh \| sudo bash -s upgrade` |
| Reset a forgotten password | `curl -fsSL …/install.sh \| sudo bash -s passwd` |
| Uninstall, keep data | `curl -fsSL …/install.sh \| sudo bash -s uninstall` |
| Uninstall and delete data | `curl -fsSL …/install.sh \| sudo bash -s uninstall --purge` |

Signed-in users can also change the password under Settings → 面板密码 (Panel password); every other session is signed out.

**Where things live**

| Path | Contents |
|---|---|
| `/opt/vastmon/vastmon` | the binary |
| `/etc/vastmon/vastmon.env` | API key, listen address, extra flags (mode 600) |
| `/var/lib/vastmon/vastmon.db` | all data (SQLite) |
| `/var/lib/vastmon/backups/input.tar.gz` | ComfyUI input backup |

**Flags** (set them in `VASTMON_ARGS` in `vastmon.env`, then restart)

| Flag | Default | Meaning |
|---|---|---|
| `-addr` | `0.0.0.0:8090` | listen address (the installer manages it via `VASTMON_ADDR`) |
| `-interval` | `2m` | how often offers are polled |
| `-disk` | `96` | disk size in GB used for pricing, and the minimum free disk a listed machine must have |
| `-db` | `vastmon.db` | database file |
| `-key` | | API key file (by default the `VAST_API_KEY` environment variable is used) |
| `-api` | `https://console.vast.ai` | vast API base URL |

## Using the panel

### Machine list (机器)

Each row is *one machine at one GPU count*: an 8-GPU host lists 1-, 2- and 4-GPU offers at the same time, so with the GPU count filter left open you will see the same machine ID on several rows. That is expected.

- **$/h** already includes disk charges for `-disk` GB.
- **Est. $/day** = hourly price × 24 + daily download GB × download price + daily upload GB × upload price. Your expected daily traffic is set at the bottom of the filters.
- Bandwidth price colors: green < 8, orange 8–20, red ≥ 20 ($ per TB).
- A machine counts as online if it appeared in any of the last three polls. Tick 显示已下架 (show gone) to see vanished machines and their last price.

### Renting (开机)

Click 开机 (rent), pick a template (yours, vast's recommended ones, or a raw hash_id), a disk size and a label. The panel looks up the machine's latest offer and refuses to rent if the price went up since you confirmed. Two options in the dialog:

- **Turn off auto-tmux on login** — vast images drop every SSH login into tmux, which gets in the way of the web terminal; this switches it off once the instance is up.
- **Start the downloader after boot** — choose a download set and it runs as soon as the instance is reachable.

### Benchmark (算力检测)

The benchmark button on an instance card runs a PyTorch script on the instance over SSH; it takes about a minute. It tries `/opt/vastmon/bin/python`, `/venv/main/bin/python` and `python3` in turn and uses the first one with working CUDA, so the image needs PyTorch with CUDA (ComfyUI and PyTorch templates have it). Results are compared against the same model and power tier in Settings → 性能基准数据库 (baseline database); a result judged normal can be saved as a new baseline for future comparisons.

### Startup downloader (启动下载器)

Under Settings → 启动下载配置 (download configuration), create one or more lists. Each entry is a download URL plus a target directory, relative to the *working directory* (default `/workspace/ComfyUI`):

- Hugging Face `/blob/` links are rewritten to `/resolve/`; gated models need an HF token.
- For Civitai you can paste a model page, a version download link or just a model ID; most models need a Civitai token.
- The panel resolves the real download URL with your token first and hands only that to the instance, so tokens normally never reach the instance.

aria2 is installed with `apt-get` if the instance does not have it. *ComfyUI input sync* only makes sense for ComfyUI: on start it unpacks `backups/input.tar.gz` into `input/` under the working directory, and before stop / recycle / destroy it pulls the instance's `input/` back and merges it in (adding and updating, never deleting).

### Browser notifications (浏览器通知)

Open the panel at its HTTPS address, go to Settings → 浏览器通知 and click 在这个浏览器开启通知 (enable notifications in this browser). Each browser and device is enabled separately; after that notifications arrive even with the panel closed. Delivery goes through the browser vendor's push service: Chrome uses Google FCM (may be unreachable from mainland China), Edge uses Microsoft's and Firefox uses Mozilla's.

## Security notes

- **Your vast API key can rent machines and spend credit.** It lives only in `vastmon.env` on the server and is never sent to the browser.
- **`vastmon.db` is sensitive**: it holds the panel password hash, sessions, the SSH key passphrase (if set), HF / Civitai tokens and the VAPID private key used for push. Keep backups private.
- Sign-in is locked for 15 minutes after 5 failures from one IP or 30 in total; every write request must carry an `X-Requested-With` header to block cross-site form posts.
- Files served by the file browser carry `Content-Security-Policy: sandbox`, so HTML or SVG on an instance cannot run scripts on the panel's origin.
- The service runs as its own `vastmon` user with a read-only system, no privilege escalation and other systemd restrictions.

## Building from source

Go 1.26 or newer; no CGO needed (SQLite is pure Go).

```bash
git clone https://github.com/FishPort-Dev-Team/vastmon.git
cd vastmon
go build -trimpath -ldflags "-s -w -X main.version=$(git describe --tags --always)" -o vastmon .
./vastmon passwd                        # set the panel password first
VAST_API_KEY=your-key ./vastmon         # then open http://<server-ip>:8090
```

The front end is plain JavaScript modules embedded in the binary; there is no build step.

## FAQ

**The terminal keeps saying "connection lost".** Read the message: if it says SSH from the panel is fine, your reverse proxy or CDN is blocking WebSocket; if it shows an SSH error, check under Settings → SSH key whether the public key is in your vast account.

**I cannot enable notifications.** The panel must be opened over HTTPS. Also check that notifications are not blocked in the browser's site settings.

**The machine list is empty.** The first poll takes up to two minutes after start-up (or click 立即抓取, *poll now*). Otherwise your filters may be too strict, or nobody is renting out the selected models right now.

## License

[MIT](LICENSE). The bundled [xterm.js](https://github.com/xtermjs/xterm.js) used by the web terminal is also MIT-licensed; its license is in `web/vendor/xterm/`.
