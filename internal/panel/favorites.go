package panel

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// 收藏的机器：开过机、觉得好用的机器记下来，监控每轮顺带查一次它们现在能不能租。

const favoritesSchema = `
CREATE TABLE IF NOT EXISTS favorites (
	machine_id INTEGER PRIMARY KEY, num_gpus INTEGER, gpu_name TEXT, geo TEXT, cpu_name TEXT,
	note TEXT NOT NULL DEFAULT '', source TEXT NOT NULL DEFAULT '', created INTEGER,
	checked_at INTEGER NOT NULL DEFAULT 0, last_avail_at INTEGER NOT NULL DEFAULT 0,
	offer TEXT NOT NULL DEFAULT '', check_err TEXT NOT NULL DEFAULT ''
);
`

type Favorite struct {
	MachineID   int64  `json:"machine_id"`
	NumGPUs     int    `json:"num_gpus"`
	GPUName     string `json:"gpu_name"`
	Geo         string `json:"geolocation"`
	CPUName     string `json:"cpu_name"`
	Note        string `json:"note"`
	Source      string `json:"source"`
	Created     int64  `json:"created"`
	CheckedAt   int64  `json:"checked_at"`
	LastAvailAt int64  `json:"last_avail_at"`
	CheckErr    string `json:"check_err"`
	// Offer 是最近一次检查时最便宜的可租报价；nil 表示当时租不了。
	Offer *Offer `json:"offer"`
}

func (s *Store) Favorites() ([]Favorite, error) {
	rs, err := s.db.Query(`SELECT machine_id, num_gpus, gpu_name, geo, cpu_name, note, source, created,
		checked_at, last_avail_at, offer, check_err FROM favorites ORDER BY created DESC`)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := []Favorite{}
	for rs.Next() {
		var f Favorite
		var offer string
		if err := rs.Scan(&f.MachineID, &f.NumGPUs, &f.GPUName, &f.Geo, &f.CPUName, &f.Note, &f.Source, &f.Created,
			&f.CheckedAt, &f.LastAvailAt, &offer, &f.CheckErr); err != nil {
			return nil, err
		}
		if offer != "" {
			var o Offer
			if err := json.Unmarshal([]byte(offer), &o); err == nil {
				f.Offer = &o
			}
		}
		out = append(out, f)
	}
	return out, rs.Err()
}

// AddFavorite 收藏一台机器并把它移出黑名单；已收藏时只补全空着的信息，给了新原因才覆盖原因，收藏时间不变。
func (s *Store) AddFavorite(f Favorite) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM blacklist WHERE machine_id = ?`, f.MachineID); err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO favorites (machine_id, num_gpus, gpu_name, geo, cpu_name, note, source, created)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(machine_id) DO UPDATE SET
			gpu_name = COALESCE(NULLIF(excluded.gpu_name, ''), gpu_name),
			geo = COALESCE(NULLIF(excluded.geo, ''), geo),
			cpu_name = COALESCE(NULLIF(excluded.cpu_name, ''), cpu_name),
			note = COALESCE(NULLIF(excluded.note, ''), note)`,
		f.MachineID, f.NumGPUs, f.GPUName, f.Geo, f.CPUName, f.Note, f.Source, f.Created)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RemoveFavorite(machineID int64) error {
	_, err := s.db.Exec(`DELETE FROM favorites WHERE machine_id = ?`, machineID)
	return err
}

