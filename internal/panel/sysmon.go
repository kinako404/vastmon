package panel

import (
	"bufio"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 系统监控（终端页左边那一栏）：在实例上起一个 sh 循环，每 2 秒把 /proc 和 cgroup 里的
// 原始数字打出来，面板解析、和上一帧相减得到 CPU、网速、各进程的 CPU。浏览器每 2 秒来取
// 一次（GET /api/instances/{id}/monitor）；20 秒没人取就把实例上的循环停掉。
//
// vast 的实例是容器：/proc/stat、/proc/meminfo 是整台宿主机的，所以 CPU 和内存优先用
// cgroup v2（cpu.stat / cpu.max、memory.current / memory.max），没有才退回 /proc。
// 负载（/proc/loadavg）拿不到容器自己的，显示的是宿主机的。
//
// GPU（监控栏底下的开关）：有浏览器带着 gpu=1 来取时，循环里多跑两次 nvidia-smi（各几十毫秒）：
// 每张卡的占用 / 显存 / 温度 / 功耗 / 风扇，和 pmon 给的各进程占用。要不要采 GPU 变了就把
// 会话重开一次。不直接用 nvtop：Ubuntu 24.04 源里的 nvtop 3.0.2 只有全屏界面，没有输出数据的
// 选项；两者读的都是 NVML。

const (
	monKeep  = 300 // 留多少个采样点（每 2 秒一个，10 分钟）
	monIdle  = 20 * time.Second
	monRetry = 10 * time.Second // 断了之后隔多久重连；连着失败就翻倍，最多 80 秒
	monProcs = 12               // CPU 最高和内存最高的进程各取多少个
)

// monScript 在实例上循环输出。每一帧：@@frame <unix 秒>，若干 @@<段名> 加原始内容，@@end。
// 第一帧和第二帧之间只等 1 秒，页面能早点出数。df 每 5 帧一次，只留挂在目录上的
// （容器里 /etc/hosts、nvidia-smi 这些单文件挂载不算磁盘）。
const monScript = `export LC_ALL=C
cg=/sys/fs/cgroup
t=
command -v timeout >/dev/null 2>&1 && t="timeout 5"
echo "@@init $(getconf CLK_TCK 2>/dev/null || echo 100) $(getconf PAGESIZE 2>/dev/null || echo 4096) $(nproc 2>/dev/null || echo 1) $(stat -c %Y /proc/1 2>/dev/null || echo 0)"
n=0
while :; do
  echo "@@frame $(date +%s)" || exit 0
  echo "@@uptime"; cat /proc/uptime
  echo "@@loadavg"; cat /proc/loadavg
  echo "@@stat"; head -n 1 /proc/stat
  echo "@@meminfo"; grep -E '^(MemTotal|MemAvailable|SwapTotal|SwapFree):' /proc/meminfo
  for f in cpu.max cpu.stat memory.max memory.current memory.stat memory.swap.max memory.swap.current; do
    [ -r $cg/$f ] && { echo "@@cg $f"; cat $cg/$f; }
  done
  echo "@@net"; cat /proc/net/dev
  echo "@@procs"; cat /proc/[0-9]*/stat 2>/dev/null
  if [ -n "$gpu" ]; then
    echo "@@gpu"
    if command -v nvidia-smi >/dev/null 2>&1; then
      $t nvidia-smi --query-gpu=index,name,utilization.gpu,memory.used,memory.total,temperature.gpu,power.draw,power.limit,fan.speed --format=csv,noheader,nounits 2>&1
      echo "@@gpuproc"; $t nvidia-smi pmon -c 1 -s um 2>/dev/null
    else
      echo "这台实例上没有 nvidia-smi"
    fi
  fi
  if [ $((n % 5)) -eq 0 ]; then
    echo "@@df"
    $t df -P -k 2>/dev/null | tail -n +2 | while read -r fs blocks used avail cap mnt; do
      [ -d "$mnt" ] && echo "$blocks $avail $fs $mnt"
    done
  fi
  echo "@@end"
  [ $n -eq 0 ] && sleep 1 || sleep 2
  n=$((n + 1))
done
`

type monProc struct {
	PID  int     `json:"pid"`
	Name string  `json:"name"`
	CPU  float64 `json:"cpu"` // 占一个核的百分比（和 top 一样，多线程可以超过 100）
	Mem  int64   `json:"mem"` // 常驻内存，字节
}

type monDisk struct {
	Path  string `json:"path"`
	FS    string `json:"fs"`
	Size  int64  `json:"size"`
	Avail int64  `json:"avail"`
}

// monGPU 是一张卡；读不到的项（比如没有风扇的服务器卡）是 null。
type monGPU struct {
	Index      int      `json:"index"`
	Name       string   `json:"name"`
	Util       *float64 `json:"util"`
	MemUsed    int64    `json:"mem_used"`
	MemTotal   int64    `json:"mem_total"`
	Temp       *float64 `json:"temp"`
	Power      *float64 `json:"power"`
	PowerLimit *float64 `json:"power_limit"`
	Fan        *float64 `json:"fan"`
}

type monGPUProc struct {
	GPU  int      `json:"gpu"`
	PID  int      `json:"pid"`
	Type string   `json:"type"` // C 计算 / G 图形
	Name string   `json:"name"`
	Util *float64 `json:"util"` // 占这张卡的百分比，没在算时是 null
	Mem  int64    `json:"mem"`  // 显存，字节
	CPU  float64  `json:"cpu"`
}

type monGPUInfo struct {
	GPUs  []monGPU     `json:"gpus"`
	Procs []monGPUProc `json:"procs"`
	Error string       `json:"error"`
}

type monUsage struct {
	Used  int64 `json:"used"`
	Total int64 `json:"total"`
}

type monSnapshot struct {
	Time   int64      `json:"time"`
	Uptime int64      `json:"uptime"` // 容器（1 号进程）跑了多少秒
	Load   [3]float64 `json:"load"`
	// Cores 是 CPU 百分比的分母：容器的 CPU 配额，没有配额就是能看到的核数。
	Cores float64 `json:"cores"`
	// Quota 表示 CPU / 内存是按容器配额（cgroup）算的，不是整台机器。
	Quota  bool      `json:"quota"`
	CPU    float64   `json:"cpu"`
	Mem    monUsage  `json:"mem"`
	Swap   monUsage  `json:"swap"`
	Procs  []monProc `json:"procs"`
	NProcs int       `json:"nprocs"`
	Disks  []monDisk `json:"disks"`
	Ifaces []string  `json:"ifaces"`
	// GPU 只在有人要（gpu=1）时才有。
	GPU *monGPUInfo `json:"gpu"`
}

type monSample struct {
	Seq  int64   `json:"seq"`
	Time int64   `json:"time"`
	CPU  float64 `json:"cpu"`
	// Lat 是面板机到实例的 SSH 往返延迟（毫秒），没测到是 -1。
	Lat float64 `json:"lat"`
	// Net 是每个网卡的 [下行, 上行]，字节 / 秒。
	Net map[string][2]float64 `json:"net"`
	// GPU 是每张卡的 [占用 %, 显存 %]，读不到是 -1；没采 GPU 时没有这一项。
	GPU [][2]float64 `json:"gpu,omitempty"`
}

type sysMon struct {
	id    int64
	epoch int64
	// lastPoll、lastGPU（最近一次有人要 GPU 数据）由 monManager.mu 保护。
	lastPoll time.Time
	lastGPU  time.Time

	mu      sync.Mutex
	seq     int64
	samples []monSample
	snap    *monSnapshot
	err     string
}

type monManager struct {
	mu   sync.Mutex
	mons map[int64]*sysMon
}

// sysMonitor 返回实例的最新状态和 after 之后的采样点；第一次有人取时把监控起起来。
// epoch 变了说明监控重新起过，浏览器手里的序号作废，整段历史重发。
func (s *Server) sysMonitor(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	epoch, _ := strconv.ParseInt(q.Get("epoch"), 10, 64)
	after, _ := strconv.ParseInt(q.Get("after"), 10, 64)

	s.sm.mu.Lock()
	if s.sm.mons == nil {
		s.sm.mons = map[int64]*sysMon{}
	}
	m := s.sm.mons[id]
	if m == nil {
		m = &sysMon{id: id, epoch: time.Now().UnixMilli()}
		s.sm.mons[id] = m
		go s.runSysMon(m)
	}
	m.lastPoll = time.Now()
	if q.Get("gpu") == "1" {
		m.lastGPU = m.lastPoll
	}
	s.sm.mu.Unlock()

	m.mu.Lock()
	defer m.mu.Unlock()
	if epoch != m.epoch {
		after = 0
	}
	samples := []monSample{}
	for _, sm := range m.samples {
		if sm.Seq > after {
			samples = append(samples, sm)
		}
	}
	writeJSON(w, map[string]any{"epoch": m.epoch, "now": m.snap, "samples": samples, "error": m.err, "keep": monKeep})
}

// monIsIdle 报告是不是已经没有浏览器在看；remove 为真时顺手把它从表里拿掉。
func (s *Server) monIsIdle(m *sysMon, remove bool) bool {
	s.sm.mu.Lock()
	defer s.sm.mu.Unlock()
	idle := time.Since(m.lastPoll) > monIdle
	if idle && remove && s.sm.mons[m.id] == m {
		delete(s.sm.mons, m.id)
	}
	return idle
}

// monWantsGPU 报告最近有没有浏览器要 GPU 数据。
func (s *Server) monWantsGPU(m *sysMon) bool {
	s.sm.mu.Lock()
	defer s.sm.mu.Unlock()
	return time.Since(m.lastGPU) <= monIdle
}

// errMonRestart：要不要采 GPU 变了，会话是自己关掉准备重开的，不算出错。
var errMonRestart = errors.New("restart")

func (s *Server) runSysMon(m *sysMon) {
	fails := 0
	for {
		start := time.Now()
		err := s.sysMonSession(m)
		if s.monIsIdle(m, true) {
			return
		}
		if errors.Is(err, errMonRestart) {
			continue
		}
		m.mu.Lock()
		m.err = errString(err)
		m.snap = nil
		m.mu.Unlock()
		// 实例停了 / 销毁了的时候每次重连都要问一遍 vast，别问得太勤。
		if time.Since(start) > time.Minute {
			fails = 0
		}
		time.Sleep(monRetry << min(fails, 3))
		fails++
		if s.monIsIdle(m, true) {
			return
		}
	}
}

// tailBuffer 只留最后一小段标准错误，出错时带给页面。
type tailBuffer struct {
	mu sync.Mutex
	b  []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > 2048 {
		t.b = t.b[len(t.b)-2048:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.b))
}

// sysMonSession 在实例上跑监控循环，一直读到它结束（没人看了、SSH 断了、脚本退出）。
func (s *Server) sysMonSession(m *sysMon) error {
	sess, p, err := s.session(m.id)
	if err != nil {
		return err
	}
	defer sess.Close()
	// 和终端一样算一个使用者，连接不会因为“空闲 15 分钟”被关掉。
	s.pool.mu.Lock()
	p.users++
	s.pool.mu.Unlock()
	defer func() {
		s.pool.mu.Lock()
		p.users--
		p.last = time.Now()
		s.pool.mu.Unlock()
	}()

	stdout, err := sess.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr tailBuffer
	sess.Stderr = &stderr
	gpu := s.monWantsGPU(m)
	script := monScript
	if gpu {
		script = "gpu=1\n" + script
	}
	if err := sess.Start("exec sh -c " + shellQuote(script)); err != nil {
		return err
	}
	done := make(chan struct{})
	defer close(done)

	// 没人看了就关会话；实例上的循环下一次输出时收到 SIGPIPE 退出。
	// 要不要采 GPU 变了也关，外面马上重开。
	var restart atomic.Bool
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if s.monIsIdle(m, false) {
					sess.Close()
					return
				}
				if s.monWantsGPU(m) != gpu {
					restart.Store(true)
					sess.Close()
					return
				}
			}
		}
	}()

	// 面板机到实例的 SSH 往返延迟，用保活请求量。
	var latMicros atomic.Int64
	latMicros.Store(-1)
	go func() {
		for {
			start := time.Now()
			if _, _, err := p.client.SendRequest("keepalive@openssh.com", true, nil); err != nil {
				latMicros.Store(-1)
				return
			}
			latMicros.Store(time.Since(start).Microseconds())
			select {
			case <-done:
				return
			case <-time.After(2 * time.Second):
			}
		}
	}()

	ps := monParser{clk: 100, page: 4096, nproc: 1}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		snap, sample := ps.line(sc.Text())
		if snap == nil {
			continue
		}
		sample.Lat = -1
		if us := latMicros.Load(); us >= 0 {
			sample.Lat = float64(us) / 1000
		}
		m.mu.Lock()
		m.seq++
		sample.Seq = m.seq
		m.samples = append(m.samples, *sample)
		if len(m.samples) > monKeep {
			m.samples = append([]monSample(nil), m.samples[len(m.samples)-monKeep:]...)
		}
		m.snap = snap
		m.err = ""
		m.mu.Unlock()
	}
	if restart.Load() {
		return errMonRestart
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if msg := stderr.String(); msg != "" {
		return errors.New("实例上的监控脚本退出了：" + firstLine(msg))
	}
	return errors.New("实例上的监控脚本退出了（SSH 断开或进程被杀），正在重连")
}

