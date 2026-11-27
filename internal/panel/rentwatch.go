package panel

import (
	"context"
	"log"
	"net/http"
	"time"
)

// 被租监控：标了的机器每 5 分钟查一次眼下有几张卡被租着，记录保留 30 天，
// 算近 24 小时 / 7 天 / 30 天的被租率。按机器号查 vast 每次都给全（不像按型号搜索那样随机抽一部分），
// 所以每个时间点的结果是准的。关掉监控只是不再采样，已有记录留着，30 天后自然过期。

const rentWatchSchema = `
CREATE TABLE IF NOT EXISTS rent_watch (
	machine_id INTEGER PRIMARY KEY, created INTEGER NOT NULL,
	checked_at INTEGER NOT NULL DEFAULT 0, check_err TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS rent_samples (
	machine_id INTEGER NOT NULL, ts INTEGER NOT NULL, total INTEGER NOT NULL, free INTEGER NOT NULL,
	PRIMARY KEY (machine_id, ts)
) WITHOUT ROWID;
`

const (
	rentStep   = 300        // 采样间隔（秒）
	rentKeep   = 30 * 86400 // 记录保留多久（秒）
	rentOffset = 20         // 每个 5 分钟整点后多少秒采样，错开整点
)

// RentStats 是一段时间里的被租统计。Rate = 被租的卡·次 ÷ 挂着的卡·次；
// 没挂出来（查不到任何报价）的采样不算进被租率，单独记在 Offline。
type RentStats struct {
	Samples int     `json:"samples"`
	Offline int     `json:"offline"`
	Rate    float64 `json:"rate"` // 0–1；没有挂出来的采样时为 -1
}

type RentWatch struct {
	MachineID int64  `json:"machine_id"`
	Created   int64  `json:"created"`
	CheckedAt int64  `json:"checked_at"`
	CheckErr  string `json:"check_err"`
	// 最近一次采样：Total 是整机卡数（0 = 没挂出来），Free 是能租的卡数。
	LastTS  int64     `json:"last_ts"`
	Total   int       `json:"total"`
	Free    int       `json:"free"`
	Since   int64     `json:"since"` // 保留期内最早一条记录
	Day     RentStats `json:"day"`
	Week    RentStats `json:"week"`
	Month   RentStats `json:"month"`
	GPUName string    `json:"gpu_name"`
	NumGPUs int       `json:"num_gpus"` // 机器列表里这台最近一条报价的卡数，链接详情页用
	Geo     string    `json:"geolocation"`
}

