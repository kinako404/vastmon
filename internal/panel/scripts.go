package panel

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 开机脚本：在“设置 → 开机脚本”里写几段 bash 脚本、存几个文件（程序、配置），
// 实例跑起来后（或在实例卡片上点“运行脚本”）由面板经 SSH 在实例上依次运行。
// 文件存在面板自己这里：运行前用 SFTP 推到实例的 /opt/vastmon/files/，
// 也可以用 /f/<令牌>/<文件名> 这个不用登录的链接下载（令牌随机，可以重新生成）。
// 变量（key=value，比如 TG_TOKEN）只存在服务器上，运行时作为环境变量交给脚本。

const scriptsSchema = `
CREATE TABLE IF NOT EXISTS script_files (
	name TEXT PRIMARY KEY, size INTEGER, sha256 TEXT, token TEXT UNIQUE, uploaded INTEGER
);
CREATE TABLE IF NOT EXISTS script_jobs (
	id INTEGER PRIMARY KEY AUTOINCREMENT, instance_id INTEGER, created INTEGER, status TEXT, data TEXT
);
CREATE INDEX IF NOT EXISTS script_jobs_inst ON script_jobs(instance_id, id);
`

const (
	remoteFilesDir     = "/opt/vastmon/files"
	remoteScriptsDir   = "/tmp/vastmon-scripts"
	maxScripts         = 50
	maxScriptBytes     = 256 << 10
	maxScriptFileBytes = 4 << 30
	scriptLogTail      = 16 << 10
	defaultScriptMin   = 30
)

type startScript struct {
	ID         int      `json:"id"`
	Name       string   `json:"name"`
	Content    string   `json:"content"`
	Files      []string `json:"files"`
	Auto       bool     `json:"auto"`        // 开机后自动运行 / “运行脚本”默认勾选
	TimeoutMin int      `json:"timeout_min"` // 超时分钟，0 = 默认 30
	Note       string   `json:"note"`
}

type scriptVar struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type scriptsConfig struct {
	Scripts []startScript `json:"scripts"`
	Vars    []scriptVar   `json:"vars"`
}

func (s *Server) scriptsConfig() (scriptsConfig, error) {
	c := scriptsConfig{}
	_, err := s.st.Get("scripts", &c)
	if c.Scripts == nil {
		c.Scripts = []startScript{}
	}
	if c.Vars == nil {
		c.Vars = []scriptVar{}
	}
	for i := range c.Scripts {
		if c.Scripts[i].Files == nil {
			c.Scripts[i].Files = []string{}
		}
	}
	return c, err
}

func (s *Server) filesDir() string { return filepath.Join(s.dataDir, "files") }

type scriptFile struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
	Token    string `json:"token"`
	Uploaded int64  `json:"uploaded"`
}

