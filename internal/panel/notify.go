package panel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 浏览器通知：在“设置 → 浏览器通知”里给浏览器开通知（Web Push 订阅存在
// push_subs），面板每分钟检查一次，有事就推给所有绑定的浏览器，并记进 notify_events。
// 浏览器只在 HTTPS（或 localhost）下允许通知，所以要配“站点 URL”（自己套 CDN / 反代）。

const notifySchema = `
CREATE TABLE IF NOT EXISTS push_subs (
	id INTEGER PRIMARY KEY AUTOINCREMENT, endpoint TEXT UNIQUE, p256dh TEXT, auth TEXT, name TEXT,
	created INTEGER, last_ts INTEGER NOT NULL DEFAULT 0, last_err TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS notify_events (
	id INTEGER PRIMARY KEY AUTOINCREMENT, ts INTEGER, kind TEXT, title TEXT, body TEXT, url TEXT,
	sent INTEGER NOT NULL DEFAULT 0, err TEXT NOT NULL DEFAULT ''
);
`

const (
	notifyEvery     = time.Minute
	balanceRemind   = 6 * time.Hour
	spendCheckEvery = 5 * time.Minute
	maxPushSubs     = 20
)

type notifyConfig struct {
	SiteURL      string  `json:"site_url"`
	Instance     bool    `json:"instance"`      // 实例开机 / 运行 / 停止 / 离线 / 销毁
	Download     bool    `json:"download"`      // 启动下载器结束
	Scripts      bool    `json:"scripts"`       // 开机脚本跑完 / 失败
	BalanceBelow float64 `json:"balance_below"` // 余额低于多少美元提醒，0 = 不提醒
	BalanceHours float64 `json:"balance_hours"` // 按当前花费撑不过几小时提醒，0 = 不提醒
	DailySpend   float64 `json:"daily_spend"`   // 今天（UTC 日，vast 账单）花费超过多少美元提醒，0 = 不提醒
}

func (s *Server) notifyConfig() (notifyConfig, error) {
	c := notifyConfig{Instance: true, Download: true, Scripts: true, BalanceBelow: 10, BalanceHours: 12}
	_, err := s.st.Get("notify", &c)
	return c, err
}

// siteLink 是通知点开后打开的地址：站点 URL + 面板路由。
func (c notifyConfig) siteLink(route string) string {
	if c.SiteURL == "" {
		return route
	}
	return strings.TrimRight(c.SiteURL, "/") + "/" + route
}

type pushSub struct {
	ID       int64  `json:"id"`
	Endpoint string `json:"-"`
	P256dh   string `json:"-"`
	Auth     string `json:"-"`
	Name     string `json:"name"`
	Host     string `json:"host"`
	Key      string `json:"key"` // 推送地址的 sha256 前 16 位，浏览器用来认出“这就是我”
	Created  int64  `json:"created"`
	LastTS   int64  `json:"last_ts"`
	LastErr  string `json:"last_err"`
}

type notifyEvent struct {
	ID    int64  `json:"id"`
	TS    int64  `json:"ts"`
	Kind  string `json:"kind"`
	Title string `json:"title"`
	Body  string `json:"body"`
	URL   string `json:"url"`
	Sent  int    `json:"sent"`
	Err   string `json:"err"`
}

