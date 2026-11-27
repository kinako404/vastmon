package panel

import (
	"context"
	"log"
	"time"
)

type Monitor struct {
	st       *Store
	cl       *Client
	disk     float64
	interval time.Duration
	trigger  chan struct{}
}

const catalogTTL = 6 * time.Hour

type catalog struct {
	TS    int64
	Names []string
}

func (m *Monitor) Poke() {
	select {
	case m.trigger <- struct{}{}:
	default:
	}
}

func (m *Monitor) Run(ctx context.Context) {
	t := time.NewTicker(m.interval)
	defer t.Stop()
	for {
		m.refreshCatalog()
		m.pollOnce()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-m.trigger:
		}
	}
}

func (m *Monitor) refreshCatalog() {
	var c catalog
	if _, err := m.st.Get("catalog", &c); err != nil {
		log.Printf("读取型号目录失败: %v", err)
	}
	if time.Since(time.Unix(c.TS, 0)) < catalogTTL {
		return
	}
	names, err := m.cl.GPUNames(m.disk)
	if err != nil {
		log.Printf("刷新型号目录失败: %v", err)
		return
	}
	if err := m.st.Set("catalog", catalog{TS: time.Now().Unix(), Names: names}); err != nil {
		log.Printf("保存型号目录失败: %v", err)
	}
}

func (m *Monitor) pollOnce() {
	f := loadFilter(m.st)
	ts := time.Now().Unix()
	offers, err := m.cl.Search(f.GPUs, f.NumGPUs, m.disk)
	if err == nil {
		err = m.st.SaveOffers(ts, offers)
	}
	msg := ""
	if err != nil {
		msg = err.Error()
		log.Printf("抓取失败: %v", err)
	} else {
		log.Printf("抓取 %d 台 %v", len(offers), f.GPUs)
	}
	if err == nil {
		// vast 同一接口大约每秒限一次
		time.Sleep(1500 * time.Millisecond)
		rented, rerr := m.cl.SearchRented(f.GPUs, f.NumGPUs, m.disk)
		if rerr == nil {
			rerr = m.st.SaveRented(ts, rented)
		}
		if rerr != nil {
			log.Printf("抓取被租走的机器失败: %v", rerr)
		}
	}
	if err := m.st.AddPoll(ts, len(offers), msg); err != nil {
		log.Printf("记录抓取失败: %v", err)
	}
	if err := CheckFavorites(m.st, m.cl, m.disk); err != nil {
		log.Printf("检查收藏机器失败: %v", err)
	}
}

func loadFilter(st *Store) Filter {
	f := defaultFilter()
	if _, err := st.Get("filter", &f); err != nil {
		log.Printf("读取过滤条件失败: %v", err)
	}
	if _, ok := sortKeys[f.Sort]; !ok {
		f.Sort, f.Desc = "est", false
	}
	return f
}