// monFrame 是一帧里的原始数字。
type monFrame struct {
	time      int64
	up        float64 // /proc/uptime 第一个数，只用来算两帧间隔
	load      [3]float64
	statBusy  float64 // /proc/stat：非空闲的时间片
	statTotal float64
	meminfo   map[string]int64 // kB
	cg        map[string]string
	net       map[string][2]uint64 // 网卡 -> [收, 发] 字节
	procs     map[int]monRawProc
	disks     []monDisk
	hasDF     bool
	gpu       *monGPUInfo
	pmonCols  map[string]int // nvidia-smi pmon 表头里各列的位置
}

type monRawProc struct {
	name  string
	ticks uint64
	rss   int64 // 页
}

type monParser struct {
	clk, page float64
	nproc     float64
	boot      int64 // 1 号进程的启动时间
	section   string
	cur, prev *monFrame
	disks     []monDisk
}

// line 喂一行输出；凑够一帧并且有上一帧可以相减时返回快照和采样点。
func (ps *monParser) line(line string) (*monSnapshot, *monSample) {
	if !strings.HasPrefix(line, "@@") {
		if ps.cur != nil {
			ps.cur.add(ps.section, line)
		}
		return nil, nil
	}
	f := strings.Fields(line[2:])
	if len(f) == 0 {
		return nil, nil
	}
	switch f[0] {
	case "init":
		if len(f) >= 5 {
			ps.clk = positive(atof(f[1]), 100)
			ps.page = positive(atof(f[2]), 4096)
			ps.nproc = positive(atof(f[3]), 1)
			ps.boot, _ = strconv.ParseInt(f[4], 10, 64)
		}
	case "frame":
		ps.cur = &monFrame{meminfo: map[string]int64{}, cg: map[string]string{}, net: map[string][2]uint64{}, procs: map[int]monRawProc{}}
		if len(f) >= 2 {
			ps.cur.time, _ = strconv.ParseInt(f[1], 10, 64)
		}
		ps.section = ""
	case "cg":
		ps.section = "cg"
		if len(f) >= 2 {
			ps.section = "cg " + f[1]
		}
	case "df":
		ps.section = "df"
		if ps.cur != nil {
			ps.cur.hasDF = true
		}
	case "gpu":
		ps.section = "gpu"
		if ps.cur != nil {
			ps.cur.gpu = &monGPUInfo{GPUs: []monGPU{}, Procs: []monGPUProc{}}
		}
	case "end":
		cur := ps.cur
		ps.cur = nil
		if cur == nil {
			return nil, nil
		}
		if cur.hasDF {
			ps.disks = dedupeDisks(cur.disks)
		}
		prev := ps.prev
		ps.prev = cur
		if prev == nil || cur.up <= prev.up {
			return nil, nil
		}
		return ps.diff(prev, cur)
	default:
		ps.section = f[0]
	}
	return nil, nil
}