func (s *Server) scriptFiles() ([]scriptFile, error) {
	rs, err := s.st.db.Query(`SELECT name, size, sha256, token, uploaded FROM script_files ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := []scriptFile{}
	for rs.Next() {
		var f scriptFile
		if err := rs.Scan(&f.Name, &f.Size, &f.SHA256, &f.Token, &f.Uploaded); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rs.Err()
}

func randToken() string {
	b := make([]byte, 18)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// publicScripts 是发给浏览器的配置：变量只给名字和末 4 位。
func (s *Server) publicScripts() (map[string]any, error) {
	c, err := s.scriptsConfig()
	if err != nil {
		return nil, err
	}
	files, err := s.scriptFiles()
	if err != nil {
		return nil, err
	}
	vars := make([]map[string]string, 0, len(c.Vars))
	for _, v := range c.Vars {
		vars = append(vars, map[string]string{"key": v.Key, "hint": tokenHint(v.Value)})
	}
	nc, _ := s.notifyConfig()
	dc, _ := s.dlConfig()
	return map[string]any{"scripts": c.Scripts, "vars": vars, "files": files, "site_url": nc.SiteURL,
		"files_dir": remoteFilesDir, "workdir": dc.Root}, nil
}

func (s *Server) scriptsGet(w http.ResponseWriter, r *http.Request) {
	out, err := s.publicScripts()
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, out)
}

var varKeyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

// scriptsPut 整体替换脚本列表；vars 里 value 为空且 keep 为真时保留原值。
func (s *Server) scriptsPut(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Scripts []startScript `json:"scripts"`
		Vars    []struct {
			Key   string `json:"key"`
			Value string `json:"value"`
			Keep  bool   `json:"keep"`
		} `json:"vars"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "请求格式不对："+err.Error())
		return
	}
	cur, err := s.scriptsConfig()
	if err != nil {
		s.fail(w, err)
		return
	}
	if body.Scripts != nil {
		if len(body.Scripts) > maxScripts {
			writeError(w, http.StatusBadRequest, "BAD_REQUEST", fmt.Sprintf("最多 %d 个脚本", maxScripts))
			return
		}
		next := 0
		for _, sc := range cur.Scripts {
			next = max(next, sc.ID)
		}
		seen := map[int]bool{}
		out := make([]startScript, 0, len(body.Scripts))
		for _, sc := range body.Scripts {
			sc.Name = truncRunes(strings.TrimSpace(sc.Name), 60)
			if sc.Name == "" {
				writeError(w, http.StatusBadRequest, "BAD_REQUEST", "脚本名不能为空")
				return
			}
			if len(sc.Content) > maxScriptBytes {
				writeError(w, http.StatusBadRequest, "BAD_REQUEST", sc.Name+"：脚本太长（最多 256 KB）")
				return
			}
			sc.Content = strings.ReplaceAll(sc.Content, "\r\n", "\n")
			if sc.TimeoutMin < 0 || sc.TimeoutMin > 24*60 {
				writeError(w, http.StatusBadRequest, "BAD_REQUEST", sc.Name+"：超时要在 0–1440 分钟")
				return
			}
			files := []string{}
			for _, f := range sc.Files {
				if n, err := cleanName(f); err == nil && n != "" {
					files = append(files, n)
				}
			}
			sc.Files = files
			sc.Note = truncRunes(strings.TrimSpace(sc.Note), 200)
			if sc.ID <= 0 || seen[sc.ID] {
				next++
				sc.ID = next
			}
			seen[sc.ID] = true
			out = append(out, sc)
		}
		cur.Scripts = out
	}
	if body.Vars != nil {
		old := map[string]string{}
		for _, v := range cur.Vars {
			old[v.Key] = v.Value
		}
		seen := map[string]bool{}
		vars := []scriptVar{}
		for _, v := range body.Vars {
			v.Key = strings.TrimSpace(v.Key)
			if v.Key == "" {
				continue
			}
			if !varKeyRE.MatchString(v.Key) || strings.HasPrefix(v.Key, "VASTMON_") {
				writeError(w, http.StatusBadRequest, "BAD_REQUEST", "变量名只能用字母、数字、下划线，不能以数字或 VASTMON_ 开头："+v.Key)
				return
			}
			if seen[v.Key] {
				writeError(w, http.StatusBadRequest, "BAD_REQUEST", "变量名重复："+v.Key)
				return
			}
			seen[v.Key] = true
			val := v.Value
			if val == "" && v.Keep {
				val = old[v.Key]
			}
			if strings.ContainsAny(val, "\x00") || len(val) > 16<<10 {
				writeError(w, http.StatusBadRequest, "BAD_REQUEST", "变量值不对："+v.Key)
				return
			}
			vars = append(vars, scriptVar{Key: v.Key, Value: val})
		}
		cur.Vars = vars
	}
	if err := s.st.Set("scripts", cur); err != nil {
		s.fail(w, err)
		return
	}
	s.scriptsGet(w, r)
}

// ---------------------------------------------------------------- 文件库

