package panel

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// input 备份任务：把实例的 input 拉回来是后台任务，不占着 HTTP 请求——实例到面板机的
// 网络慢的时候要拉很久，套在 Cloudflare 后面的请求超过 100 秒会被掐成 524，请求一断
// 备份也跟着取消。任务可以带一个后续操作（停止 / 重建 / 销毁），备份成功了才执行。
//
// 超过“设置”里的时间还没拉完，页面会问：继续等、放弃备份（直接做后续操作），还是取消。
// 问的时候传输不停，不回答就是继续等。任务只记在内存里，面板重启就没了，后续操作不会执行。

const (
	defaultInputAskMin = 3
	inputJobHardLimit  = 6 * time.Hour
	inputJobKeep       = 10 * time.Minute
)

type inputJob struct {
	ID         int64  `json:"id"`
	InstanceID int64  `json:"instance_id"`
	Action     string `json:"action"` // 备份完要做的：stop / recycle / destroy；空 = 只备份
	ActionName string `json:"action_name"`
	Status     string `json:"status"` // backup / acting / done / failed / canceled
	Started    int64  `json:"started"`
	Finished   int64  `json:"finished"`
	Bytes      int64  `json:"bytes"`   // 已经拉回来的字节
	Total      int64  `json:"total"`   // 实例上 input 目录的大小（估计总量用），0 = 不知道
	AskAt      int64  `json:"ask_at"`  // 到这个时间还在备份，页面就问要不要继续等
	Skipped    bool   `json:"skipped"` // 放弃了备份，直接做的后续操作
	Text       string `json:"text"`
	Err        string `json:"err"`

	cancel   context.CancelFunc
	decision string // skip / cancel
}

func (j *inputJob) active() bool { return j.Status == "backup" || j.Status == "acting" }

type inputJobs struct {
	mu   sync.Mutex
	seq  int64
	jobs []*inputJob
}

func (s *Server) inputAskAfter() time.Duration {
	c, err := s.dlConfig()
	if err != nil {
		return defaultInputAskMin * time.Minute
	}
	return time.Duration(c.InputAskMin) * time.Minute
}

// startInputJob 开始备份实例的 input；act 非空时备份成功后执行它。
func (s *Server) startInputJob(id int64, action string, act *instanceAction) (inputJob, error) {
	ask := s.inputAskAfter()
	s.ij.mu.Lock()
	defer s.ij.mu.Unlock()
	for _, x := range s.ij.jobs {
		if x.InstanceID == id && x.active() {
			return inputJob{}, fmt.Errorf("实例 %d 正在备份 ComfyUI input（在实例卡片上可以放弃或取消）", id)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), inputJobHardLimit)
	now := time.Now()
	s.ij.seq++
	j := &inputJob{ID: s.ij.seq, InstanceID: id, Action: action, Status: "backup", Started: now.Unix(), AskAt: now.Add(ask).Unix(), cancel: cancel}
	if act != nil {
		j.ActionName = act.name
	}
	s.ij.jobs = append(s.ij.jobs, j)
	go s.runInputJob(ctx, j, act)
	return *j, nil
}

func (s *Server) finishInputJob(j *inputJob, status, text, errText string) {
	s.ij.mu.Lock()
	j.Status, j.Text, j.Err, j.Finished = status, text, errText, time.Now().Unix()
	s.ij.mu.Unlock()
	log.Printf("input 备份任务 %d（实例 %d）结束：%s %s%s", j.ID, j.InstanceID, status, text, errText)
}

func (s *Server) addAction(id int64, action, detail string, err error) {
	if e := s.st.AddAction(InstanceAction{TS: time.Now().Unix(), InstanceID: id, Action: action, Detail: detail, Err: errString(err)}); e != nil {
		log.Printf("记录实例操作失败: %v", e)
	}
}

