package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// 启动下载器：按配置在实例上用 aria2 多线程下载文件，可先把 input 备份推上去。
// aria2 在实例上以 setsid 后台跑，面板每 2 秒经 SSH 看一次进度；面板重启后
// 从数据库里接着看（实例上的 aria2 不受影响）。每个任务一行 download_jobs，
// 进度整个存成 JSON。

const downloadsSchema = `
CREATE TABLE IF NOT EXISTS download_jobs (
	id INTEGER PRIMARY KEY AUTOINCREMENT, instance_id INTEGER, created INTEGER, status TEXT, data TEXT
);
CREATE INDEX IF NOT EXISTS download_jobs_inst ON download_jobs(instance_id, id);
`

const (
	dlPoll        = 2 * time.Second
	dlWaitEvery   = 15 * time.Second
	dlWaitGiveUp  = 3 * time.Hour
	dlMaxAttempts = 5
	dlSSHGiveUp   = 10 * time.Minute
	dlRemoteDir   = "/tmp/vastmon-dl"
	dlSpaceMargin = 512 << 20
)

// 文件状态
const (
	fQueued   = "queued"
	fActive   = "downloading"
	fDone     = "done"
	fSkipped  = "exists"
	fFailed   = "failed"
	fNoSpace  = "nospace"
	fCanceled = "canceled"
)

type dlFile struct {
	ItemID   int     `json:"item_id"`
	URL      string  `json:"url"`
	Dir      string  `json:"dir"`
	Name     string  `json:"name"`
	Size     int64   `json:"size"`
	Done     int64   `json:"done"`
	Speed    float64 `json:"speed"`
	Status   string  `json:"status"`
	Err      string  `json:"err"`
	Attempts int     `json:"attempts"`
	Started  int64   `json:"started"`
	Finished int64   `json:"finished"`
}

type dlInput struct {
	Status string `json:"status"` // ""（不同步）/ queued / running / done / failed
	Err    string `json:"err"`
	Done   int64  `json:"done"`
	Total  int64  `json:"total"`
	Files  int    `json:"files"`
}

type dlJob struct {
	ID          int64    `json:"id"`
	InstanceID  int64    `json:"instance_id"`
	Created     int64    `json:"created"`
	Started     int64    `json:"started"`
	Finished    int64    `json:"finished"`
	Status      string   `json:"status"` // waiting / running / done / partial / failed / canceled
	Phase       string   `json:"phase"`
	Auto        bool     `json:"auto"`
	Profile     string   `json:"profile"`
	Root        string   `json:"root"`
	Connections int      `json:"connections"`
	Input       dlInput  `json:"input"`
	Files       []dlFile `json:"files"`
	Err         string   `json:"err"`
}

func (j *dlJob) active() bool { return j.Status == "waiting" || j.Status == "running" }

type dlManager struct {
	mu       sync.Mutex
	base     context.Context
	jobs     map[int64]*dlJob
	cancels  map[int64]context.CancelFunc
	userStop map[int64]bool
}

// ---------------------------------------------------------------- 存储

func (s *Server) saveJob(j *dlJob) {
	s.dl.mu.Lock()
	b, err := json.Marshal(j)
	status := j.Status
	s.dl.mu.Unlock()
	if err == nil {
		_, err = s.st.db.Exec(`UPDATE download_jobs SET status = ?, data = ? WHERE id = ?`, status, string(b), j.ID)
	}
	if err != nil {
		log.Printf("保存下载任务 %d 失败: %v", j.ID, err)
	}
}