func (s *Server) uploadScriptFile(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxScriptFileBytes+(1<<20))
	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "要用 multipart 上传")
		return
	}
	overwrite := r.URL.Query().Get("overwrite") == "1"
	if err := os.MkdirAll(s.filesDir(), 0o700); err != nil {
		s.fail(w, err)
		return
	}
	var saved []string
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, "BAD_REQUEST", "上传中断："+err.Error())
			return
		}
		if part.FormName() != "file" || part.FileName() == "" {
			continue
		}
		name, err := cleanName(path.Base(strings.ReplaceAll(part.FileName(), "\\", "/")))
		if err != nil || name == "" {
			writeError(w, http.StatusBadRequest, "BAD_REQUEST", "文件名不对："+part.FileName())
			return
		}
		dst := filepath.Join(s.filesDir(), name)
		if _, err := os.Stat(dst); err == nil && !overwrite {
			writeError(w, http.StatusConflict, "EXISTS", "已经有同名文件："+name)
			return
		}
		tmp, err := os.CreateTemp(s.filesDir(), ".upload-*")
		if err != nil {
			s.fail(w, err)
			return
		}
		h := sha256.New()
		n, err := io.Copy(io.MultiWriter(tmp, h), part)
		tmp.Close()
		if err != nil {
			os.Remove(tmp.Name())
			writeError(w, http.StatusBadRequest, "BAD_REQUEST", "上传失败："+err.Error())
			return
		}
		if err := os.Rename(tmp.Name(), dst); err != nil {
			os.Remove(tmp.Name())
			s.fail(w, err)
			return
		}
		_, err = s.st.db.Exec(`INSERT INTO script_files (name, size, sha256, token, uploaded) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(name) DO UPDATE SET size = excluded.size, sha256 = excluded.sha256, uploaded = excluded.uploaded`,
			name, n, hex.EncodeToString(h.Sum(nil)), randToken(), time.Now().Unix())
		if err != nil {
			s.fail(w, err)
			return
		}
		saved = append(saved, name)
	}
	if len(saved) == 0 {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "没有收到文件")
		return
	}
	s.scriptsGet(w, r)
}

func (s *Server) deleteScriptFile(w http.ResponseWriter, r *http.Request) {
	name, err := cleanName(r.PathValue("name"))
	if err != nil || name == "" {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "文件名不对")
		return
	}
	if _, err := s.st.db.Exec(`DELETE FROM script_files WHERE name = ?`, name); err != nil {
		s.fail(w, err)
		return
	}
	os.Remove(filepath.Join(s.filesDir(), name))
	s.scriptsGet(w, r)
}

// renewFileToken 换一个下载链接（旧链接立即失效）。
func (s *Server) renewFileToken(w http.ResponseWriter, r *http.Request) {
	name, err := cleanName(r.PathValue("name"))
	if err != nil || name == "" {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "文件名不对")
		return
	}
	if _, err := s.st.db.Exec(`UPDATE script_files SET token = ? WHERE name = ?`, randToken(), name); err != nil {
		s.fail(w, err)
		return
	}
	s.scriptsGet(w, r)
}

// publicFile 是 /f/<令牌>/<文件名>：不用登录，靠令牌保密，给脚本里 curl 用。
func (s *Server) publicFile(w http.ResponseWriter, r *http.Request) {
	tok, name := r.PathValue("token"), r.PathValue("name")
	var size int64
	err := s.st.db.QueryRow(`SELECT size FROM script_files WHERE token = ? AND name = ?`, tok, name).Scan(&size)
	if err != nil || len(tok) < 16 {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(filepath.Join(s.filesDir(), name))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, name, st.ModTime(), f)
}

// ---------------------------------------------------------------- 任务

type scriptStep struct {
	ScriptID int    `json:"script_id"`
	Name     string `json:"name"`
	Status   string `json:"status"` // queued / running / done / failed / canceled
	Started  int64  `json:"started"`
	Finished int64  `json:"finished"`
	Exit     int    `json:"exit"`
	Log      string `json:"log"` // 输出的最后一段
	Err      string `json:"err"`
}

type scriptJob struct {
	ID         int64        `json:"id"`
	InstanceID int64        `json:"instance_id"`
	Created    int64        `json:"created"`
	Started    int64        `json:"started"`
	Finished   int64        `json:"finished"`
	Status     string       `json:"status"` // waiting / running / done / partial / failed / canceled
	Phase      string       `json:"phase"`
	Auto       bool         `json:"auto"`
	Steps      []scriptStep `json:"steps"`
	Err        string       `json:"err"`
}

func (j *scriptJob) active() bool { return j.Status == "waiting" || j.Status == "running" }

type scriptManager struct {
	mu       sync.Mutex
	jobs     map[int64]*scriptJob
	cancels  map[int64]context.CancelFunc
	userStop map[int64]bool
}

func (s *Server) saveScriptJob(j *scriptJob) {
	s.sc.mu.Lock()
	b, err := json.Marshal(j)
	status := j.Status
	s.sc.mu.Unlock()
	if err == nil {
		_, err = s.st.db.Exec(`UPDATE script_jobs SET status = ?, data = ? WHERE id = ?`, status, string(b), j.ID)
	}
	if err != nil {
		log.Printf("保存脚本任务 %d 失败: %v", j.ID, err)
	}
}