func atof(s string) float64 {
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

func positive(v, def float64) float64 {
	if v > 0 {
		return v
	}
	return def
}

// smiNum 读 nvidia-smi 的一个数；[N/A]、[Not Supported]、- 这些读不到的是 nil。
func smiNum(v string) *float64 {
	n, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		return nil
	}
	return &n
}

func smiMiB(v string) int64 {
	if n := smiNum(v); n != nil {
		return int64(*n) << 20
	}
	return 0
}

// addGPU 解析 --query-gpu 的一行：序号, 型号, 占用, 已用显存, 总显存, 温度, 功耗, 功耗上限, 风扇。
// 不是这个样子的行（nvidia-smi 报错）记成错误。
func (f *monFrame) addGPU(line string) {
	p := strings.Split(line, ", ")
	idx, err := strconv.Atoi(strings.TrimSpace(p[0]))
	if len(p) != 9 || err != nil {
		if f.gpu.Error == "" {
			f.gpu.Error = strings.TrimSpace(line)
		}
		return
	}
	f.gpu.GPUs = append(f.gpu.GPUs, monGPU{Index: idx, Name: strings.TrimSpace(p[1]), Util: smiNum(p[2]),
		MemUsed: smiMiB(p[3]), MemTotal: smiMiB(p[4]), Temp: smiNum(p[5]), Power: smiNum(p[6]), PowerLimit: smiNum(p[7]), Fan: smiNum(p[8])})
}

