package main

import (
	"net/http"
	"strings"
	"time"
)

// 黑名单：开过、但不想再租的机器。和收藏互斥：加进一边就从另一边移出。

const blacklistSchema = `
CREATE TABLE IF NOT EXISTS blacklist (
	machine_id INTEGER PRIMARY KEY, num_gpus INTEGER, gpu_name TEXT, geo TEXT, cpu_name TEXT,
	note TEXT NOT NULL DEFAULT '', source TEXT NOT NULL DEFAULT '', created INTEGER
);
`

type Blacklisted struct {
	MachineID int64  `json:"machine_id"`
	NumGPUs   int    `json:"num_gpus"`
	GPUName   string `json:"gpu_name"`
	Geo       string `json:"geolocation"`
	CPUName   string `json:"cpu_name"`
	Note      string `json:"note"`
	Source    string `json:"source"`
	Created   int64  `json:"created"`
}

func (s *Store) Blacklist() ([]Blacklisted, error) {
	rs, err := s.db.Query(`SELECT machine_id, num_gpus, gpu_name, geo, cpu_name, note, source, created
		FROM blacklist ORDER BY created DESC`)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := []Blacklisted{}
	for rs.Next() {
		var b Blacklisted
		if err := rs.Scan(&b.MachineID, &b.NumGPUs, &b.GPUName, &b.Geo, &b.CPUName, &b.Note, &b.Source, &b.Created); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rs.Err()
}

// AddBlacklist 拉黑一台机器并把它移出收藏；已在黑名单时只补全空着的信息，给了新原因才覆盖。
func (s *Store) AddBlacklist(b Blacklisted) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM favorites WHERE machine_id = ?`, b.MachineID); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO blacklist (machine_id, num_gpus, gpu_name, geo, cpu_name, note, source, created)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(machine_id) DO UPDATE SET
			gpu_name = COALESCE(NULLIF(excluded.gpu_name, ''), gpu_name),
			geo = COALESCE(NULLIF(excluded.geo, ''), geo),
			cpu_name = COALESCE(NULLIF(excluded.cpu_name, ''), cpu_name),
			note = COALESCE(NULLIF(excluded.note, ''), note)`,
		b.MachineID, b.NumGPUs, b.GPUName, b.Geo, b.CPUName, b.Note, b.Source, b.Created); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RemoveBlacklist(machineID int64) error {
	_, err := s.db.Exec(`DELETE FROM blacklist WHERE machine_id = ?`, machineID)
	return err
}

func (s *Store) SetBlacklistNote(machineID int64, note string) (bool, error) {
	res, err := s.db.Exec(`UPDATE blacklist SET note = ? WHERE machine_id = ?`, note, machineID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// BlacklistMarks 是机器列表上标红要用的：机器号和拉黑原因。
func (s *Store) BlacklistMarks() ([]FavoriteMark, error) {
	rs, err := s.db.Query(`SELECT machine_id, note FROM blacklist`)
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

// writeMarks 回收藏和黑名单两份列表：改了一边，另一边也可能变了。
func (s *Server) writeMarks(w http.ResponseWriter) {
	favs, err := s.st.Favorites()
	if err != nil {
		s.fail(w, err)
		return
	}
	black, err := s.st.Blacklist()
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"favorites": favs, "blacklist": black, "interval_sec": int(s.mon.interval.Seconds())})
}

func (s *Server) blacklist(w http.ResponseWriter, r *http.Request) {
	s.writeMarks(w)
}

func (s *Server) addBlacklist(w http.ResponseWriter, r *http.Request) {
	var b Blacklisted
	if !decodeBody(w, r, &b) {
		return
	}
	if b.MachineID <= 0 {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "缺少机器号")
		return
	}
	if b.NumGPUs <= 0 {
		b.NumGPUs = 1
	}
	b.Source = strings.TrimSpace(b.Source)
	b.Note = strings.TrimSpace(b.Note)
	if len([]rune(b.Note)) > 200 {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "拉黑原因最多 200 个字符")
		return
	}
	b.Created = time.Now().Unix()
	if err := s.st.AddBlacklist(b); err != nil {
		s.fail(w, err)
		return
	}
	s.writeMarks(w)
}

func (s *Server) removeBlacklist(w http.ResponseWriter, r *http.Request) {
	id, ok := favoriteID(w, r)
	if !ok {
		return
	}
	if err := s.st.RemoveBlacklist(id); err != nil {
		s.fail(w, err)
		return
	}
	s.writeMarks(w)
}

func (s *Server) blacklistNote(w http.ResponseWriter, r *http.Request) {
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
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "拉黑原因最多 200 个字符")
		return
	}
	found, err := s.st.SetBlacklistNote(id, note)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "黑名单里没有这台机器")
		return
	}
	s.writeMarks(w)
}