func (s *Server) loadScriptJobs(q string, args ...any) ([]*scriptJob, error) {
	rs, err := s.st.db.Query(`SELECT data FROM script_jobs `+q, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []*scriptJob
	for rs.Next() {
		var d string
		if err := rs.Scan(&d); err != nil {
			return nil, err
		}
		j := &scriptJob{}
		if json.Unmarshal([]byte(d), j) == nil && j.ID != 0 {
			out = append(out, j)
		}
	}
	return out, rs.Err()
}

// createScriptJob 建任务：ids 为空就用勾了“开机自动”的脚本。
func (s *Server) createScriptJob(instanceID int64, ids []int, auto bool) (*scriptJob, error) {
	c, err := s.scriptsConfig()
	if err != nil {
		return nil, err
	}
	want := map[int]bool{}
	for _, id := range ids {
		want[id] = true
	}
	j := &scriptJob{InstanceID: instanceID, Created: time.Now().Unix(), Status: "waiting", Auto: auto, Steps: []scriptStep{}}
	for _, sc := range c.Scripts {
		if (len(want) == 0 && sc.Auto) || want[sc.ID] {
			j.Steps = append(j.Steps, scriptStep{ScriptID: sc.ID, Name: sc.Name, Status: "queued"})
		}
	}
	if len(j.Steps) == 0 {
		return nil, errors.New("没有要运行的脚本：到“设置 → 开机脚本”里添加，或勾上“开机自动运行”")
	}
	s.sc.mu.Lock()
	defer s.sc.mu.Unlock()
	for _, x := range s.sc.jobs {
		if x.InstanceID == instanceID && x.active() {
			return nil, fmt.Errorf("实例 %d 已经有脚本在跑（任务 %d）", instanceID, x.ID)
		}
	}
	res, err := s.st.db.Exec(`INSERT INTO script_jobs (instance_id, created, status, data) VALUES (?, ?, ?, '{}')`, instanceID, j.Created, j.Status)
	if err != nil {
		return nil, err
	}
	j.ID, _ = res.LastInsertId()
	s.sc.jobs[j.ID] = j
	return j, nil
}

// scriptsInit 在开始监听前调用：接着跑面板重启前没跑完的任务。
func (s *Server) scriptsInit() {
	s.sc.jobs = map[int64]*scriptJob{}
	s.sc.cancels = map[int64]context.CancelFunc{}
	s.sc.userStop = map[int64]bool{}
	jobs, err := s.loadScriptJobs(`WHERE status IN ('waiting', 'running') ORDER BY id`)
	if err != nil {
		log.Printf("读取脚本任务失败: %v", err)
		return
	}
	for _, j := range jobs {
		s.sc.jobs[j.ID] = j
		if j.Status == "running" {
			log.Printf("接着跑脚本任务 %d（实例 %d）", j.ID, j.InstanceID)
			s.startScriptJob(j)
		}
	}
}

func (s *Server) startScriptJob(j *scriptJob) {
	ctx, cancel := context.WithCancel(s.dl.base)
	s.sc.mu.Lock()
	s.sc.cancels[j.ID] = cancel
	j.Status = "running"
	if j.Started == 0 {
		j.Started = time.Now().Unix()
	}
	s.sc.mu.Unlock()
	s.saveScriptJob(j)
	go s.runScriptJob(ctx, j)
}

// scriptsLoop 等开机后自动运行的任务的实例跑起来、SSH 连得上，再开始。
func (s *Server) scriptsLoop(ctx context.Context) {
	t := time.NewTicker(dlWaitEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.sc.mu.Lock()
		var waiting []*scriptJob
		for _, j := range s.sc.jobs {
			if j.Status == "waiting" {
				waiting = append(waiting, j)
			}
		}
		s.sc.mu.Unlock()
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
				s.finishScriptJob(j, "failed", "实例已经不在了")
				continue
			case time.Since(time.Unix(j.Created, 0)) > dlWaitGiveUp:
				s.finishScriptJob(j, "failed", "等了 3 小时实例还没跑起来")
				continue
			case st != "running":
				s.setScriptPhase(j, "等实例跑起来（"+st+"）")
				continue
			}
			rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			out, _, err := s.runOn(rctx, j.InstanceID, "echo VASTMON_OK", nil)
			cancel()
			if err != nil || !containsOK(out) {
				s.setScriptPhase(j, "等 SSH 连得上："+firstLine(errString(err)))
				continue
			}
			s.startScriptJob(j)
		}
	}
}

