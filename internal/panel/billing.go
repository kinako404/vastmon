package panel

import (
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 账户花费：vast 账单按 UTC 日记，会延迟几分钟到一小时。四次请求的结果缓存 billingTTL。

const billingTTL = time.Minute

type spendItem struct {
	Amount float64 `json:"amount"`
	Desc   string  `json:"desc"`
}

type instanceSpend struct {
	ID     int64                `json:"id"`
	Label  string               `json:"label"`
	Start  int64                `json:"start"`
	End    int64                `json:"end"`
	Amount float64              `json:"amount"`
	Items  map[string]spendItem `json:"items"`
}

type billing struct {
	Credit    float64         `json:"credit"`
	Today     float64         `json:"today"`
	Week      float64         `json:"week"`
	Month     float64         `json:"month"`
	Instances []instanceSpend `json:"instances"`
	UpdatedAt int64           `json:"updated_at"`
}

type billingCache struct {
	mu  sync.Mutex
	val *billing
	at  time.Time
}

func sum(cs []Charge) float64 {
	t := 0.0
	for _, c := range cs {
		t += c.Amount
	}
	return t
}

func (s *Server) loadBilling() (*billing, error) {
	s.bill.mu.Lock()
	defer s.bill.mu.Unlock()
	if s.bill.val != nil && time.Since(s.bill.at) < billingTTL {
		return s.bill.val, nil
	}
	now := time.Now().Unix()
	today := now - now%86400
	end := today + 86400
	credit, err := s.cl.Credit()
	if err != nil {
		return nil, err
	}
	day, err := s.cl.Charges(today, end)
	if err != nil {
		return nil, err
	}
	week, err := s.cl.Charges(today-6*86400, end)
	if err != nil {
		return nil, err
	}
	month, err := s.cl.Charges(today-29*86400, end)
	if err != nil {
		return nil, err
	}
	b := &billing{Credit: credit, Today: sum(day), Week: sum(week), Month: sum(month), UpdatedAt: now, Instances: []instanceSpend{}}
	for _, c := range month {
		id, err := strconv.ParseInt(strings.TrimPrefix(c.Source, "instance-"), 10, 64)
		if c.Type != "instance" || err != nil {
			continue
		}
		is := instanceSpend{ID: id, Start: c.Start, End: c.End, Amount: c.Amount, Items: map[string]spendItem{}}
		if l, ok := c.Metadata["label"].(string); ok {
			is.Label = l
		}
		for _, it := range c.Items {
			prev := is.Items[it.Type]
			is.Items[it.Type] = spendItem{Amount: prev.Amount + it.Amount, Desc: it.Description}
		}
		b.Instances = append(b.Instances, is)
	}
	sort.Slice(b.Instances, func(i, j int) bool { return b.Instances[i].ID > b.Instances[j].ID })
	s.bill.val, s.bill.at = b, time.Now()
	return b, nil
}

func (s *Server) billing(w http.ResponseWriter, r *http.Request) {
	b, err := s.loadBilling()
	if err != nil {
		log.Printf("读取账单失败: %v", err)
		writeError(w, http.StatusBadGateway, "VAST", "读取账单失败："+err.Error())
		return
	}
	alert := 0.0
	if c, err := s.notifyConfig(); err == nil {
		alert = c.DailySpend
	}
	writeJSON(w, struct {
		*billing
		TodayAlert float64 `json:"today_alert"` // 设置里“今天花费超过多少提醒”，0 = 没设
	}{b, alert})
}
