package panel

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"
)

// 网页终端。终端会话（实例上一个带 PTY 的 shell）由面板持有，不跟着网页走：浏览器刷新、
// 断网、关掉页面，shell 和里面跑着的程序都还在；再打开终端页（换台电脑也一样）会把还活着
// 的会话列出来接回去，并回放最近的输出。会话在这些时候结束：shell 自己退出、用户关掉标签
// （DELETE）、实例没了、面板重启（会话只在内存里），或者 7 天没有任何浏览器连着。
//
// 浏览器用 WebSocket 连 /api/term/{id}/ws[?session=会话号]：带着还活着的会话号就接回那个
// 会话，否则新开一个。二进制帧是键盘输入 / 终端输出；文本帧是控制消息：浏览器发改大小
// {"type":"resize","cols":..,"rows":..}；服务端开头发 {"type":"ready","session":..,"resumed":..}
// （紧跟着一帧回放），每 3 秒发一次面板机到实例的 SSH 往返延迟 {"type":"latency","ms":..}，
// shell 结束时发 {"type":"exit","msg":..} 再关连接。一个会话可以同时接几个浏览器。

const (
	termReplay     = 1 << 20 // 每个会话留多少输出，接回时回放
	termDetachKeep = 7 * 24 * time.Hour
	termMax        = 24
)

