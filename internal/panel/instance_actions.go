package panel

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// 实例操作：都要登录，每次操作（成功或失败）记进 instance_actions。

type instanceAction struct {
	name string
	run  func(c *Client, id int64) error
}

var instanceActions = map[string]instanceAction{
	"start":   {"启动", func(c *Client, id int64) error { return c.SetState(id, "running") }},
	"stop":    {"停止", func(c *Client, id int64) error { return c.SetState(id, "stopped") }},
	"reboot":  {"重启", (*Client).Reboot},
	"recycle": {"重建", (*Client).Recycle},
	"destroy": {"销毁", (*Client).Destroy},
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "实例 ID 不对")
		return 0, false
	}
	return id, true
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "请求格式不对："+err.Error())
		return false
	}
	return true
}

// record 记一次操作并返回结果；err 非空时回 502。
func (s *Server) record(w http.ResponseWriter, id int64, action, detail string, err error) {
	a := InstanceAction{TS: time.Now().Unix(), InstanceID: id, Action: action, Detail: detail}
	if err != nil {
		a.Err = err.Error()
	}
	if e := s.st.AddAction(a); e != nil {
		log.Printf("记录实例操作失败: %v", e)
	}
	if err != nil {
		log.Printf("实例 %d %s 失败: %v", id, action, err)
		writeError(w, http.StatusBadGateway, "VAST", action+"失败："+err.Error())
		return
	}
	log.Printf("实例 %d %s %s", id, action, detail)
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) instanceAction(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Action  string `json:"action"`
		Confirm string `json:"confirm"`
		Backup  bool   `json:"backup_input"` // 停止 / 重建 / 销毁前先把 input 拉回来
	}
	if !decodeBody(w, r, &body) {
		return
	}
	act, ok := instanceActions[body.Action]
	if !ok {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "不认识的操作："+body.Action)
		return
	}
	if body.Action == "destroy" && strings.TrimSpace(body.Confirm) != strconv.FormatInt(id, 10) {
		writeError(w, http.StatusBadRequest, "CONFIRM", "销毁前要输入实例 ID 确认")
		return
	}
	detail := ""
	if body.Backup && (body.Action == "stop" || body.Action == "recycle" || body.Action == "destroy") {
		x, err := s.findInstance(id)
		if err == nil && str(x["actual_status"]) != "running" {
			detail = "没在运行，跳过备份 ComfyUI input"
		} else {
			// 备份可能要很久：交给后台任务，备份成功后由它执行这个操作，这里马上返回。
			j, err := s.startInputJob(id, body.Action, &act)
			if err != nil {
				writeError(w, http.StatusConflict, "BUSY", err.Error())
				return
			}
			writeJSON(w, map[string]any{"ok": true, "job": j})
			return
		}
	}
	if !s.actionMu.TryLock() {
		writeError(w, http.StatusConflict, "BUSY", "上一个操作还没结束")
		return
	}
	defer s.actionMu.Unlock()
	s.record(w, id, act.name, detail, act.run(s.cl, id))
}

func (s *Server) instanceLabel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Label string `json:"label"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	label := strings.TrimSpace(body.Label)
	if len([]rune(label)) > 128 {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "名称最多 128 个字符")
		return
	}
	s.record(w, id, "改名", label, s.cl.SetLabel(id, label))
}

func (s *Server) instanceLogs(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Tail   int    `json:"tail"`
		Filter string `json:"filter"`
		Daemon bool   `json:"daemon"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.Tail <= 0 || body.Tail > 100000 {
		body.Tail = 1000
	}
	u, err := s.cl.RequestLogs(id, body.Tail, strings.TrimSpace(body.Filter), body.Daemon)
	if err != nil {
		writeError(w, http.StatusBadGateway, "VAST", "请求日志失败："+err.Error())
		return
	}
	text, err := s.cl.FetchLogs(u, 30*time.Second, 2<<20)
	if err != nil {
		writeError(w, http.StatusBadGateway, "VAST", "读取日志失败："+err.Error())
		return
	}
	writeJSON(w, map[string]string{"text": text, "url": u})
}

func (s *Server) sshKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.cl.SSHKeys()
	if err != nil {
		writeError(w, http.StatusBadGateway, "VAST", "读取 SSH 密钥失败："+err.Error())
		return
	}
	if keys == nil {
		keys = []SSHKey{}
	}
	writeJSON(w, map[string]any{"keys": keys})
}

func (s *Server) instanceSSH(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Key string `json:"ssh_key"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	key := strings.TrimSpace(body.Key)
	fields := strings.Fields(key)
	if len(fields) < 2 || strings.ContainsAny(key, "\r\n") ||
		!(strings.HasPrefix(fields[0], "ssh-") || strings.HasPrefix(fields[0], "ecdsa-") || strings.HasPrefix(fields[0], "sk-")) {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "这不像 SSH 公钥（应以 ssh-ed25519 / ssh-rsa 等开头，只有一行）")
		return
	}
	detail := fields[0] + " …" + fields[1][max(0, len(fields[1])-12):]
	if len(fields) > 2 {
		detail += " " + strings.Join(fields[2:], " ")
	}
	s.record(w, id, "添加 SSH 密钥", detail, s.cl.AttachSSH(id, key))
}