func (s *Server) setScriptPhase(j *scriptJob, p string) {
	s.sc.mu.Lock()
	changed := j.Phase != p
	j.Phase = p
	s.sc.mu.Unlock()
	if changed {
		s.saveScriptJob(j)
	}
}

func (s *Server) setStep(j *scriptJob, i int, fn func(st *scriptStep)) {
	s.sc.mu.Lock()
	fn(&j.Steps[i])
	s.sc.mu.Unlock()
	s.saveScriptJob(j)
}

func (s *Server) finishScriptJob(j *scriptJob, status, msg string) {
	s.sc.mu.Lock()
	j.Status, j.Err, j.Phase = status, msg, ""
	j.Finished = time.Now().Unix()
	for i := range j.Steps {
		if st := &j.Steps[i]; st.Status == "queued" || st.Status == "running" {
			st.Status = "canceled"
			if status == "failed" {
				st.Status = "failed"
			}
		}
	}
	delete(s.sc.jobs, j.ID)
	delete(s.sc.cancels, j.ID)
	delete(s.sc.userStop, j.ID)
	s.sc.mu.Unlock()
	s.saveScriptJob(j)
	n := map[string]int{}
	for _, st := range j.Steps {
		n[st.Status]++
	}
	summary := fmt.Sprintf("成功 %d 个", n["done"])
	if n["failed"] > 0 {
		summary += fmt.Sprintf("，失败 %d 个", n["failed"])
	}
	if n["canceled"] > 0 {
		summary += fmt.Sprintf("，取消 %d 个", n["canceled"])
	}
	if e := s.st.AddAction(InstanceAction{TS: time.Now().Unix(), InstanceID: j.InstanceID, Action: "开机脚本", Detail: fmt.Sprintf("任务 %d：%s", j.ID, summary), Err: msg}); e != nil {
		log.Printf("记录实例操作失败: %v", e)
	}
	log.Printf("脚本任务 %d（实例 %d）结束：%s %s", j.ID, j.InstanceID, status, msg)
	if c, err := s.notifyConfig(); err == nil && c.Scripts && status != "canceled" {
		title := map[string]string{"done": "开机脚本跑完了", "partial": "有开机脚本失败", "failed": "开机脚本失败"}[status]
		var failed []string
		for _, st := range j.Steps {
			if st.Status == "failed" {
				failed = append(failed, st.Name)
			}
		}
		body := fmt.Sprintf("实例 %d：%s", j.InstanceID, summary)
		if len(failed) > 0 {
			body += "（" + strings.Join(failed, "、") + "）"
		}
		if msg != "" {
			body += "。" + msg
		}
		s.notify("script", title, body, "#/instances")
	}
}

func (s *Server) runScriptJob(ctx context.Context, j *scriptJob) {
	err := s.runScriptSteps(ctx, j)
	s.sc.mu.Lock()
	userStop := s.sc.userStop[j.ID]
	s.sc.mu.Unlock()
	switch {
	case userStop:
		s.finishScriptJob(j, "canceled", "")
	case ctx.Err() != nil:
		s.saveScriptJob(j) // 面板在退出：保持 running，下次启动接着看
	case err != nil:
		s.finishScriptJob(j, "failed", err.Error())
	default:
		status := "done"
		for _, st := range j.Steps {
			if st.Status != "done" {
				status = "partial"
			}
		}
		s.finishScriptJob(j, status, "")
	}
}