type termMsg struct {
	Type    string  `json:"type"`
	Cols    int     `json:"cols,omitempty"`
	Rows    int     `json:"rows,omitempty"`
	Msg     string  `json:"msg,omitempty"`
	Ms      float64 `json:"ms,omitempty"`
	Session string  `json:"session,omitempty"`
	Resumed bool    `json:"resumed,omitempty"`
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

// modeTracker 从输出里认 ESC[?…h / ESC[?…l，记下终端各个模式现在是开是关（备用屏、
// 鼠标跟踪、隐藏光标这些）。回放被截短时，开头那些切换模式的序列已经不在了，要靠它补上。
type modeTracker struct {
	state  int // 0 普通，1 见到 ESC，2 见到 ESC[，3 在 ESC[? 的参数里
	params []byte
	modes  map[int]bool
}

func (m *modeTracker) feed(b []byte) {
	for _, ch := range b {
		switch {
		case ch == 0x1b:
			m.state = 1
		case m.state == 1 && ch == '[':
			m.state = 2
		case m.state == 2 && ch == '?':
			m.state, m.params = 3, m.params[:0]
		case m.state == 3 && (ch >= '0' && ch <= '9' || ch == ';') && len(m.params) < 64:
			m.params = append(m.params, ch)
		case m.state == 3 && (ch == 'h' || ch == 'l'):
			if m.modes == nil {
				m.modes = map[int]bool{}
			}
			n, have := 0, false
			for _, p := range append(m.params, ';') {
				if p != ';' {
					n, have = n*10+int(p-'0'), true
					continue
				}
				if _, known := m.modes[n]; have && n < 10000 && (known || len(m.modes) < 64) {
					m.modes[n] = ch == 'h'
				}
				n, have = 0, false
			}
			m.state = 0
		default:
			m.state = 0
		}
	}
}

// alt 报告现在是不是在备用屏（vim、htop、nvtop 这类全屏程序）。
func (m *modeTracker) alt() bool {
	return m.modes[1049] || m.modes[1047] || m.modes[47]
}

// prefix 是把一个刚重置过的终端调到现在这些模式的序列；先进备用屏，再设别的。
func (m *modeTracker) prefix() []byte {
	nums := make([]int, 0, len(m.modes))
	for n := range m.modes {
		if n != 3 { // 132 列模式会清屏改宽度，不重放
			nums = append(nums, n)
		}
	}
	isAlt := func(n int) bool { return n == 1049 || n == 1047 || n == 47 }
	sort.Slice(nums, func(i, j int) bool {
		if isAlt(nums[i]) != isAlt(nums[j]) {
			return isAlt(nums[i])
		}
		return nums[i] < nums[j]
	})
	var out []byte
	for _, n := range nums {
		c := byte('l')
		if m.modes[n] {
			c = 'h'
		}
		out = append(out, fmt.Sprintf("\x1b[?%d%c", n, c)...)
	}
	return out
}

type termOut struct {
	data []byte
	ctl  *termMsg
}

// termClient 是接在会话上的一个浏览器连接。
type termClient struct {
	ch   chan termOut
	gone chan struct{}
	once sync.Once
}

func (c *termClient) drop() { c.once.Do(func() { close(c.gone) }) }

// send 把输出交给这个浏览器。它来不及收时等一等（shell 的输出因此慢下来，和直接连着时
// 一样）；15 秒还塞不进去就断开它，免得一个卡住的页面拖住别的。
func (c *termClient) send(o termOut) {
	select {
	case c.ch <- o:
		return
	case <-c.gone:
		return
	default:
	}
	t := time.NewTimer(15 * time.Second)
	defer t.Stop()
	select {
	case c.ch <- o:
	case <-c.gone:
	case <-t.C:
		c.drop()
	}
}

type termSession struct {
	id         string
	instanceID int64
	created    time.Time
	sess       *ssh.Session
	pool       *pooledSSH
	inMu       sync.Mutex
	stdin      io.Writer

	mu       sync.Mutex
	buf      []byte // 最近的输出
	total    int64  // 一共输出过多少字节
	modes    modeTracker
	clients  map[*termClient]struct{}
	rows     int
	cols     int
	done     bool
	detached time.Time // 最后一个浏览器断开的时间；有人连着时是零值
	why      string    // 是面板主动关的话，记下原因
}

// kill 结束会话：关掉 SSH 通道，shell 和里面的前台程序会收到挂断信号。
func (ts *termSession) kill(why string) {
	ts.mu.Lock()
	if ts.why == "" {
		ts.why = why
	}
	ts.mu.Unlock()
	ts.sess.Close()
}

type termManager struct {
	mu       sync.Mutex
	sessions map[string]*termSession
	reaper   sync.Once
}

func (s *Server) termGet(sid string, instanceID int64) *termSession {
	s.tm.mu.Lock()
	defer s.tm.mu.Unlock()
	if ts := s.tm.sessions[sid]; ts != nil && ts.instanceID == instanceID {
		return ts
	}
	return nil
}

// termNew 在实例上开一个带 PTY 的 shell，并开始把它的输出收进会话。
func (s *Server) termNew(instanceID int64, rows, cols int) (*termSession, error) {
	s.tm.mu.Lock()
	n := len(s.tm.sessions)
	s.tm.mu.Unlock()
	if n >= termMax {
		return nil, fmt.Errorf("已经开着 %d 个终端了，先关掉几个不用的标签", n)
	}
	sess, p, err := s.session(instanceID)
	if err != nil {
		return nil, err
	}
	modes := ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 115200, ssh.TTY_OP_OSPEED: 115200}
	if err := sess.RequestPty("xterm-256color", rows, cols, modes); err != nil {
		sess.Close()
		return nil, errors.New("申请终端失败：" + err.Error())
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		sess.Close()
		return nil, err
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		sess.Close()
		return nil, err
	}
	sess.Stderr = nil
	if err := sess.Shell(); err != nil {
		sess.Close()
		return nil, errors.New("启动 shell 失败：" + err.Error())
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		sess.Close()
		return nil, err
	}
	ts := &termSession{id: hex.EncodeToString(id), instanceID: instanceID, created: time.Now(), sess: sess, pool: p, stdin: stdin,
		clients: map[*termClient]struct{}{}, rows: rows, cols: cols, detached: time.Now()}
	// 和以前一样算一个使用者，SSH 连接不会因为“空闲”被关掉。
	s.pool.mu.Lock()
	p.users++
	s.pool.mu.Unlock()
	s.tm.mu.Lock()
	if s.tm.sessions == nil {
		s.tm.sessions = map[string]*termSession{}
	}
	s.tm.sessions[ts.id] = ts
	s.tm.mu.Unlock()
	s.tm.reaper.Do(func() { go s.termReap() })
	log.Printf("实例 %d 打开终端（%dx%d）", instanceID, cols, rows)

	// 实例 → 会话 → 接着的浏览器
	outDone := make(chan struct{})
	go func() {
		defer close(outDone)
		buf := make([]byte, 32<<10)
		for {
			n, err := stdout.Read(buf)
			if n > 0 {
				ts.output(append([]byte(nil), buf[:n]...))
			}
			if err != nil {
				return
			}
		}
	}()
	// shell 退出（exit、被关掉、实例断开）：等输出收完，把原因告诉浏览器再收尾。
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
		s.tm.mu.Lock()
		delete(s.tm.sessions, ts.id)
		s.tm.mu.Unlock()
		ts.mu.Lock()
		ts.done = true
		if ts.why != "" {
			msg = ts.why
		}
		clients := ts.clientList()
		ts.mu.Unlock()
		for _, c := range clients {
			c.send(termOut{ctl: &termMsg{Type: "exit", Msg: msg}})
		}
		sess.Close()
		s.pool.mu.Lock()
		p.users--
		p.last = time.Now()
		s.pool.mu.Unlock()
		log.Printf("实例 %d 关闭终端（%s）", instanceID, msg)
	}()
	return ts, nil
}

