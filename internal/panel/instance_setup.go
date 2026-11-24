package panel

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"
)

// 开机后的小设置：vast 镜像的 ~/.bashrc 在 SSH 登录时自动进 tmux（除非有
// ~/.no_auto_tmux），网页终端和平时 ssh 都不好用。开机时勾了“关闭登录自动 tmux”，
// 就记一条待办，后台等实例跑起来、SSH 连得上，再去 touch 这个文件。

const setupSchema = `
CREATE TABLE IF NOT EXISTS instance_setup (
	instance_id INTEGER PRIMARY KEY, no_tmux INTEGER, created INTEGER,
	done_at INTEGER NOT NULL DEFAULT 0, tries INTEGER NOT NULL DEFAULT 0, err TEXT NOT NULL DEFAULT ''
);
`

const (
	setupEvery   = 15 * time.Second
	setupGiveUp  = 3 * time.Hour
	noTmuxCmd    = "touch /root/.no_auto_tmux && echo VASTMON_OK"
	noTmuxAction = "关闭自动 tmux"
)

type InstanceSetup struct {
	InstanceID int64  `json:"instance_id"`
	NoTmux     bool   `json:"no_tmux"`
	Created    int64  `json:"created"`
	DoneAt     int64  `json:"done_at"`
	Tries      int    `json:"tries"`
	Err        string `json:"err"`
}

func (s *Store) AddSetup(id int64, noTmux bool) error {
	_, err := s.db.Exec(`INSERT INTO instance_setup (instance_id, no_tmux, created) VALUES (?, ?, ?)
		ON CONFLICT(instance_id) DO UPDATE SET no_tmux = excluded.no_tmux, done_at = 0, tries = 0, err = ''`,
		id, noTmux, time.Now().Unix())
	return err
}

func (s *Store) Setups() ([]InstanceSetup, error) {
	rs, err := s.db.Query(`SELECT instance_id, no_tmux, created, done_at, tries, err FROM instance_setup ORDER BY created DESC LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := []InstanceSetup{}
	for rs.Next() {
		var x InstanceSetup
		if err := rs.Scan(&x.InstanceID, &x.NoTmux, &x.Created, &x.DoneAt, &x.Tries, &x.Err); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rs.Err()
}

func (s *Store) setupResult(id int64, done bool, msg string) {
	q := `UPDATE instance_setup SET tries = tries + 1, err = ? WHERE instance_id = ?`
	args := []any{msg, id}
	if done {
		q = `UPDATE instance_setup SET tries = tries + 1, err = '', done_at = ? WHERE instance_id = ?`
		args = []any{time.Now().Unix(), id}
	}
	if _, err := s.db.Exec(q, args...); err != nil {
		log.Printf("记录开机设置失败: %v", err)
	}
}

// setupLoop 定期处理还没做完的开机设置。
func (s *Server) setupLoop(ctx context.Context) {
	t := time.NewTicker(setupEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.runSetups(ctx)
	}
}

func (s *Server) runSetups(ctx context.Context) {
	all, err := s.st.Setups()
	if err != nil {
		log.Printf("读取开机设置失败: %v", err)
		return
	}
	var todo []InstanceSetup
	for _, x := range all {
		if x.DoneAt == 0 && x.NoTmux && x.Err != "超时没做成" {
			todo = append(todo, x)
		}
	}
	if len(todo) == 0 {
		return
	}
	ins, err := s.cl.Instances()
	if err != nil {
		return
	}
	status := map[int64]string{}
	for _, x := range ins {
		status[int64(num(x["id"]))] = str(x["actual_status"])
	}
	for _, x := range todo {
		st, alive := status[x.InstanceID]
		switch {
		case !alive:
			s.st.setupResult(x.InstanceID, false, "实例已经不在了")
			continue
		case time.Since(time.Unix(x.Created, 0)) > setupGiveUp:
			s.st.setupResult(x.InstanceID, false, "超时没做成")
			continue
		case st != "running":
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, 40*time.Second)
		out, errOut, err := s.runOn(rctx, x.InstanceID, noTmuxCmd, nil)
		cancel()
		if err == nil && containsOK(out) {
			s.st.setupResult(x.InstanceID, true, "")
			if e := s.st.AddAction(InstanceAction{TS: time.Now().Unix(), InstanceID: x.InstanceID, Action: noTmuxAction, Detail: "开机后自动"}); e != nil {
				log.Printf("记录实例操作失败: %v", e)
			}
			log.Printf("实例 %d 已关闭登录自动 tmux", x.InstanceID)
			continue
		}
		msg := errOut
		if err != nil {
			msg = err.Error()
		}
		s.st.setupResult(x.InstanceID, false, firstLine(msg))
	}
}

func containsOK(out string) bool {
	return strings.Contains(out, "VASTMON_OK")
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		s = "没有输出"
	}
	return s
}

// noTmuxNow 给已经开着的实例立刻关掉自动 tmux。
func (s *Server) noTmuxNow(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	out, errOut, err := s.runOn(ctx, id, noTmuxCmd, nil)
	if err == nil && !containsOK(out) {
		err = errors.New(firstLine(errOut))
	}
	if err == nil {
		if e := s.st.AddSetup(id, true); e == nil {
			s.st.setupResult(id, true, "")
		}
	}
	s.record(w, id, noTmuxAction, "", err)
}