func (s *Server) pushSubs() ([]pushSub, error) {
	rs, err := s.st.db.Query(`SELECT id, endpoint, p256dh, auth, name, created, last_ts, last_err FROM push_subs ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := []pushSub{}
	for rs.Next() {
		var x pushSub
		if err := rs.Scan(&x.ID, &x.Endpoint, &x.P256dh, &x.Auth, &x.Name, &x.Created, &x.LastTS, &x.LastErr); err != nil {
			return nil, err
		}
		if u, err := url.Parse(x.Endpoint); err == nil {
			x.Host = u.Host
		}
		sum := sha256.Sum256([]byte(x.Endpoint))
		x.Key = hex.EncodeToString(sum[:8])
		out = append(out, x)
	}
	return out, rs.Err()
}

// ---------------------------------------------------------------- 发送

type pushPayload struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	URL   string `json:"url"`
	Tag   string `json:"tag"`
	TS    int64  `json:"ts"`
}

// notify 记一条事件并推给所有绑定的浏览器（后台发，不阻塞调用方）。
func (s *Server) notify(kind, title, body, route string) {
	c, err := s.notifyConfig()
	if err != nil {
		log.Printf("读通知设置失败: %v", err)
		return
	}
	link := c.siteLink(route)
	now := time.Now().Unix()
	res, err := s.st.db.Exec(`INSERT INTO notify_events (ts, kind, title, body, url) VALUES (?, ?, ?, ?, ?)`, now, kind, title, body, link)
	if err != nil {
		log.Printf("记录通知失败: %v", err)
		return
	}
	id, _ := res.LastInsertId()
	s.st.db.Exec(`DELETE FROM notify_events WHERE id <= ?`, id-500)
	log.Printf("通知[%s] %s：%s", kind, title, body)
	go s.deliver(id, pushPayload{Title: title, Body: body, URL: link, Tag: kind, TS: now}, c, "")
}

// deliver 推给所有订阅（only 非空时只推这一个），把结果记回事件和订阅。
// 推送服务说订阅失效（404 / 410）就删掉它。
func (s *Server) deliver(eventID int64, p pushPayload, c notifyConfig, only string) (int, []string) {
	return s.deliverOpt(eventID, p, c, only, true)
}

func (s *Server) deliverOpt(eventID int64, p pushPayload, c notifyConfig, only string, dropGone bool) (int, []string) {
	subs, err := s.pushSubs()
	if err != nil {
		log.Printf("读浏览器订阅失败: %v", err)
		return 0, []string{err.Error()}
	}
	payload, _ := json.Marshal(p)
	contact := c.SiteURL
	if !strings.HasPrefix(contact, "https://") {
		contact = "mailto:vastmon@localhost"
	}
	urgency := "normal"
	if p.Tag == "balance" || p.Tag == "instance-bad" {
		urgency = "high"
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sent := 0
	var errs []string
	for _, sub := range subs {
		if only != "" && sub.Endpoint != only {
			continue
		}
		wg.Add(1)
		go func(sub pushSub) {
			defer wg.Done()
			err := s.sendPush(sub, payload, contact, urgency)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case errors.Is(err, errSubGone) && !dropGone:
				errs = append(errs, sub.Name+"："+err.Error())
			case errors.Is(err, errSubGone):
				s.st.db.Exec(`DELETE FROM push_subs WHERE id = ?`, sub.ID)
				log.Printf("删除失效的浏览器订阅 %s: %v", sub.Name, err)
				errs = append(errs, sub.Name+"："+err.Error()+"，已删除")
			case err != nil:
				s.st.db.Exec(`UPDATE push_subs SET last_ts = ?, last_err = ? WHERE id = ?`, time.Now().Unix(), err.Error(), sub.ID)
				errs = append(errs, sub.Name+"："+err.Error())
			default:
				s.st.db.Exec(`UPDATE push_subs SET last_ts = ?, last_err = '' WHERE id = ?`, time.Now().Unix(), sub.ID)
				sent++
			}
		}(sub)
	}
	wg.Wait()
	if eventID > 0 {
		s.st.db.Exec(`UPDATE notify_events SET sent = ?, err = ? WHERE id = ?`, sent, strings.Join(errs, "；"), eventID)
	}
	return sent, errs
}

// ---------------------------------------------------------------- 检查

type notifyState struct {
	mu       sync.Mutex
	seeded   bool
	inst     map[int64]instSnap
	machines map[int64]bool // 自己的实例在哪些机器上（机器列表据此标“你在租”）
	lowSince time.Time      // 余额提醒上次发出的时间，零值 = 现在没在报警
	spendAt  time.Time      // 上次查今天花费的时间
	spendDay string         // 今天花费已经提醒过的 UTC 日期
}

type instSnap struct {
	status string
	name   string
}

func instName(x map[string]any) string {
	n := fmt.Sprintf("实例 %d · %d× %s", int64(num(x["id"])), num(x["num_gpus"]), str(x["gpu_name"]))
	if l := str(x["label"]); l != "" {
		n += " · " + l
	}
	return n
}

// hourlyCost 是实例现在每小时的花费：运行中按 dph_total，其它状态按 vast 给的
// instance.totalHour（启动中 / 停止时只收磁盘费）。
func hourlyCost(x map[string]any) float64 {
	if in, ok := x["instance"].(map[string]any); ok {
		if v, ok := in["totalHour"].(float64); ok && v >= 0 && str(x["actual_status"]) != "running" {
			return v
		}
	}
	if str(x["actual_status"]) == "running" {
		if v, ok := x["dph_total"].(float64); ok {
			return v
		}
	}
	if v, ok := x["storage_total_cost"].(float64); ok {
		return v
	}
	return 0
}

var statusWords = map[string]string{
	"running": "运行中", "loading": "启动中", "created": "已创建", "scheduling": "排队中", "rebooting": "重启中",
	"recycling": "重建中", "stopped": "已停止", "exited": "已退出", "offline": "离线",
}

func statusWord(s string) string {
	if w, ok := statusWords[s]; ok {
		return w
	}
	if s == "" {
		return "未知"
	}
	return s
}

func (s *Server) notifyLoop(ctx context.Context) {
	t := time.NewTicker(notifyEvery)
	defer t.Stop()
	for {
		s.notifyCheck()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// myMachines 是自己的实例所在的机器（每分钟随通知检查刷新一次）。
func (s *Server) myMachines() map[int64]bool {
	s.notifyState.mu.Lock()
	defer s.notifyState.mu.Unlock()
	return s.notifyState.machines
}

func (s *Server) notifyCheck() {
	c, err := s.notifyConfig()
	if err != nil {
		return
	}
	ins, err := s.cl.Instances()
	if err != nil {
		return
	}
	mine := map[int64]bool{}
	for _, x := range ins {
		mine[int64(num(x["machine_id"]))] = true
	}
	s.notifyState.mu.Lock()
	s.notifyState.machines = mine
	s.notifyState.mu.Unlock()
	burn := 0.0
	for _, x := range ins {
		burn += hourlyCost(x)
	}
	now := time.Now()

	ns := &s.notifyState
	ns.mu.Lock()
	defer ns.mu.Unlock()
	cur := map[int64]instSnap{}
	for _, x := range ins {
		cur[int64(num(x["id"]))] = instSnap{status: str(x["actual_status"]), name: instName(x)}
	}
	if ns.seeded && c.Instance {
		for id, x := range cur {
			prev, had := ns.inst[id]
			switch {
			case !had:
				s.notify("instance", "开机了", fmt.Sprintf("%s，状态：%s", x.name, statusWord(x.status)), "#/instances")
			case prev.status == x.status:
			case x.status == "running":
				s.notify("instance", "实例运行起来了", x.name, "#/instances")
			case x.status == "offline" || x.status == "exited":
				s.notify("instance-bad", "实例"+statusWord(x.status), fmt.Sprintf("%s（之前%s）", x.name, statusWord(prev.status)), "#/instances")
			case x.status == "stopped":
				s.notify("instance", "实例已停止", x.name+"（磁盘照常计费）", "#/instances")
			}
		}
		for id, prev := range ns.inst {
			if _, ok := cur[id]; !ok {
				s.notify("instance", "实例已销毁", prev.name, "#/instances")
			}
		}
	}
	ns.inst, ns.seeded = cur, true

	if c.BalanceBelow > 0 || c.BalanceHours > 0 {
		s.checkBalance(c, burn, now)
	}
	if c.DailySpend > 0 {
		s.checkDailySpend(c, burn, now)
	}
}

func fmtHours(h float64) string {
	switch {
	case math.IsInf(h, 1) || h > 24*365:
		return "很久"
	case h >= 48:
		return fmt.Sprintf("%.1f 天", h/24)
	default:
		return fmt.Sprintf("%.1f 小时", h)
	}
}

// checkBalance 余额低于设定值、或按现在的花费撑不过设定的小时数时提醒；
// 一直没恢复就每 6 小时再提醒一次。调用时持有 notifyState.mu。
func (s *Server) checkBalance(c notifyConfig, burn float64, now time.Time) {
	credit, err := s.cl.Credit()
	if err != nil {
		return
	}
	hours := math.Inf(1)
	if burn > 0 {
		hours = credit / burn
	}
	var why []string
	if c.BalanceBelow > 0 && credit < c.BalanceBelow {
		why = append(why, fmt.Sprintf("低于 $%.2f", c.BalanceBelow))
	}
	if c.BalanceHours > 0 && hours < c.BalanceHours {
		why = append(why, fmt.Sprintf("按现在 $%.3f/h 只够 %s", burn, fmtHours(hours)))
	}
	ns := &s.notifyState
	if len(why) == 0 {
		ns.lowSince = time.Time{}
		return
	}
	if !ns.lowSince.IsZero() && now.Sub(ns.lowSince) < balanceRemind {
		return
	}
	ns.lowSince = now
	s.notify("balance", fmt.Sprintf("余额不足：$%.2f", credit), strings.Join(why, "，")+"。到 vast 充值，或停掉不用的实例。", "#/instances")
}

// checkDailySpend 今天（UTC 日，和实例页“今天花费”同一个数，来自 vast 账单）花费
// 超过设定额度时提醒，每个 UTC 日只提醒一次。账单每 5 分钟查一次。调用时持有 notifyState.mu。
func (s *Server) checkDailySpend(c notifyConfig, burn float64, now time.Time) {
	ns := &s.notifyState
	if now.Sub(ns.spendAt) < spendCheckEvery {
		return
	}
	ns.spendAt = now
	day := now.UTC().Format("2006-01-02")
	if ns.spendDay == "" {
		s.st.Get("notify_spend_day", &ns.spendDay)
	}
	if ns.spendDay == day {
		return
	}
	start := now.Unix() - now.Unix()%86400
	charges, err := s.cl.Charges(start, start+86400)
	if err != nil {
		return
	}
	today := sum(charges)
	if today <= c.DailySpend {
		return
	}
	ns.spendDay = day
	s.st.Set("notify_spend_day", day)
	body := fmt.Sprintf("今天（UTC 日）vast 账单已经 $%.2f，超过了 $%.2f 的提醒额度。现在所有实例 $%.3f/h（≈$%.2f/天）。账单会比实际慢几分钟到一小时。", today, c.DailySpend, burn, burn*24)
	if credit, err := s.cl.Credit(); err == nil {
		body += fmt.Sprintf("余额 $%.2f。", credit)
	}
	s.notify("spend", fmt.Sprintf("今天花费 $%.2f，超过 $%.2f", today, c.DailySpend), body, "#/instances")
}

// ---------------------------------------------------------------- 接口

func (s *Server) notifySettings(w http.ResponseWriter, r *http.Request) {
	c, err := s.notifyConfig()
	if err != nil {
		s.fail(w, err)
		return
	}
	k, err := s.vapidKey()
	if err != nil {
		s.fail(w, err)
		return
	}
	pub, err := vapidPublic(k)
	if err != nil {
		s.fail(w, err)
		return
	}
	subs, err := s.pushSubs()
	if err != nil {
		s.fail(w, err)
		return
	}
	events, err := s.notifyEvents(30)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"config": c, "vapid_public": pub, "subs": subs, "events": events})
}

func (s *Server) notifyEvents(n int) ([]notifyEvent, error) {
	rs, err := s.st.db.Query(`SELECT id, ts, kind, title, body, url, sent, err FROM notify_events ORDER BY id DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := []notifyEvent{}
	for rs.Next() {
		var e notifyEvent
		if err := rs.Scan(&e.ID, &e.TS, &e.Kind, &e.Title, &e.Body, &e.URL, &e.Sent, &e.Err); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rs.Err()
}

func (s *Server) putNotifySettings(w http.ResponseWriter, r *http.Request) {
	var c notifyConfig
	if !decodeBody(w, r, &c) {
		return
	}
	c.SiteURL = strings.TrimRight(strings.TrimSpace(c.SiteURL), "/")
	if c.SiteURL != "" {
		u, err := url.Parse(c.SiteURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
			writeError(w, http.StatusBadRequest, "BAD_REQUEST", "站点 URL 要写成 https://域名[:端口][/路径]")
			return
		}
	}
	if c.BalanceBelow < 0 || c.BalanceHours < 0 || c.DailySpend < 0 {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "提醒额度不能是负的")
		return
	}
	if old, _ := s.notifyConfig(); c.DailySpend != old.DailySpend {
		// 额度改了：今天按新额度重新判断
		s.notifyState.mu.Lock()
		s.notifyState.spendDay, s.notifyState.spendAt = "", time.Time{}
		s.notifyState.mu.Unlock()
		s.st.Set("notify_spend_day", "")
	}
	if err := s.st.Set("notify", c); err != nil {
		s.fail(w, err)
		return
	}
	s.notifySettings(w, r)
}

// subscribePush 绑定一个浏览器（同一个推送地址再绑就更新密钥和名字）。
func (s *Server) subscribePush(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Endpoint string `json:"endpoint"`
		Keys     struct {
			P256dh string `json:"p256dh"`
			Auth   string `json:"auth"`
		} `json:"keys"`
		Name string `json:"name"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	u, err := url.Parse(body.Endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "推送地址不对")
		return
	}
	p, err1 := decodeB64(body.Keys.P256dh)
	a, err2 := decodeB64(body.Keys.Auth)
	if err1 != nil || err2 != nil || len(p) != 65 || len(a) != 16 {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "浏览器给的密钥不对")
		return
	}
	subs, err := s.pushSubs()
	if err != nil {
		s.fail(w, err)
		return
	}
	if len(subs) >= maxPushSubs {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", fmt.Sprintf("最多绑定 %d 个浏览器，先删掉不用的", maxPushSubs))
		return
	}
	name := truncRunes(strings.TrimSpace(body.Name), 60)
	if name == "" {
		name = "浏览器"
	}
	_, err = s.st.db.Exec(`INSERT INTO push_subs (endpoint, p256dh, auth, name, created) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(endpoint) DO UPDATE SET p256dh = excluded.p256dh, auth = excluded.auth, name = excluded.name, last_err = ''`,
		body.Endpoint, body.Keys.P256dh, body.Keys.Auth, name, time.Now().Unix())
	if err != nil {
		s.fail(w, err)
		return
	}
	// 刚建的订阅推送服务那边可能还没生效（FCM 会先回 410），欢迎消息重试几次，期间不删订阅。
	c, _ := s.notifyConfig()
	welcome := pushPayload{Title: "vastmon 通知已开启", Body: "这个浏览器以后会收到实例、余额和消费的通知。", URL: c.siteLink("#/settings"), Tag: "test", TS: time.Now().Unix()}
	var sent int
	var errs []string
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(3 * time.Second):
			}
		}
		if sent, errs = s.deliverOpt(0, welcome, c, body.Endpoint, false); sent > 0 {
			errs = nil
			break
		}
	}
	s.writeSubResult(w, sent, errs)
}

func (s *Server) writeSubResult(w http.ResponseWriter, sent int, errs []string) {
	subs, err := s.pushSubs()
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"subs": subs, "sent": sent, "errors": errs})
}

func (s *Server) deletePushSub(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("sid"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "ID 不对")
		return
	}
	if _, err := s.st.db.Exec(`DELETE FROM push_subs WHERE id = ?`, id); err != nil {
		s.fail(w, err)
		return
	}
	s.writeSubResult(w, 0, nil)
}

// unsubscribePush 按推送地址解绑（浏览器上点“关闭通知”时用）。
func (s *Server) unsubscribePush(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Endpoint string `json:"endpoint"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if _, err := s.st.db.Exec(`DELETE FROM push_subs WHERE endpoint = ?`, body.Endpoint); err != nil {
		s.fail(w, err)
		return
	}
	s.writeSubResult(w, 0, nil)
}

// testPush 发一条测试通知。
func (s *Server) testPush(w http.ResponseWriter, r *http.Request) {
	c, err := s.notifyConfig()
	if err != nil {
		s.fail(w, err)
		return
	}
	title, text, route := "vastmon 测试通知", "能看到这条，说明这个浏览器的通知是通的。", "#/settings"
	sent, errs := s.deliver(0, pushPayload{Title: title, Body: text, URL: c.siteLink(route), Tag: "test", TS: time.Now().Unix()}, c, "")
	s.writeSubResult(w, sent, errs)
}