func (ts *termSession) clientList() []*termClient {
	out := make([]*termClient, 0, len(ts.clients))
	for c := range ts.clients {
		out = append(out, c)
	}
	return out
}

func (ts *termSession) output(chunk []byte) {
	ts.mu.Lock()
	ts.modes.feed(chunk)
	ts.total += int64(len(chunk))
	ts.buf = append(ts.buf, chunk...)
	if len(ts.buf) > 2*termReplay {
		ts.buf = append(make([]byte, 0, 2*termReplay), ts.buf[len(ts.buf)-termReplay:]...)
	}
	clients := ts.clientList()
	ts.mu.Unlock()
	for _, c := range clients {
		c.send(termOut{data: chunk})
	}
}

// attach 把浏览器接到会话上：先把回放放进它的队列，之后的输出跟在后面。会话已经结束时返回 false。
func (ts *termSession) attach(c *termClient) bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.done {
		return false
	}
	tail := ts.buf
	if len(tail) > termReplay {
		tail = tail[len(tail)-termReplay:]
	}
	replay := []byte{}
	if ts.total > int64(len(tail)) {
		// 前面的被截掉了：从一个转义序列的开头（没有就从下一行）接着放，并补上现在的模式。
		for i, ch := range tail[:min(len(tail), 8<<10)] {
			if ch == 0x1b || (ch == '\n' && i+1 < len(tail)) {
				if ch == '\n' {
					i++
				}
				tail = tail[i:]
				break
			}
		}
		replay = append(replay, ts.modes.prefix()...)
	}
	replay = append(replay, tail...)
	c.ch <- termOut{data: replay}
	ts.clients[c] = struct{}{}
	ts.detached = time.Time{}
	return true
}

func (ts *termSession) detach(c *termClient) {
	ts.mu.Lock()
	delete(ts.clients, c)
	if len(ts.clients) == 0 {
		ts.detached = time.Now()
	}
	ts.mu.Unlock()
}

func (ts *termSession) resize(rows, cols int) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.done {
		return
	}
	ts.rows, ts.cols = rows, cols
	_ = ts.sess.WindowChange(rows, cols)
}

// redraw 让全屏程序整屏重画：窗口大小抖一下，它会收到 SIGWINCH。只在备用屏里才做。
func (ts *termSession) redraw(rows, cols int) {
	ts.mu.Lock()
	alt := ts.modes.alt() && !ts.done
	ts.mu.Unlock()
	if !alt {
		return
	}
	ts.resize(rows, max(cols-1, 20))
	time.Sleep(150 * time.Millisecond)
	ts.resize(rows, cols)
}

func (ts *termSession) write(b []byte) error {
	ts.inMu.Lock()
	defer ts.inMu.Unlock()
	_, err := ts.stdin.Write(b)
	return err
}