func (s *Server) runScriptSteps(ctx context.Context, j *scriptJob) error {
	c, err := s.scriptsConfig()
	if err != nil {
		return err
	}
	byID := map[int]startScript{}
	for _, sc := range c.Scripts {
		byID[sc.ID] = sc
	}
	for i := range j.Steps {
		s.sc.mu.Lock()
		st := j.Steps[i]
		s.sc.mu.Unlock()
		if st.Status != "queued" && st.Status != "running" {
			continue
		}
		sc, ok := byID[st.ScriptID]
		if !ok {
			s.setStep(j, i, func(x *scriptStep) {
				x.Status, x.Err, x.Finished = "failed", "这个脚本已经从设置里删掉了", time.Now().Unix()
			})
			continue
		}
		if err := s.runScriptStep(ctx, j, i, sc, c.Vars); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) scriptRemote(ctx context.Context, j *scriptJob, cmd string, stdin []byte, timeout time.Duration) (string, string, error) {
	return s.remoteRetry(ctx, j.InstanceID, cmd, stdin, timeout, func(msg string) { s.setScriptPhase(j, msg) })
}

// pushScriptFiles 把脚本要用的文件推到实例的 /opt/vastmon/files/（同名同 sha256 的跳过）。
func (s *Server) pushScriptFiles(ctx context.Context, j *scriptJob, names []string) error {
	if len(names) == 0 {
		return nil
	}
	files, err := s.scriptFiles()
	if err != nil {
		return err
	}
	byName := map[string]scriptFile{}
	for _, f := range files {
		byName[f.Name] = f
	}
	if _, errOut, err := s.scriptRemote(ctx, j, "mkdir -p "+remoteFilesDir, nil, 30*time.Second); err != nil {
		return fmt.Errorf("建 %s 失败：%s", remoteFilesDir, firstLine(errOut+" "+err.Error()))
	}
	for _, name := range names {
		f, ok := byName[name]
		if !ok {
			return fmt.Errorf("文件库里没有 %s", name)
		}
		dst := remoteFilesDir + "/" + name
		out, _, _ := s.scriptRemote(ctx, j, "sha256sum "+shellQuote(dst)+" 2>/dev/null | cut -c1-64", nil, 10*time.Minute)
		if strings.TrimSpace(out) == f.SHA256 {
			continue
		}
		if err := s.uploadToInstance(ctx, j, filepath.Join(s.filesDir(), name), dst, f.Size); err != nil {
			return err
		}
	}
	return nil
}

// uploadToInstance 经 SSH 标准输入把文件流过去（SFTP 逐块等回包，延迟高时很慢），
// 先写临时文件，传完整了再改名。
func (s *Server) uploadToInstance(ctx context.Context, j *scriptJob, local, remote string, size int64) error {
	src, err := os.Open(local)
	if err != nil {
		return fmt.Errorf("面板上的文件读不到：%v", err)
	}
	defer src.Close()
	sess, _, err := s.session(j.InstanceID)
	if err != nil {
		return err
	}
	defer sess.Close()
	name := path.Base(remote)
	last := time.Time{}
	sess.Stdin = &progressReader{r: src, fn: func(n int64) {
		if time.Since(last) > time.Second {
			last = time.Now()
			s.setScriptPhase(j, fmt.Sprintf("上传 %s %.1f / %.1f MB", name, float64(n)/1048576, float64(size)/1048576))
		}
	}}
	var errb strings.Builder
	sess.Stderr = &errb
	tmp := shellQuote(remote + ".vastmon-upload")
	done := make(chan error, 1)
	go func() {
		done <- sess.Run("cat > " + tmp + " && chmod 755 " + tmp + " && mv -f " + tmp + " " + shellQuote(remote))
	}()
	select {
	case err = <-done:
	case <-ctx.Done():
		sess.Close()
		return ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("上传 %s 失败：%s", name, firstLine(errb.String()+" "+err.Error()))
	}
	return nil
}

// scriptEnv 是脚本开头的 export：面板提供的 VASTMON_* 加上用户的变量。
func (s *Server) scriptEnv(j *scriptJob, vars []scriptVar) string {
	nc, _ := s.notifyConfig()
	dc, _ := s.dlConfig()
	env := [][2]string{
		{"VASTMON_FILES", remoteFilesDir},
		{"VASTMON_WORKDIR", dc.Root},
		{"VASTMON_INSTANCE_ID", strconv.FormatInt(j.InstanceID, 10)},
		{"VASTMON_SITE", nc.SiteURL},
	}
	if x, err := s.findInstance(j.InstanceID); err == nil {
		env = append(env, [2]string{"VASTMON_LABEL", str(x["label"])}, [2]string{"VASTMON_GPU", fmt.Sprintf("%d× %s", num(x["num_gpus"]), str(x["gpu_name"]))})
	}
	for _, v := range vars {
		env = append(env, [2]string{v.Key, v.Value})
	}
	var b strings.Builder
	for _, kv := range env {
		b.WriteString("export " + kv[0] + "=" + shellQuote(kv[1]) + "\n")
	}
	return b.String()
}

// runScriptStep 推文件 → 写脚本 → 后台运行 → 每 2 秒看输出和退出码，超时就杀。
// 返回非 nil 表示整个任务要停（实例没了 / 取消 / 面板退出）。
func (s *Server) runScriptStep(ctx context.Context, j *scriptJob, i int, sc startScript, vars []scriptVar) error {
	pre := fmt.Sprintf("%s/j%d-%d", remoteScriptsDir, j.ID, i)
	timeout := time.Duration(sc.TimeoutMin) * time.Minute
	if timeout == 0 {
		timeout = defaultScriptMin * time.Minute
	}
	s.sc.mu.Lock()
	resume := j.Steps[i].Status == "running"
	started := j.Steps[i].Started
	s.sc.mu.Unlock()
	if !resume {
		s.setScriptPhase(j, "准备 "+sc.Name)
		if err := s.pushScriptFiles(ctx, j, sc.Files); err != nil {
			if ctx.Err() != nil || errors.Is(err, errInstanceGone) {
				return orCtx(ctx, err)
			}
			s.setStep(j, i, func(x *scriptStep) { x.Status, x.Err, x.Finished = "failed", err.Error(), time.Now().Unix() })
			return nil
		}
		body := "#!/bin/bash\n" + s.scriptEnv(j, vars) + "cd \"${VASTMON_WORKDIR:-/root}\" 2>/dev/null || cd /root\n" + sc.Content + "\n"
		wrapper := "#!/bin/bash\necho $$ > " + pre + ".pid\nbash " + pre + ".sh > " + pre + ".log 2>&1 < /dev/null\nrc=$?\nrm -f " + pre + ".sh\necho $rc > " + pre + ".rc\n"
		cmd := "mkdir -p " + remoteScriptsDir + " && chmod 700 " + remoteScriptsDir + " && umask 077 && cat > " + pre + ".sh"
		if _, errOut, err := s.scriptRemote(ctx, j, cmd, []byte(body), 30*time.Second); err != nil {
			if ctx.Err() != nil || errors.Is(err, errInstanceGone) {
				return orCtx(ctx, err)
			}
			s.setStep(j, i, func(x *scriptStep) {
				x.Status, x.Err, x.Finished = "failed", "写脚本失败："+firstLine(errOut+" "+err.Error()), time.Now().Unix()
			})
			return nil
		}
		// 只把 setsid 那一步放后台：写成 "a && b &" 的话整串都进后台，cat 读不到标准输入
		launch := "{ umask 077 && cat > " + pre + ".run && rm -f " + pre + ".rc " + pre + ".pid " + pre + ".log; } && { setsid nohup bash " + pre + ".run >/dev/null 2>&1 </dev/null & } && echo started"
		if _, errOut, err := s.scriptRemote(ctx, j, launch, []byte(wrapper), 30*time.Second); err != nil {
			if ctx.Err() != nil || errors.Is(err, errInstanceGone) {
				return orCtx(ctx, err)
			}
			s.setStep(j, i, func(x *scriptStep) {
				x.Status, x.Err, x.Finished = "failed", "启动脚本失败："+firstLine(errOut+" "+err.Error()), time.Now().Unix()
			})
			return nil
		}
		started = time.Now().Unix()
		s.setStep(j, i, func(x *scriptStep) { x.Status, x.Started, x.Err, x.Log = "running", started, "", "" })
	}
	s.setScriptPhase(j, "运行 "+sc.Name)
	poll := "cat " + pre + ".rc 2>/dev/null || echo run; { p=$(cat " + pre + ".pid 2>/dev/null) && kill -0 $p 2>/dev/null && echo alive || echo dead; }; " +
		"echo ---VASTMON-LOG---; tail -c " + strconv.Itoa(scriptLogTail) + " " + pre + ".log 2>/dev/null; true"
	dead := 0
	lastSave := time.Now()
	for {
		select {
		case <-ctx.Done():
			return s.stopScriptStep(j, i, pre)
		case <-time.After(dlPoll):
		}
		out, errOut, err := s.scriptRemote(ctx, j, poll, nil, 30*time.Second)
		if err != nil {
			if ctx.Err() != nil {
				return s.stopScriptStep(j, i, pre)
			}
			if errors.Is(err, errInstanceGone) {
				return err
			}
			s.setStep(j, i, func(x *scriptStep) {
				x.Status, x.Err, x.Finished = "failed", "查看脚本状态失败："+firstLine(errOut+" "+err.Error()), time.Now().Unix()
			})
			return nil
		}
		head, logText, _ := strings.Cut(out, "---VASTMON-LOG---\n")
		f := strings.Fields(head)
		if len(f) < 2 {
			continue
		}
		rc, state := f[0], f[1]
		s.sc.mu.Lock()
		j.Steps[i].Log = logText
		s.sc.mu.Unlock()
		if rc != "run" {
			code, _ := strconv.Atoi(rc)
			s.scriptRemote(ctx, j, "rm -f "+pre+".rc "+pre+".pid "+pre+".run", nil, 20*time.Second)
			s.setStep(j, i, func(x *scriptStep) {
				x.Exit, x.Finished = code, time.Now().Unix()
				x.Status = "done"
				if code != 0 {
					x.Status, x.Err = "failed", fmt.Sprintf("退出码 %d", code)
				}
			})
			return nil
		}
		if state == "dead" {
			dead++
			if dead >= 3 {
				s.setStep(j, i, func(x *scriptStep) {
					x.Status, x.Err, x.Finished = "failed", "脚本进程没了（实例重启过？）", time.Now().Unix()
				})
				return nil
			}
			continue
		}
		dead = 0
		if time.Since(time.Unix(started, 0)) > timeout {
			s.killScript(j, pre)
			s.setStep(j, i, func(x *scriptStep) {
				x.Status, x.Err, x.Finished = "failed", fmt.Sprintf("超过 %d 分钟没跑完，已停止", int(timeout.Minutes())), time.Now().Unix()
			})
			return nil
		}
		if time.Since(lastSave) > 10*time.Second {
			s.saveScriptJob(j)
			lastSave = time.Now()
		}
	}
}

func (s *Server) killScript(j *scriptJob, pre string) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s.runOn(ctx, j.InstanceID, "p=$(cat "+pre+".pid 2>/dev/null) && kill -- -$p 2>/dev/null; kill $p 2>/dev/null; rm -f "+pre+".sh "+pre+".run "+pre+".pid "+pre+".rc", nil)
}