// addGPUProc 解析 nvidia-smi pmon 的输出。各版本驱动的列不一样多，按第一行表头
// （# gpu pid type sm mem enc dec … fb … command）找列。
func (f *monFrame) addGPUProc(fs []string) {
	if fs[0] == "#" {
		if f.pmonCols == nil {
			f.pmonCols = map[string]int{}
			for i, name := range fs[1:] {
				f.pmonCols[name] = i
			}
		}
		return
	}
	col := func(name string) string {
		if i, ok := f.pmonCols[name]; ok && i < len(fs) {
			return fs[i]
		}
		return ""
	}
	pid, err := strconv.Atoi(col("pid"))
	if err != nil { // 没有进程时 pmon 输出一行全是 -
		return
	}
	gpu, _ := strconv.Atoi(col("gpu"))
	name := ""
	if i, ok := f.pmonCols["command"]; ok && i < len(fs) {
		name = strings.Join(fs[i:], " ")
	}
	f.gpu.Procs = append(f.gpu.Procs, monGPUProc{GPU: gpu, PID: pid, Type: col("type"), Name: name, Util: smiNum(col("sm")), Mem: smiMiB(col("fb"))})
}

func (f *monFrame) add(section, line string) {
	fs := strings.Fields(line)
	if len(fs) == 0 {
		return
	}
	switch {
	case section == "gpu":
		f.addGPU(line)
	case section == "gpuproc":
		f.addGPUProc(fs)
	case section == "uptime":
		f.up = atof(fs[0])
	case section == "loadavg":
		for i := 0; i < 3 && i < len(fs); i++ {
			f.load[i] = atof(fs[i])
		}
	case section == "stat":
		// cpu user nice system idle iowait irq softirq steal ...
		if fs[0] != "cpu" {
			return
		}
		for i, v := range fs[1:] {
			if i >= 8 {
				break
			}
			n := atof(v)
			f.statTotal += n
			if i != 3 && i != 4 {
				f.statBusy += n
			}
		}
	case section == "meminfo":
		if len(fs) >= 2 {
			f.meminfo[strings.TrimSuffix(fs[0], ":")], _ = strconv.ParseInt(fs[1], 10, 64)
		}
	case strings.HasPrefix(section, "cg "):
		name := section[3:]
		switch name {
		case "cpu.stat":
			if fs[0] == "usage_usec" && len(fs) >= 2 {
				f.cg["cpu.usage_usec"] = fs[1]
			}
		case "memory.stat":
			if fs[0] == "inactive_file" && len(fs) >= 2 {
				f.cg["memory.inactive_file"] = fs[1]
			}
		default:
			f.cg[name] = strings.Join(fs, " ")
		}
	case section == "net":
		// 网卡名: 收字节 包 … (8 列) 发字节 …；网卡名和数字之间可能没有空格
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			return
		}
		v := strings.Fields(rest)
		if len(v) < 9 {
			return
		}
		rx, err1 := strconv.ParseUint(v[0], 10, 64)
		tx, err2 := strconv.ParseUint(v[8], 10, 64)
		if err1 == nil && err2 == nil {
			f.net[strings.TrimSpace(name)] = [2]uint64{rx, tx}
		}
	case section == "procs":
		// pid (名字，可能有空格和括号) 状态 … 第 14、15 列是用户态 / 内核态时间片，第 24 列是常驻页数
		l, r := strings.IndexByte(line, '('), strings.LastIndexByte(line, ')')
		if l < 0 || r < l {
			return
		}
		pid, err := strconv.Atoi(strings.TrimSpace(line[:l]))
		rest := strings.Fields(line[r+1:])
		if err != nil || len(rest) < 22 {
			return
		}
		ut, _ := strconv.ParseUint(rest[11], 10, 64)
		st, _ := strconv.ParseUint(rest[12], 10, 64)
		rss, _ := strconv.ParseInt(rest[21], 10, 64)
		f.procs[pid] = monRawProc{name: line[l+1 : r], ticks: ut + st, rss: rss}
	case section == "df":
		// 我们自己拼的：总 kB、可用 kB、文件系统、挂载点（可能有空格）
		p := strings.SplitN(line, " ", 4)
		if len(p) < 4 {
			return
		}
		size, err1 := strconv.ParseInt(p[0], 10, 64)
		avail, err2 := strconv.ParseInt(p[1], 10, 64)
		if err1 != nil || err2 != nil {
			return
		}
		f.disks = append(f.disks, monDisk{Path: p[3], FS: p[2], Size: size << 10, Avail: avail << 10})
	}
}