// termReap 关掉太久没有浏览器连着的会话。
func (s *Server) termReap() {
	for range time.Tick(time.Minute) {
		s.tm.mu.Lock()
		var old []*termSession
		for _, ts := range s.tm.sessions {
			ts.mu.Lock()
			if !ts.detached.IsZero() && time.Since(ts.detached) > termDetachKeep {
				old = append(old, ts)
			}
			ts.mu.Unlock()
		}
		s.tm.mu.Unlock()
		for _, ts := range old {
			ts.kill(fmt.Sprintf("终端 %d 天没人连，已关闭", int(termDetachKeep.Hours()/24)))
		}
	}
}

// termSessions 列出还活着的终端会话，终端页打开时按它恢复标签。
func (s *Server) termSessions(w http.ResponseWriter, r *http.Request) {
	type item struct {
		ID         string `json:"id"`
		InstanceID int64  `json:"instance_id"`
		Created    int64  `json:"created"`
		Clients    int    `json:"clients"`
	}
	out := []item{}
	s.tm.mu.Lock()
	for _, ts := range s.tm.sessions {
		ts.mu.Lock()
		out = append(out, item{ID: ts.id, InstanceID: ts.instanceID, Created: ts.created.UnixMilli(), Clients: len(ts.clients)})
		ts.mu.Unlock()
	}
	s.tm.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Created != out[j].Created {
			return out[i].Created < out[j].Created
		}
		return out[i].ID < out[j].ID
	})
	writeJSON(w, map[string]any{"sessions": out})
}

// termClose 是关掉标签：结束会话，shell 和里面的前台程序会收到挂断信号。
func (s *Server) termClose(w http.ResponseWriter, r *http.Request) {
	s.tm.mu.Lock()
	ts := s.tm.sessions[r.PathValue("sid")]
	s.tm.mu.Unlock()
	if ts != nil {
		ts.kill("终端已关闭")
	}
	writeJSON(w, map[string]bool{"ok": true})
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
	defer ws.Close(websocket.StatusNormalClosure, "")

	cols := clampDim(r.URL.Query().Get("cols"), 120, 20, 500)
	rows := clampDim(r.URL.Query().Get("rows"), 32, 5, 300)
	c := &termClient{ch: make(chan termOut, 64), gone: make(chan struct{})}
	ts := s.termGet(r.URL.Query().Get("session"), id)
	resumed := ts != nil && ts.attach(c)
	if !resumed {
		if ts, err = s.termNew(id, rows, cols); err != nil {
			sendCtl(termMsg{Type: "exit", Msg: err.Error()})
			return
		}
		if !ts.attach(c) {
			sendCtl(termMsg{Type: "exit", Msg: "终端刚打开就退出了"})
			return
		}
	}
	defer ts.detach(c)
	sendCtl(termMsg{Type: "ready", Session: ts.id, Resumed: resumed})
	if resumed {
		log.Printf("实例 %d 接回终端", id)
		go ts.redraw(rows, cols)
	}

	// 面板机到实例的 SSH 往返延迟：用 SSH 保活请求量。
	go func() {
		t := time.NewTicker(3 * time.Second)
		defer t.Stop()
		for {
			start := time.Now()
			if _, _, err := ts.pool.client.SendRequest("keepalive@openssh.com", true, nil); err != nil {
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
	// 浏览器 → 实例
	go func() {
		defer cancel()
		for {
			typ, data, err := ws.Read(ctx)
			if err != nil {
				return
			}
			if typ == websocket.MessageBinary {
				if ts.write(data) != nil {
					return
				}
				continue
			}
			var m termMsg
			if json.Unmarshal(data, &m) == nil && m.Type == "resize" && m.Cols > 0 && m.Rows > 0 {
				ts.resize(min(m.Rows, 300), min(m.Cols, 500))
			}
		}
	}()
	// 会话 → 浏览器。浏览器断开只是从会话上摘下来，shell 留着。
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.gone:
			return
		case o := <-c.ch:
			if o.ctl != nil {
				sendCtl(*o.ctl)
				return
			}
			if len(o.data) > 0 && send(websocket.MessageBinary, o.data) != nil {
				return
			}
		}
	}
}