func (s *Server) loadJobs(q string, args ...any) ([]*dlJob, error) {
	rs, err := s.st.db.Query(`SELECT data FROM download_jobs `+q, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []*dlJob
	for rs.Next() {
		var d string
		if err := rs.Scan(&d); err != nil {
			return nil, err
		}
		j := &dlJob{}
		if err := json.Unmarshal([]byte(d), j); err == nil {
			out = append(out, j)
		}
	}
	return out, rs.Err()
}

// createJob 建任务：用第 profileID 套配置（0 = 默认那套），itemIDs 为空就用里面勾了“启用”的文件。
func (s *Server) createJob(instanceID int64, profileID int, itemIDs []int, syncInput, auto bool) (*dlJob, error) {
	c, err := s.dlConfig()
	if err != nil {
		return nil, err
	}
	want := map[int]bool{}
	for _, id := range itemIDs {
		want[id] = true
	}
	prof := c.profile(profileID)
	j := &dlJob{InstanceID: instanceID, Created: time.Now().Unix(), Status: "waiting", Auto: auto, Profile: prof.Name,
		Root: c.Root, Connections: c.Connections, Files: []dlFile{}}
	for _, it := range prof.Items {
		if (len(want) == 0 && it.Enabled) || want[it.ID] {
			j.Files = append(j.Files, dlFile{ItemID: it.ID, URL: it.URL, Dir: it.Dir, Name: it.Name, Size: it.Size, Status: fQueued})
		}
	}
	if syncInput {
		j.Input.Status = "queued"
	}
	if len(j.Files) == 0 && !syncInput {
		return nil, errors.New("没有要下载的文件：到“设置 → 启动下载配置”里添加或勾选")
	}
	s.dl.mu.Lock()
	defer s.dl.mu.Unlock()
	for _, x := range s.dl.jobs {
		if x.InstanceID == instanceID && x.active() {
			return nil, fmt.Errorf("实例 %d 已经有一个下载任务在跑（任务 %d）", instanceID, x.ID)
		}
	}
	res, err := s.st.db.Exec(`INSERT INTO download_jobs (instance_id, created, status, data) VALUES (?, ?, ?, '{}')`, instanceID, j.Created, j.Status)
	if err != nil {
		return nil, err
	}
	j.ID, _ = res.LastInsertId()
	s.dl.jobs[j.ID] = j
	return j, nil
}

// ---------------------------------------------------------------- 调度

// downloadInit 在开始监听前调用：记下根 context，接着跑面板重启前没跑完的任务。
func (s *Server) downloadInit(ctx context.Context) {
	s.dl.base = ctx
	s.dl.jobs = map[int64]*dlJob{}
	s.dl.cancels = map[int64]context.CancelFunc{}
	s.dl.userStop = map[int64]bool{}
	jobs, err := s.loadJobs(`WHERE status IN ('waiting', 'running') ORDER BY id`)
	if err != nil {
		log.Printf("读取下载任务失败: %v", err)
		return
	}
	for _, j := range jobs {
		s.dl.jobs[j.ID] = j
		if j.Status == "running" {
			log.Printf("接着跑下载任务 %d（实例 %d）", j.ID, j.InstanceID)
			s.startJob(j)
		}
	}
}

func (s *Server) startJob(j *dlJob) {
	ctx, cancel := context.WithCancel(s.dl.base)
	s.dl.mu.Lock()
	s.dl.cancels[j.ID] = cancel
	j.Status = "running"
	if j.Started == 0 {
		j.Started = time.Now().Unix()
	}
	s.dl.mu.Unlock()
	s.saveJob(j)
	go s.runJob(ctx, j)
}

// downloadLoop 等开机后自动下载的任务的实例跑起来、SSH 连得上，再开始。
func (s *Server) downloadLoop(ctx context.Context) {
	t := time.NewTicker(dlWaitEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.dl.mu.Lock()
		var waiting []*dlJob
		for _, j := range s.dl.jobs {
			if j.Status == "waiting" {
				waiting = append(waiting, j)
			}
		}
		s.dl.mu.Unlock()
		if len(waiting) == 0 {
			continue
		}
		ins, err := s.cl.Instances()
		if err != nil {
			continue
		}
		status := map[int64]string{}
		for _, x := range ins {
			status[int64(num(x["id"]))] = str(x["actual_status"])
		}
		for _, j := range waiting {
			st, alive := status[j.InstanceID]
			switch {
			case !alive:
				s.finishJob(j, "failed", "实例已经不在了")
				continue
			case time.Since(time.Unix(j.Created, 0)) > dlWaitGiveUp:
				s.finishJob(j, "failed", "等了 3 小时实例还没跑起来")
				continue
			case st != "running":
				s.setPhase(j, "等实例跑起来（"+st+"）")
				continue
			}
			rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			out, _, err := s.runOn(rctx, j.InstanceID, "echo VASTMON_OK", nil)
			cancel()
			if err != nil || !containsOK(out) {
				s.setPhase(j, "等 SSH 连得上："+firstLine(errString(err)))
				continue
			}
			log.Printf("实例 %d 跑起来了，开始下载任务 %d", j.InstanceID, j.ID)
			s.startJob(j)
		}
	}
}

func (s *Server) setPhase(j *dlJob, p string) {
	s.dl.mu.Lock()
	changed := j.Phase != p
	j.Phase = p
	s.dl.mu.Unlock()
	if changed {
		s.saveJob(j)
	}
}

func (s *Server) finishJob(j *dlJob, status, msg string) {
	s.dl.mu.Lock()
	j.Status, j.Err, j.Phase = status, msg, ""
	j.Finished = time.Now().Unix()
	for i := range j.Files {
		if f := &j.Files[i]; f.Status == fQueued || f.Status == fActive {
			f.Status, f.Speed = fCanceled, 0
			if status == "failed" {
				f.Status = fFailed
			}
		}
	}
	if j.Input.Status == "queued" || j.Input.Status == "running" {
		j.Input.Status = "failed"
		if status == "canceled" {
			j.Input.Status = ""
		}
	}
	delete(s.dl.jobs, j.ID)
	delete(s.dl.cancels, j.ID)
	delete(s.dl.userStop, j.ID)
	s.dl.mu.Unlock()
	s.saveJob(j)
	detail := fmt.Sprintf("任务 %d：%s", j.ID, jobSummary(j))
	if e := s.st.AddAction(InstanceAction{TS: time.Now().Unix(), InstanceID: j.InstanceID, Action: "启动下载器", Detail: detail, Err: msg}); e != nil {
		log.Printf("记录实例操作失败: %v", e)
	}
	log.Printf("下载任务 %d（实例 %d）结束：%s %s", j.ID, j.InstanceID, status, msg)
	if c, err := s.notifyConfig(); err == nil && c.Download && status != "canceled" {
		title := map[string]string{"done": "下载完成", "partial": "下载没全部完成", "failed": "下载失败"}[status]
		body := fmt.Sprintf("实例 %d：%s", j.InstanceID, jobSummary(j))
		if msg != "" {
			body += "。" + msg
		}
		s.notify("download", title, body, "#/instances")
	}
}

func jobSummary(j *dlJob) string {
	n := map[string]int{}
	for _, f := range j.Files {
		n[f.Status]++
	}
	parts := []string{}
	for _, k := range []struct{ k, t string }{{fDone, "下完"}, {fSkipped, "已存在"}, {fFailed, "失败"}, {fNoSpace, "空间不够"}, {fCanceled, "取消"}} {
		if n[k.k] > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", k.t, n[k.k]))
		}
	}
	switch j.Input.Status {
	case "done":
		parts = append(parts, fmt.Sprintf("ComfyUI input 已同步 %d 个文件", j.Input.Files))
	case "failed":
		parts = append(parts, "ComfyUI input 同步失败")
	}
	if len(parts) == 0 {
		return "没有文件"
	}
	return strings.Join(parts, "，")
}