func (s *Server) runInputJob(ctx context.Context, j *inputJob, act *instanceAction) {
	defer j.cancel()
	id := j.InstanceID
	if c, err := s.dlConfig(); err == nil {
		sizeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		out, _, err := s.runOn(sizeCtx, id, "du -sb "+shellQuote(c.Root)+"/input 2>/dev/null | cut -f1", nil)
		cancel()
		if n, perr := strconv.ParseInt(strings.TrimSpace(out), 10, 64); err == nil && perr == nil {
			s.ij.mu.Lock()
			j.Total = n
			s.ij.mu.Unlock()
		}
	}
	res, err := s.backupInput(ctx, id, func(n int64) {
		s.ij.mu.Lock()
		j.Bytes = n
		s.ij.mu.Unlock()
	})
	s.ij.mu.Lock()
	decision, got := j.decision, j.Bytes
	s.ij.mu.Unlock()

	detail := ""
	if act != nil {
		detail = "在" + act.name + "前"
	}
	gotText := fmt.Sprintf("已拉回 %.1f MB", float64(got)/1048576)
	switch {
	case decision == "cancel":
		s.addAction(id, "备份 ComfyUI input", detail, errors.New("取消了（"+gotText+"）"))
		text := "已取消备份"
		if act != nil {
			text += "，没有" + act.name
		}
		s.finishInputJob(j, "canceled", text, "")
		return
	case err == nil:
		s.addAction(id, "备份 ComfyUI input", strings.TrimPrefix(detail+"："+res.String(), "："), nil)
	case decision == "skip":
		s.addAction(id, "备份 ComfyUI input", detail, errors.New("放弃了（"+gotText+"）"))
	default:
		s.addAction(id, "备份 ComfyUI input", detail, err)
		text := ""
		if act != nil {
			text = "没有" + act.name + "。不需要备份就取消勾选“先备份 ComfyUI 的 input 目录”再试。"
		}
		s.finishInputJob(j, "failed", text, "备份 ComfyUI input 失败："+err.Error())
		return
	}
	if act == nil {
		if err != nil {
			s.finishInputJob(j, "canceled", "已放弃备份", "")
		} else {
			s.finishInputJob(j, "done", "ComfyUI input 已备份："+res.String(), "")
		}
		return
	}

	s.ij.mu.Lock()
	j.Status, j.Skipped = "acting", err != nil
	s.ij.mu.Unlock()
	how := "已先备份 ComfyUI input"
	if err != nil {
		how = "没备份 ComfyUI input（放弃了）"
	}
	s.actionMu.Lock()
	actErr := act.run(s.cl, id)
	s.actionMu.Unlock()
	s.addAction(id, act.name, how, actErr)
	if actErr != nil {
		log.Printf("实例 %d %s 失败: %v", id, act.name, actErr)
		s.finishInputJob(j, "failed", how, act.name+"失败："+actErr.Error())
		return
	}
	log.Printf("实例 %d %s %s", id, act.name, how)
	s.finishInputJob(j, "done", "已"+act.name+"，"+how, "")
}

// inputJobList 返回还在跑的和刚结束不久的任务。
func (s *Server) inputJobList(w http.ResponseWriter, r *http.Request) {
	ask := s.inputAskAfter()
	now := time.Now()
	s.ij.mu.Lock()
	keep := s.ij.jobs[:0]
	out := []inputJob{}
	for _, j := range s.ij.jobs {
		if !j.active() && now.Unix()-j.Finished > int64(inputJobKeep.Seconds()) {
			continue
		}
		keep = append(keep, j)
		out = append(out, *j)
	}
	s.ij.jobs = keep
	s.ij.mu.Unlock()
	writeJSON(w, map[string]any{"jobs": out, "now": now.Unix(), "ask_min": int(ask.Minutes())})
}

// inputJobDecide 是超时后（或者任何时候）用户的选择：wait 继续等，skip 放弃备份直接做
// 后续操作，cancel 备份和后续操作都不做了。只在还在备份时有效。
func (s *Server) inputJobDecide(w http.ResponseWriter, r *http.Request) {
	jid, err := strconv.ParseInt(r.PathValue("jid"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "任务号不对")
		return
	}
	var body struct {
		Decision string `json:"decision"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	ask := s.inputAskAfter()
	s.ij.mu.Lock()
	defer s.ij.mu.Unlock()
	for _, j := range s.ij.jobs {
		if j.ID != jid {
			continue
		}
		if j.Status != "backup" {
			writeError(w, http.StatusConflict, "TOO_LATE", "备份已经结束了，改不了")
			return
		}
		switch body.Decision {
		case "wait":
			j.AskAt = time.Now().Add(ask).Unix()
		case "skip", "cancel":
			if j.decision == "" {
				j.decision = body.Decision
				log.Printf("input 备份任务 %d（实例 %d）：用户选了 %s", j.ID, j.InstanceID, body.Decision)
			}
			j.cancel()
		default:
			writeError(w, http.StatusBadRequest, "BAD_REQUEST", "decision 要是 wait / skip / cancel")
			return
		}
		writeJSON(w, map[string]any{"ok": true, "job": *j})
		return
	}
	writeError(w, http.StatusNotFound, "NOT_FOUND", "没有这个备份任务（面板重启过？）")
}

// clearInputArchive 是“设置”里的“清空 input 备份”：把 input.tar.gz 改名成 .prev
// （留一份后悔药，下次备份时被覆盖），之后新开的实例不再同步旧的 input。
func (s *Server) clearInputArchive(w http.ResponseWriter, r *http.Request) {
	p := inputArchivePath()
	s.inputMu.Lock()
	st, err := os.Stat(p)
	if p == "" || err != nil {
		s.inputMu.Unlock()
		writeError(w, http.StatusNotFound, "NOT_FOUND", "还没有 ComfyUI input 备份，不用清")
		return
	}
	err = os.Rename(p, p+".prev")
	s.inputMu.Unlock()
	if err != nil {
		s.fail(w, err)
		return
	}
	log.Printf("清空了 ComfyUI input 备份（%.1f MB，旧的留在 %s.prev）", float64(st.Size())/1048576, p)
	c, err := s.dlConfig()
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, c.public())
}