// dedupeDisks 去掉不到 1MB 的、/proc 和 /sys 下面的，以及同一个文件系统的重复挂载
// （Docker 的 overlay 会把同一块盘列很多遍），留最先出现的那个。
func dedupeDisks(in []monDisk) []monDisk {
	out := []monDisk{}
	seen := map[string]bool{}
	for _, d := range in {
		if d.Size < 1<<20 || strings.HasPrefix(d.Path, "/proc") || strings.HasPrefix(d.Path, "/sys") {
			continue
		}
		key := d.FS + "|" + strconv.FormatInt(d.Size, 10) + "|" + strconv.FormatInt(d.Avail, 10)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, d)
	}
	return out
}

// cgInt 读 cgroup 里的一个数；文件没有或者写着 max（不限）时 ok 为假。
func (f *monFrame) cgInt(name string) (int64, bool) {
	v, ok := f.cg[name]
	if !ok || v == "" || v == "max" {
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	return n, err == nil
}

func clampPct(v float64) float64 {
	return max(0, min(100, v))
}

func (ps *monParser) diff(prev, cur *monFrame) (*monSnapshot, *monSample) {
	dt := cur.up - prev.up
	snap := &monSnapshot{Time: cur.time, Load: cur.load, Cores: ps.nproc, NProcs: len(cur.procs), Disks: ps.disks, Procs: []monProc{}, Ifaces: []string{}}
	if ps.boot > 0 && cur.time > ps.boot {
		snap.Uptime = cur.time - ps.boot
	}

	// CPU：容器配额 cpu.max 是“配额 周期”（微秒），没限制时第一个数是 max。
	if q := strings.Fields(cur.cg["cpu.max"]); len(q) == 2 && q[0] != "max" {
		if quota, period := atof(q[0]), atof(q[1]); quota > 0 && period > 0 {
			snap.Cores = quota / period
			snap.Quota = true
		}
	}
	a, okA := prev.cgInt("cpu.usage_usec")
	b, okB := cur.cgInt("cpu.usage_usec")
	switch {
	case okA && okB && b >= a:
		snap.CPU = clampPct(float64(b-a) / (dt * 1e6 * snap.Cores) * 100)
	case cur.statTotal > prev.statTotal:
		snap.CPU = clampPct((cur.statBusy - prev.statBusy) / (cur.statTotal - prev.statTotal) * 100)
		snap.Cores = ps.nproc
	}

	// 内存：容器有上限时用 cgroup 的；已用里不算随时能回收的文件缓存（和 docker stats 一样）。
	limit, okL := cur.cgInt("memory.max")
	used, okU := cur.cgInt("memory.current")
	if okL && okU {
		inactive, _ := cur.cgInt("memory.inactive_file")
		snap.Mem = monUsage{Used: max(0, used-inactive), Total: limit}
		snap.Quota = true
	} else {
		total := cur.meminfo["MemTotal"] << 10
		snap.Mem = monUsage{Used: max(0, total-cur.meminfo["MemAvailable"]<<10), Total: total}
	}
	if _, has := cur.cg["memory.swap.max"]; has {
		// 写着 max 时不限，容器实际能用到的是宿主机的交换分区
		if lim, ok := cur.cgInt("memory.swap.max"); ok {
			snap.Swap.Total = lim
		} else {
			snap.Swap.Total = cur.meminfo["SwapTotal"] << 10
		}
		snap.Swap.Used, _ = cur.cgInt("memory.swap.current")
	} else {
		total := cur.meminfo["SwapTotal"] << 10
		snap.Swap = monUsage{Used: max(0, total-cur.meminfo["SwapFree"]<<10), Total: total}
	}

	// 进程：CPU 最高的和内存最高的各取几个，页面上两种排序都能用。
	all := make([]monProc, 0, len(cur.procs))
	for pid, p := range cur.procs {
		ticks := p.ticks
		if old, ok := prev.procs[pid]; ok && old.name == p.name && p.ticks >= old.ticks {
			ticks -= old.ticks
		}
		all = append(all, monProc{PID: pid, Name: p.name, CPU: float64(ticks) / ps.clk / dt * 100, Mem: p.rss * int64(ps.page)})
	}
	pick := map[int]bool{}
	take := func(key func(p monProc) float64) {
		sort.Slice(all, func(i, j int) bool {
			if a, b := key(all[i]), key(all[j]); a != b {
				return a > b
			}
			return all[i].PID < all[j].PID
		})
		for i := 0; i < len(all) && i < monProcs; i++ {
			if !pick[all[i].PID] {
				pick[all[i].PID] = true
				snap.Procs = append(snap.Procs, all[i])
			}
		}
	}
	take(func(p monProc) float64 { return p.CPU })
	take(func(p monProc) float64 { return float64(p.Mem) })

	sample := &monSample{Time: cur.time, CPU: snap.CPU, Net: map[string][2]float64{}}
	if g := cur.gpu; g != nil {
		snap.GPU = g
		if len(g.GPUs) == 0 && g.Error == "" {
			g.Error = "nvidia-smi 没有列出显卡"
		}
		// 进程的 CPU 占用（和读不到名字时的名字）从上面的进程表里按 PID 补。
		byPID := make(map[int]monProc, len(all))
		for _, p := range all {
			byPID[p.PID] = p
		}
		for i := range g.Procs {
			if p, ok := byPID[g.Procs[i].PID]; ok {
				g.Procs[i].CPU = p.CPU
				if g.Procs[i].Name == "" || g.Procs[i].Name == "-" {
					g.Procs[i].Name = p.Name
				}
			}
		}
		sort.SliceStable(g.Procs, func(i, j int) bool { return g.Procs[i].Mem > g.Procs[j].Mem })
		for _, c := range g.GPUs {
			pt := [2]float64{-1, -1}
			if c.Util != nil {
				pt[0] = *c.Util
			}
			if c.MemTotal > 0 {
				pt[1] = float64(c.MemUsed) / float64(c.MemTotal) * 100
			}
			sample.GPU = append(sample.GPU, pt)
		}
	}
	for name, c := range cur.net {
		snap.Ifaces = append(snap.Ifaces, name)
		rate := [2]float64{}
		if o, ok := prev.net[name]; ok {
			for i := 0; i < 2; i++ {
				if c[i] >= o[i] {
					rate[i] = float64(c[i]-o[i]) / dt
				}
			}
		}
		sample.Net[name] = rate
	}
	sort.Strings(snap.Ifaces)
	return snap, sample
}