// ---------------------------------------------------------------- 执行

var errInstanceGone = errors.New("实例已经不在了")

// remote 在实例上执行命令；SSH 断了会重试（最长 10 分钟），命令本身失败不重试。
func (s *Server) remote(ctx context.Context, j *dlJob, cmd string, stdin []byte, timeout time.Duration) (string, string, error) {
	start := time.Now()
	for {
		rctx, cancel := context.WithTimeout(ctx, timeout)
		out, errOut, err := s.runOn(rctx, j.InstanceID, cmd, stdin)
		cancel()
		var exit *ssh.ExitError
		if err == nil || errors.As(err, &exit) {
			return out, errOut, err
		}
		if ctx.Err() != nil {
			return "", "", ctx.Err()
		}
		if strings.Contains(err.Error(), "账户里没有实例") {
			return "", "", errInstanceGone
		}
		if time.Since(start) > dlSSHGiveUp {
			return "", "", fmt.Errorf("SSH 一直连不上：%v", err)
		}
		s.setPhase(j, "SSH 断了，重连中："+firstLine(err.Error()))
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
}

const aria2Install = `command -v aria2c >/dev/null 2>&1 || { export DEBIAN_FRONTEND=noninteractive;
  { apt-get install -y -qq aria2 || { apt-get update -qq && apt-get install -y -qq aria2; }; } >/tmp/vastmon-apt.log 2>&1; }
command -v aria2c >/dev/null 2>&1 && echo VASTMON_OK || { tail -5 /tmp/vastmon-apt.log >&2; exit 1; }`

func (s *Server) runJob(ctx context.Context, j *dlJob) {
	err := s.runJobSteps(ctx, j)
	s.dl.mu.Lock()
	userStop := s.dl.userStop[j.ID]
	s.dl.mu.Unlock()
	switch {
	case userStop:
		s.finishJob(j, "canceled", "")
	case ctx.Err() != nil:
		// 面板在退出：保持 running，下次启动接着跑
		s.saveJob(j)
	case err != nil:
		s.finishJob(j, "failed", err.Error())
	default:
		status := "done"
		for _, f := range j.Files {
			if f.Status != fDone && f.Status != fSkipped {
				status = "partial"
			}
		}
		if j.Input.Status == "failed" {
			status = "partial"
		}
		s.finishJob(j, status, "")
	}
}

func (s *Server) runJobSteps(ctx context.Context, j *dlJob) error {
	cfg, err := s.dlConfig()
	if err != nil {
		return err
	}
	if len(j.Files) > 0 {
		s.setPhase(j, "检查 aria2（没有就 apt 安装）")
		if _, errOut, err := s.remote(ctx, j, aria2Install+"; mkdir -p "+dlRemoteDir, nil, 10*time.Minute); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("装不上 aria2：%s", firstLine(errOut+" "+err.Error()))
		}
	}
	if j.Input.Status == "queued" || j.Input.Status == "running" {
		s.syncInputStep(ctx, j)
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	for i := range j.Files {
		s.dl.mu.Lock()
		st := j.Files[i].Status
		s.dl.mu.Unlock()
		if st != fQueued && st != fActive {
			continue
		}
		if err := s.runFile(ctx, j, i, cfg); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) syncInputStep(ctx context.Context, j *dlJob) {
	s.setPhase(j, "同步 ComfyUI input")
	s.dl.mu.Lock()
	j.Input.Status, j.Input.Err = "running", ""
	s.dl.mu.Unlock()
	s.saveJob(j)
	var n int
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		n, err = s.pushInput(ctx, j.InstanceID, j.Root, func(done, total int64) {
			s.dl.mu.Lock()
			j.Input.Done, j.Input.Total = done, total
			s.dl.mu.Unlock()
		})
		if err == nil || ctx.Err() != nil || strings.Contains(err.Error(), "还没有 ComfyUI input 备份") {
			break
		}
		time.Sleep(5 * time.Second)
	}
	s.dl.mu.Lock()
	if err != nil {
		j.Input.Status, j.Input.Err = "failed", err.Error()
	} else {
		j.Input.Status, j.Input.Files = "done", n
	}
	s.dl.mu.Unlock()
	s.saveJob(j)
}

func (s *Server) setFile(j *dlJob, i int, fn func(f *dlFile)) {
	s.dl.mu.Lock()
	fn(&j.Files[i])
	s.dl.mu.Unlock()
	s.saveJob(j)
}

func remoteDir(root, dir string) string {
	if strings.HasPrefix(dir, "/") {
		return dir
	}
	return path.Join(root, dir)
}

// runFile 下一个文件：解析链接 → 看实例上有没有 / 空间够不够 → 后台起 aria2 → 盯进度。
// 失败会重新解析链接再续传（签名地址可能过期），最多 dlMaxAttempts 次。
// 返回非 nil 表示整个任务要停（实例没了 / 取消）。
func (s *Server) runFile(ctx context.Context, j *dlJob, i int, cfg dlConfig) error {
	s.dl.mu.Lock()
	f := j.Files[i]
	s.dl.mu.Unlock()
	dir := remoteDir(j.Root, f.Dir)
	pre := fmt.Sprintf("%s/j%d-%d", dlRemoteDir, j.ID, i)
	resume := f.Status == fActive && f.Name != ""
	label := f.Name
	if label == "" {
		label = f.URL
	}
	for {
		if ctx.Err() != nil {
			return s.stopFile(j, i, pre)
		}
		if !resume {
			s.setPhase(j, "解析链接："+label)
			res, err := resolveDL(ctx, f.URL, cfg)
			if err != nil {
				if ctx.Err() != nil {
					continue
				}
				if s.fileAttemptFailed(j, i, "解析链接失败："+err.Error()) {
					return nil
				}
				sleepCtx(ctx, 10*time.Second)
				continue
			}
			name := f.Name
			if name == "" {
				name = res.Name
			}
			if name == "" {
				s.setFile(j, i, func(x *dlFile) {
					x.Status, x.Err, x.Finished = fFailed, "链接没给文件名，到设置里给这一项填上文件名", time.Now().Unix()
				})
				return nil
			}
			size := f.Size
			if res.Size > 0 {
				size = res.Size
			}
			f.Name, f.Size = name, size
			s.setFile(j, i, func(x *dlFile) { x.Name, x.Size = name, size })
			label = name

			// 实例上已经有了？空间够不够？
			s.setPhase(j, "检查实例上的 "+name)
			q := shellQuote(dir)
			qn := shellQuote(name)
			out, errOut, err := s.remote(ctx, j, "mkdir -p "+q+" && cd "+q+" && { stat -c %s "+qn+" 2>/dev/null || echo -1; }; "+
				"{ test -e "+qn+".aria2 && echo ctl || echo noctl; }; df -B1 --output=avail . | tail -1", nil, 30*time.Second)
			if err != nil {
				if ctx.Err() != nil || errors.Is(err, errInstanceGone) {
					return orCtx(ctx, err)
				}
				if s.fileAttemptFailed(j, i, "检查目标目录失败："+firstLine(errOut+" "+err.Error())) {
					return nil
				}
				continue
			}
			lines := strings.Fields(out)
			if len(lines) < 3 {
				if s.fileAttemptFailed(j, i, "检查目标目录：输出看不懂："+out) {
					return nil
				}
				continue
			}
			have, _ := strconv.ParseInt(lines[0], 10, 64)
			ctl := lines[1] == "ctl"
			avail, _ := strconv.ParseInt(lines[2], 10, 64)
			if have >= 0 && !ctl && (size <= 0 || have == size) {
				s.setFile(j, i, func(x *dlFile) {
					x.Status, x.Done, x.Err, x.Finished = fSkipped, have, "", time.Now().Unix()
					if x.Size <= 0 {
						x.Size = have
					}
				})
				return nil
			}
			if have >= 0 && !ctl {
				// 大小不对又没有 aria2 控制文件：不是 aria2 下了一半的，删掉重下
				s.remote(ctx, j, "rm -f "+shellQuote(path.Join(dir, name)), nil, 30*time.Second)
				have = 0
			}
			if size > 0 {
				need := size - max(have, 0) + dlSpaceMargin
				if avail < need {
					s.setFile(j, i, func(x *dlFile) {
						x.Status, x.Finished = fNoSpace, time.Now().Unix()
						x.Err = fmt.Sprintf("实例磁盘剩 %.1f GB，这个文件还要 %.1f GB", float64(avail)/(1<<30), float64(need)/(1<<30))
					})
					return nil
				}
			}

			// 后台起 aria2
			script := aria2Script(pre, dir, name, res, j.Connections)
			if _, errOut, err := s.remote(ctx, j, "mkdir -p "+dlRemoteDir+" && umask 077 && cat > "+pre+".sh", []byte(script), 30*time.Second); err != nil {
				if ctx.Err() != nil || errors.Is(err, errInstanceGone) {
					return orCtx(ctx, err)
				}
				if s.fileAttemptFailed(j, i, "写下载脚本失败："+firstLine(errOut+" "+err.Error())) {
					return nil
				}
				continue
			}
			if _, errOut, err := s.remote(ctx, j, "rm -f "+pre+".rc "+pre+".pid; setsid nohup bash "+pre+".sh >/dev/null 2>&1 </dev/null & echo started", nil, 30*time.Second); err != nil {
				if ctx.Err() != nil || errors.Is(err, errInstanceGone) {
					return orCtx(ctx, err)
				}
				if s.fileAttemptFailed(j, i, "启动 aria2 失败："+firstLine(errOut+" "+err.Error())) {
					return nil
				}
				continue
			}
			s.setFile(j, i, func(x *dlFile) {
				x.Status, x.Err, x.Speed = fActive, "", 0
				if x.Started == 0 {
					x.Started = time.Now().Unix()
				}
			})
		}
		resume = false
		s.setPhase(j, "下载 "+label)
		done, err := s.watchFile(ctx, j, i, pre, path.Join(dir, f.Name))
		switch {
		case ctx.Err() != nil:
			return s.stopFile(j, i, pre)
		case errors.Is(err, errInstanceGone):
			return err
		case done:
			return nil
		case err != nil:
			if s.fileAttemptFailed(j, i, err.Error()) {
				return nil
			}
			sleepCtx(ctx, 5*time.Second)
		}
	}
}

func orCtx(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// fileAttemptFailed 记一次失败；次数用完就标“失败”，返回 true。
func (s *Server) fileAttemptFailed(j *dlJob, i int, msg string) bool {
	gaveUp := false
	s.setFile(j, i, func(x *dlFile) {
		x.Attempts++
		x.Err, x.Speed = msg, 0
		if x.Attempts >= dlMaxAttempts {
			x.Status, x.Finished = fFailed, time.Now().Unix()
			gaveUp = true
		}
	})
	return gaveUp
}

// stopFile 用户取消时杀掉实例上的 aria2（留着 .aria2 控制文件，下次能续传）；
// 面板退出时什么都不做，让它在实例上接着下。
func (s *Server) stopFile(j *dlJob, i int, pre string) error {
	s.dl.mu.Lock()
	userStop := s.dl.userStop[j.ID]
	s.dl.mu.Unlock()
	if userStop {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		s.runOn(ctx, j.InstanceID, "p=$(cat "+pre+".pid 2>/dev/null) && kill -- -$p 2>/dev/null; kill $p 2>/dev/null; rm -f "+pre+".sh "+pre+".pid "+pre+".rc "+pre+".log", nil)
		cancel()
		s.setFile(j, i, func(x *dlFile) { x.Status, x.Speed = fCanceled, 0 })
	}
	return context.Canceled
}

func aria2Script(pre, dir, name string, res dlResolved, conns int) string {
	args := []string{
		"aria2c", "-c", "-x", strconv.Itoa(conns), "-s", strconv.Itoa(conns), "-k", "1M",
		"--file-allocation=none", "--auto-file-renaming=false", "--allow-overwrite=true",
		"--max-tries=5", "--retry-wait=5", "--connect-timeout=30", "--timeout=60",
		"--summary-interval=0", "--console-log-level=warn", "--show-console-readout=false",
		"--user-agent=vastmon/1",
		"-d", shellQuote(dir), "-o", shellQuote(name),
	}
	if res.Header != "" {
		args = append(args, "--header="+shellQuote(res.Header))
	}
	args = append(args, shellQuote(res.URL))
	return "#!/bin/bash\n" +
		"echo $$ > " + pre + ".pid\n" +
		strings.Join(args, " ") + " > " + pre + ".log 2>&1\n" +
		"rc=$?\n" +
		"rm -f " + pre + ".sh\n" +
		"echo $rc > " + pre + ".rc\n"
}

// watchFile 盯着实例上的 aria2：每 2 秒看一次已下大小（按实际占用的块算，
// aria2 多线程是稀疏写入，文件长度不代表进度）。下完返回 true。
func (s *Server) watchFile(ctx context.Context, j *dlJob, i int, pre, target string) (bool, error) {
	qt := shellQuote(target)
	cmd := "cat " + pre + ".rc 2>/dev/null || echo run; " +
		"{ p=$(cat " + pre + ".pid 2>/dev/null) && kill -0 $p 2>/dev/null && echo alive || echo dead; }; " +
		"stat -c '%s %b %B' " + qt + " 2>/dev/null || echo 0 0 0"
	var lastDone int64 = -1
	lastAt := time.Now()
	lastSave := time.Now()
	deadSeen := 0
	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(dlPoll):
		}
		out, errOut, err := s.remote(ctx, j, cmd, nil, 30*time.Second)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, errInstanceGone) {
				return false, orCtx(ctx, err)
			}
			return false, fmt.Errorf("查看进度失败：%s", firstLine(errOut+" "+err.Error()))
		}
		f := strings.Fields(out)
		if len(f) < 5 {
			continue
		}
		rc, state := f[0], f[1]
		length, _ := strconv.ParseInt(f[2], 10, 64)
		blocks, _ := strconv.ParseInt(f[3], 10, 64)
		bsize, _ := strconv.ParseInt(f[4], 10, 64)
		got := blocks * bsize
		if rc == "0" {
			var size int64
			s.dl.mu.Lock()
			size = j.Files[i].Size
			s.dl.mu.Unlock()
			if size > 0 && length != size {
				return false, fmt.Errorf("aria2 说下完了，但文件大小 %d 和链接给的 %d 不一样", length, size)
			}
			s.remote(ctx, j, "rm -f "+pre+".rc "+pre+".pid "+pre+".log", nil, 20*time.Second)
			s.setFile(j, i, func(x *dlFile) {
				x.Status, x.Done, x.Speed, x.Err, x.Finished = fDone, length, 0, "", time.Now().Unix()
				if x.Size <= 0 {
					x.Size = length
				}
			})
			return true, nil
		}
		if rc != "run" {
			tail, _, _ := s.remote(ctx, j, "grep -E 'ERROR|errorCode|Exception' "+pre+".log | tail -3; tail -2 "+pre+".log", nil, 20*time.Second)
			return false, fmt.Errorf("aria2 退出码 %s：%s", rc, strings.TrimSpace(lastLines(tail, 3)))
		}
		if state == "dead" {
			deadSeen++
			if deadSeen >= 3 {
				return false, errors.New("aria2 进程没了（实例重启过？）")
			}
			continue
		}
		deadSeen = 0
		now := time.Now()
		s.dl.mu.Lock()
		x := &j.Files[i]
		if x.Size > 0 && got > x.Size {
			got = x.Size
		}
		if lastDone >= 0 {
			inst := float64(got-lastDone) / now.Sub(lastAt).Seconds()
			if inst < 0 {
				inst = 0
			}
			if x.Speed == 0 {
				x.Speed = inst
			} else {
				x.Speed = 0.6*x.Speed + 0.4*inst
			}
		}
		x.Done = got
		s.dl.mu.Unlock()
		lastDone, lastAt = got, now
		if now.Sub(lastSave) > 15*time.Second {
			s.saveJob(j)
			lastSave = now
		}
	}
}