// stopScriptStep 用户取消时杀掉实例上的脚本；面板退出时不动它。
func (s *Server) stopScriptStep(j *scriptJob, i int, pre string) error {
	s.sc.mu.Lock()
	userStop := s.sc.userStop[j.ID]
	s.sc.mu.Unlock()
	if userStop {
		s.killScript(j, pre)
		s.setStep(j, i, func(x *scriptStep) { x.Status, x.Finished = "canceled", time.Now().Unix() })
	}
	return context.Canceled
}

// ---------------------------------------------------------------- 接口

func (s *Server) startScripts(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		ScriptIDs []int `json:"script_ids"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if len(body.ScriptIDs) == 0 {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "一个脚本都没选")
		return
	}
	j, err := s.createScriptJob(id, body.ScriptIDs, false)
	if err != nil {
		writeError(w, http.StatusConflict, "SCRIPTS", err.Error())
		return
	}
	s.startScriptJob(j)
	writeJSON(w, s.scriptJobSnapshot(j))
}

func (s *Server) scriptJobSnapshot(j *scriptJob) scriptJob {
	s.sc.mu.Lock()
	defer s.sc.mu.Unlock()
	c := *j
	c.Steps = append([]scriptStep{}, j.Steps...)
	return c
}

func (s *Server) scriptJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.loadScriptJobs(`ORDER BY id DESC LIMIT 60`)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]scriptJob, 0, len(jobs))
	for _, j := range jobs {
		s.sc.mu.Lock()
		live := s.sc.jobs[j.ID]
		s.sc.mu.Unlock()
		if live != nil {
			out = append(out, s.scriptJobSnapshot(live))
		} else {
			out = append(out, *j)
		}
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].ID > out[b].ID })
	writeJSON(w, map[string]any{"jobs": out, "now": time.Now().Unix()})
}

func (s *Server) cancelScripts(w http.ResponseWriter, r *http.Request) {
	jid, err := strconv.ParseInt(r.PathValue("jid"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "任务 ID 不对")
		return
	}
	s.sc.mu.Lock()
	j := s.sc.jobs[jid]
	cancel := s.sc.cancels[jid]
	if j != nil {
		s.sc.userStop[jid] = true
	}
	s.sc.mu.Unlock()
	switch {
	case j == nil:
		writeError(w, http.StatusNotFound, "NOT_FOUND", "这个任务已经结束了")
		return
	case cancel != nil:
		cancel()
	default:
		s.finishScriptJob(j, "canceled", "")
	}
	writeJSON(w, map[string]bool{"ok": true})
}