func (s *Store) SetFavoriteNote(machineID int64, note string) (bool, error) {
	res, err := s.db.Exec(`UPDATE favorites SET note = ? WHERE machine_id = ?`, note, machineID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// FavoriteMark 是机器列表上“已收藏”标记要用的：机器号和收藏原因。
type FavoriteMark struct {
	MachineID int64  `json:"machine_id"`
	Note      string `json:"note"`
}

func (s *Store) FavoriteMarks() ([]FavoriteMark, error) {
	rs, err := s.db.Query(`SELECT machine_id, note FROM favorites`)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := []FavoriteMark{}
	for rs.Next() {
		var m FavoriteMark
		if err := rs.Scan(&m.MachineID, &m.Note); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rs.Err()
}

// CheckFavorites 一次查完所有收藏机器眼下的报价，把结果写回表里。
func CheckFavorites(st *Store, cl *Client, disk float64) error {
	favs, err := st.Favorites()
	if err != nil || len(favs) == 0 {
		return err
	}
	ids := make([]int64, len(favs))
	for i, f := range favs {
		ids[i] = f.MachineID
	}
	now := time.Now().Unix()
	offers, err := cl.search(map[string]any{
		"machine_id":        map[string]any{"in": ids},
		"rentable":          map[string]any{"eq": true},
		"rented":            map[string]any{"eq": false},
		"type":              "on-demand",
		"limit":             1000,
		"allocated_storage": disk,
		"order":             [][]string{{"dph_total", "asc"}},
	})
	if err != nil {
		if _, e := st.db.Exec(`UPDATE favorites SET checked_at = ?, check_err = ?`, now, err.Error()); e != nil {
			log.Printf("记录收藏检查失败: %v", e)
		}
		return err
	}
	// 每台机器取最便宜的报价，优先和收藏时同样的卡数。
	best := map[int64]Offer{}
	want := map[int64]int{}
	for _, f := range favs {
		want[f.MachineID] = f.NumGPUs
	}
	for _, o := range offers {
		cur, ok := best[o.MachineID]
		match := o.NumGPUs == want[o.MachineID]
		curMatch := ok && cur.NumGPUs == want[o.MachineID]
		if !ok || (match && !curMatch) || (match == curMatch && o.Dph < cur.Dph) {
			best[o.MachineID] = o
		}
	}
	tx, err := st.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, f := range favs {
		o, ok := best[f.MachineID]
		if !ok {
			if _, err := tx.Exec(`UPDATE favorites SET checked_at = ?, offer = '', check_err = '' WHERE machine_id = ?`, now, f.MachineID); err != nil {
				return err
			}
			continue
		}
		raw := o.Raw
		if raw == "" {
			b, err := json.Marshal(o)
			if err != nil {
				return err
			}
			raw = string(b)
		}
		if _, err := tx.Exec(`UPDATE favorites SET checked_at = ?, last_avail_at = ?, offer = ?, check_err = '',
			gpu_name = ?, geo = ?, cpu_name = ? WHERE machine_id = ?`,
			now, now, raw, o.GPUName, o.Geo, o.CPUName, f.MachineID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Server) favorites(w http.ResponseWriter, r *http.Request) {
	s.writeMarks(w)
}

func (s *Server) addFavorite(w http.ResponseWriter, r *http.Request) {
	var f Favorite
	if !decodeBody(w, r, &f) {
		return
	}
	if f.MachineID <= 0 {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "缺少机器号")
		return
	}
	if f.NumGPUs <= 0 {
		f.NumGPUs = 1
	}
	f.Source = strings.TrimSpace(f.Source)
	f.Note = strings.TrimSpace(f.Note)
	if len([]rune(f.Note)) > 200 {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "收藏原因最多 200 个字符")
		return
	}
	f.Created = time.Now().Unix()
	if err := s.st.AddFavorite(f); err != nil {
		s.fail(w, err)
		return
	}
	go func() {
		if err := CheckFavorites(s.st, s.cl, s.mon.disk); err != nil {
			log.Printf("检查收藏机器失败: %v", err)
		}
	}()
	s.writeMarks(w)
}

func favoriteID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("mid"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "机器号不对")
		return 0, false
	}
	return id, true
}

func (s *Server) removeFavorite(w http.ResponseWriter, r *http.Request) {
	id, ok := favoriteID(w, r)
	if !ok {
		return
	}
	if err := s.st.RemoveFavorite(id); err != nil {
		s.fail(w, err)
		return
	}
	s.writeMarks(w)
}

func (s *Server) favoriteNote(w http.ResponseWriter, r *http.Request) {
	id, ok := favoriteID(w, r)
	if !ok {
		return
	}
	var body struct {
		Note string `json:"note"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	note := strings.TrimSpace(body.Note)
	if len([]rune(note)) > 200 {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "收藏原因最多 200 个字符")
		return
	}
	found, err := s.st.SetFavoriteNote(id, note)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "没有收藏这台机器")
		return
	}
	s.writeMarks(w)
}

func (s *Server) checkFavorites(w http.ResponseWriter, r *http.Request) {
	if err := CheckFavorites(s.st, s.cl, s.mon.disk); err != nil {
		writeError(w, http.StatusBadGateway, "VAST", "检查失败："+err.Error())
		return
	}
	s.writeMarks(w)
}