func lastLines(s string, n int) string {
	ls := strings.Split(strings.TrimSpace(s), "\n")
	seen := map[string]bool{}
	var out []string
	for _, l := range ls {
		l = strings.TrimSpace(l)
		if l != "" && !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return strings.Join(out, " / ")
}

// ---------------------------------------------------------------- 接口

func (s *Server) startDownload(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		ProfileID int   `json:"profile_id"`
		ItemIDs   []int `json:"item_ids"`
		SyncInput bool  `json:"sync_input"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if len(body.ItemIDs) == 0 && !body.SyncInput {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "一个文件都没选")
		return
	}
	ids := body.ItemIDs
	if len(ids) == 0 {
		ids = []int{-1} // 只同步 input
	}
	j, err := s.createJob(id, body.ProfileID, ids, body.SyncInput, false)
	if err != nil {
		writeError(w, http.StatusConflict, "DOWNLOAD", err.Error())
		return
	}
	s.startJob(j)
	writeJSON(w, s.jobSnapshot(j))
}

func (s *Server) jobSnapshot(j *dlJob) dlJob {
	s.dl.mu.Lock()
	defer s.dl.mu.Unlock()
	c := *j
	c.Files = append([]dlFile{}, j.Files...)
	return c
}

// downloadJobs 返回最近的任务（进行中的用内存里最新的进度）。
func (s *Server) downloadJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.loadJobs(`ORDER BY id DESC LIMIT 60`)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]dlJob, 0, len(jobs))
	for _, j := range jobs {
		s.dl.mu.Lock()
		live := s.dl.jobs[j.ID]
		s.dl.mu.Unlock()
		if live != nil {
			out = append(out, s.jobSnapshot(live))
		} else if j.ID != 0 {
			out = append(out, *j)
		}
	}
	writeJSON(w, map[string]any{"jobs": out, "now": time.Now().Unix()})
}

func (s *Server) cancelDownload(w http.ResponseWriter, r *http.Request) {
	jid, err := strconv.ParseInt(r.PathValue("jid"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "任务 ID 不对")
		return
	}
	s.dl.mu.Lock()
	j := s.dl.jobs[jid]
	cancel := s.dl.cancels[jid]
	if j != nil {
		s.dl.userStop[jid] = true
	}
	s.dl.mu.Unlock()
	switch {
	case j == nil:
		writeError(w, http.StatusNotFound, "NOT_FOUND", "这个任务已经结束了")
		return
	case cancel != nil:
		cancel()
	default:
		s.finishJob(j, "canceled", "")
	}
	writeJSON(w, map[string]bool{"ok": true})
}
