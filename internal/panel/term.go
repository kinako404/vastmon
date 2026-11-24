package panel

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"
)

// 网页终端：浏览器用 WebSocket 连 /api/term/{id}/ws，服务端在实例上开一个带 PTY 的
// shell。二进制帧是键盘输入 / 终端输出；文本帧是控制消息：浏览器发改大小
// {"type":"resize","cols":..,"rows":..}；服务端开头发 {"type":"ready"}，每 3 秒发一次
// 面板机到实例的 SSH 往返延迟 {"type":"latency","ms":..}，结束时发 {"type":"exit","msg":..} 再关连接。

type termMsg struct {
	Type string  `json:"type"`
	Cols int     `json:"cols,omitempty"`
	Rows int     `json:"rows,omitempty"`
	Msg  string  `json:"msg,omitempty"`
	Ms   float64 `json:"ms,omitempty"`
}

func clampDim(v string, def, lo, hi int) int {
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return max(lo, min(hi, n))
}

// termCheck 在网页终端连不上时由前端调用（普通 HTTP，不走 WebSocket）：
// 试一下面板到实例的 SSH，好区分是 SSH 的问题还是 CDN / 反代拦了 WebSocket。
func (s *Server) termCheck(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	start := time.Now()
	out, errOut, err := s.runOn(ctx, id, "echo VASTMON_OK", nil)
	if err == nil && !containsOK(out) {
		err = errors.New(firstLine(errOut))
	}
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "ms": float64(time.Since(start).Microseconds()) / 1000})
}

func (s *Server) terminal(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	// websocket.Accept 默认只接受和 Host 同源的 Origin，挡住跨站 WebSocket 劫持。
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(1 << 20)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wmu sync.Mutex
	send := func(typ websocket.MessageType, b []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		wctx, c := context.WithTimeout(ctx, 30*time.Second)
		defer c()
		return ws.Write(wctx, typ, b)
	}
	sendCtl := func(m termMsg) {
		b, _ := json.Marshal(m)
		_ = send(websocket.MessageText, b)
	}
	fail := func(msg string) {
		sendCtl(termMsg{Type: "exit", Msg: msg})
		ws.Close(websocket.StatusNormalClosure, "")
	}

	cols := clampDim(r.URL.Query().Get("cols"), 120, 20, 500)
	rows := clampDim(r.URL.Query().Get("rows"), 32, 5, 300)
	sess, p, err := s.session(id)
	if err != nil {
		fail(err.Error())
		return
	}
	defer sess.Close()
	s.pool.mu.Lock()
	p.users++
	s.pool.mu.Unlock()
	defer func() {
		s.pool.mu.Lock()
		p.users--
		p.last = time.Now()
		s.pool.mu.Unlock()
	}()

	modes := ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 115200, ssh.TTY_OP_OSPEED: 115200}
	if err := sess.RequestPty("xterm-256color", rows, cols, modes); err != nil {
		fail("申请终端失败：" + err.Error())
		return
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		fail(err.Error())
		return
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		fail(err.Error())
		return
	}
	sess.Stderr = nil
	if err := sess.Shell(); err != nil {
		fail("启动 shell 失败：" + err.Error())
		return
	}
	log.Printf("实例 %d 打开终端（%dx%d）", id, cols, rows)
	sendCtl(termMsg{Type: "ready"})

	// 面板机到实例的 SSH 往返延迟：用 SSH 保活请求量。
	go func() {
		t := time.NewTicker(3 * time.Second)
		defer t.Stop()
		for {
			start := time.Now()
			if _, _, err := p.client.SendRequest("keepalive@openssh.com", true, nil); err != nil {
				return
			}
			sendCtl(termMsg{Type: "latency", Ms: float64(time.Since(start).Microseconds()) / 1000})
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()

	// 实例 → 浏览器。输出读完不在这里收尾，等 shell 退出把原因发出去再关。
	outDone := make(chan struct{})
	go func() {
		defer close(outDone)
		buf := make([]byte, 32<<10)
		for {
			n, err := stdout.Read(buf)
			if n > 0 {
				if send(websocket.MessageBinary, buf[:n]) != nil {
					cancel()
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	// shell 退出（exit、实例断开）
	go func() {
		err := sess.Wait()
		select {
		case <-outDone:
		case <-time.After(2 * time.Second):
		}
		msg := "终端已退出"
		if err != nil && err != io.EOF {
			if ee, ok := err.(*ssh.ExitError); ok {
				msg = "终端已退出（代码 " + strconv.Itoa(ee.ExitStatus()) + "）"
			} else {
				msg = "连接断开：" + err.Error()
			}
		}
		sendCtl(termMsg{Type: "exit", Msg: msg})
		cancel()
	}()
	// 浏览器 → 实例
	go func() {
		defer cancel()
		for {
			typ, data, err := ws.Read(ctx)
			if err != nil {
				return
			}
			if typ == websocket.MessageBinary {
				if _, err := stdin.Write(data); err != nil {
					return
				}
				continue
			}
			var m termMsg
			if json.Unmarshal(data, &m) == nil && m.Type == "resize" && m.Cols > 0 && m.Rows > 0 {
				_ = sess.WindowChange(min(m.Rows, 300), min(m.Cols, 500))
			}
		}
	}()
	<-ctx.Done()
	log.Printf("实例 %d 关闭终端", id)
	ws.Close(websocket.StatusNormalClosure, "")
}