func (s *Store) rentWatchIDs() ([]int64, error) {
	rs, err := s.db.Query(`SELECT machine_id FROM rent_watch`)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []int64
	for rs.Next() {
		var id int64
		if err := rs.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rs.Err()
}

// RentWatches 列出开着监控的机器和它们的被租率。
func (s *Store) RentWatches() ([]RentWatch, error) {
	now := time.Now().Unix()
	rs, err := s.db.Query(`SELECT w.machine_id, w.created, w.checked_at, w.check_err,
		COALESCE(m.gpu_name, ''), COALESCE(m.num_gpus, 0), COALESCE(m.geo, '')
		FROM rent_watch w LEFT JOIN machines m ON m.rowid = (
			SELECT rowid FROM machines WHERE machine_id = w.machine_id
			ORDER BY MAX(last_seen, rented_seen) DESC LIMIT 1)
		ORDER BY w.created DESC`)
	if err != nil {
		return nil, err
	}
	out := []RentWatch{}
	idx := map[int64]int{}
	for rs.Next() {
		var w RentWatch
		if err := rs.Scan(&w.MachineID, &w.Created, &w.CheckedAt, &w.CheckErr, &w.GPUName, &w.NumGPUs, &w.Geo); err != nil {
			rs.Close()
			return nil, err
		}
		w.Day, w.Week, w.Month = RentStats{Rate: -1}, RentStats{Rate: -1}, RentStats{Rate: -1}
		idx[w.MachineID] = len(out)
		out = append(out, w)
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return nil, err
	}
	for _, p := range []struct {
		dst  func(*RentWatch) *RentStats
		span int64
	}{
		{func(w *RentWatch) *RentStats { return &w.Day }, 86400},
		{func(w *RentWatch) *RentStats { return &w.Week }, 7 * 86400},
		{func(w *RentWatch) *RentStats { return &w.Month }, rentKeep},
	} {
		rs, err := s.db.Query(`SELECT s.machine_id, COUNT(*), SUM(s.total = 0), SUM(s.total - s.free), SUM(s.total), MIN(s.ts)
			FROM rent_samples s JOIN rent_watch w ON w.machine_id = s.machine_id
			WHERE s.ts >= ? GROUP BY s.machine_id`, now-p.span)
		if err != nil {
			return nil, err
		}
		for rs.Next() {
			var id, since int64
			var n, off, rented, total int
			if err := rs.Scan(&id, &n, &off, &rented, &total, &since); err != nil {
				rs.Close()
				return nil, err
			}
			i, ok := idx[id]
			if !ok {
				continue
			}
			st := p.dst(&out[i])
			st.Samples, st.Offline = n, off
			if total > 0 {
				st.Rate = float64(rented) / float64(total)
			}
			if p.span == rentKeep {
				out[i].Since = since
			}
		}
		rs.Close()
		if err := rs.Err(); err != nil {
			return nil, err
		}
	}
	rs, err = s.db.Query(`SELECT s.machine_id, s.ts, s.total, s.free FROM rent_samples s
		JOIN (SELECT machine_id, MAX(ts) AS ts FROM rent_samples GROUP BY machine_id) l
		ON l.machine_id = s.machine_id AND l.ts = s.ts`)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	for rs.Next() {
		var id, ts int64
		var total, free int
		if err := rs.Scan(&id, &ts, &total, &free); err != nil {
			return nil, err
		}
		if i, ok := idx[id]; ok {
			out[i].LastTS, out[i].Total, out[i].Free = ts, total, free
		}
	}
	return out, rs.Err()
}

// RentSeries 是一台机器保留期内的逐个采样，按 Step 对齐，从 From 开始；
// Total[i] 为 -1 表示那个时间点没有采样（监控没开或查询失败）。
type RentSeries struct {
	From  int64 `json:"from"`
	Step  int64 `json:"step"`
	Total []int `json:"total"`
	Free  []int `json:"free"`
}

func (s *Store) RentSeries(machineID int64) (RentSeries, error) {
	now := time.Now().Unix()
	from := (now-rentKeep)/rentStep*rentStep + rentStep
	n := int((now-from)/rentStep) + 1
	out := RentSeries{From: from, Step: rentStep, Total: make([]int, n), Free: make([]int, n)}
	for i := range out.Total {
		out.Total[i] = -1
	}
	rs, err := s.db.Query(`SELECT ts, total, free FROM rent_samples WHERE machine_id = ? AND ts >= ?`, machineID, from)
	if err != nil {
		return out, err
	}
	defer rs.Close()
	for rs.Next() {
		var ts int64
		var total, free int
		if err := rs.Scan(&ts, &total, &free); err != nil {
			return out, err
		}
		if i := int((ts - from) / rentStep); i >= 0 && i < n {
			out.Total[i], out.Free[i] = total, free
		}
	}
	return out, rs.Err()
}

// SampleRent 查一次这些机器眼下的报价并记一条采样：能租的报价里最大的卡数是空着的卡，
// 所有报价（能租的和被租着的）里最大的卡数是整机卡数；一条报价都没有算没挂出来。
func SampleRent(st *Store, cl *Client, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	ts := time.Now().Unix() / rentStep * rentStep
	now := time.Now().Unix()
	total := map[int64]int{}
	free := map[int64]int{}
	var err error
	for _, rentable := range []bool{true, false} {
		if !rentable {
			time.Sleep(1500 * time.Millisecond) // vast 同一接口大约每秒限一次
		}
		var offers []Offer
		offers, err = cl.search(map[string]any{
			"machine_id": map[string]any{"in": ids},
			"rentable":   map[string]any{"eq": rentable},
			"type":       "on-demand",
			"limit":      5000,
		})
		if err != nil {
			break
		}
		for _, o := range offers {
			total[o.MachineID] = max(total[o.MachineID], o.NumGPUs)
			if rentable {
				free[o.MachineID] = max(free[o.MachineID], o.NumGPUs)
			}
		}
	}
	if err != nil {
		if _, e := st.db.Exec(`UPDATE rent_watch SET checked_at = ?, check_err = ? WHERE machine_id IN (`+placeholders(len(ids))+`)`,
			append([]any{now, err.Error()}, int64Args(ids)...)...); e != nil {
			log.Printf("记录被租监控失败: %v", e)
		}
		return err
	}
	tx, err := st.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO rent_samples (machine_id, ts, total, free) VALUES (?, ?, ?, ?)`,
			id, ts, total[id], free[id]); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE rent_watch SET checked_at = ?, check_err = '' WHERE machine_id = ?`, now, id); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM rent_samples WHERE ts < ?`, now-rentKeep); err != nil {
		return err
	}
	return tx.Commit()
}

func placeholders(n int) string {
	b := make([]byte, 0, 2*n)
	for i := 0; i < n; i++ {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, '?')
	}
	return string(b)
}

func int64Args(ids []int64) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}

// rentWatchLoop 在每个 5 分钟整点后 rentOffset 秒采一次样。
func (s *Server) rentWatchLoop(ctx context.Context) {
	for {
		now := time.Now().Unix()
		next := (now-rentOffset)/rentStep*rentStep + rentStep + rentOffset
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(next-now) * time.Second):
		}
		ids, err := s.st.rentWatchIDs()
		if err != nil {
			log.Printf("读取被租监控列表失败: %v", err)
			continue
		}
		if err := SampleRent(s.st, s.cl, ids); err != nil {
			log.Printf("被租监控采样失败: %v", err)
		}
	}
}

func (s *Server) writeRentWatches(w http.ResponseWriter) {
	ws, err := s.st.RentWatches()
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"watches": ws, "step_sec": rentStep, "keep_days": rentKeep / 86400})
}

func (s *Server) rentWatches(w http.ResponseWriter, r *http.Request) {
	s.writeRentWatches(w)
}

func (s *Server) addRentWatch(w http.ResponseWriter, r *http.Request) {
	id, ok := favoriteID(w, r)
	if !ok {
		return
	}
	if _, err := s.st.db.Exec(`INSERT OR IGNORE INTO rent_watch (machine_id, created) VALUES (?, ?)`, id, time.Now().Unix()); err != nil {
		s.fail(w, err)
		return
	}
	// 开了就先采一次，不用等到下一个 5 分钟。
	if err := SampleRent(s.st, s.cl, []int64{id}); err != nil {
		log.Printf("被租监控首次采样失败: %v", err)
	}
	s.writeRentWatches(w)
}

func (s *Server) removeRentWatch(w http.ResponseWriter, r *http.Request) {
	id, ok := favoriteID(w, r)
	if !ok {
		return
	}
	if _, err := s.st.db.Exec(`DELETE FROM rent_watch WHERE machine_id = ?`, id); err != nil {
		s.fail(w, err)
		return
	}
	s.writeRentWatches(w)
}

func (s *Server) rentSeries(w http.ResponseWriter, r *http.Request) {
	id, ok := favoriteID(w, r)
	if !ok {
		return
	}
	series, err := s.st.RentSeries(id)
	if err != nil {
		s.fail(w, err)
		return
	}
	var watching bool
	if err := s.st.db.QueryRow(`SELECT COUNT(*) > 0 FROM rent_watch WHERE machine_id = ?`, id).Scan(&watching); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"watching": watching, "series": series})
}
