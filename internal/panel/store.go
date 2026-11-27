package panel

import (
	"database/sql"
	"encoding/json"
	"errors"
	"math"

	_ "modernc.org/sqlite"
)

// vast 的搜索结果每台机器只给一条报价，但多卡机器每张卡一个报价 ID，
// 每次返回哪个 ID 是轮换的，所以按 (machine_id, num_gpus) 记录，id 存最近一次看到的报价。
const schema = `
CREATE TABLE IF NOT EXISTS machines (
	machine_id INTEGER, num_gpus INTEGER,
	id INTEGER, host_id INTEGER, gpu_name TEXT, gpu_ram REAL,
	dph REAL, min_bid REAL, inet_down REAL, inet_up REAL, down_tb REAL, up_tb REAL,
	cpu_name TEXT, cpu_total REAL, cpu_cores REAL, storage_cost REAL, storage_hour REAL, cpu_ram REAL, disk REAL, disk_bw REAL, reliability REAL, cuda REAL,
	ports INTEGER, duration REAL, geo TEXT, verification TEXT, dlperf REAL, flops REAL, power REAL,
	first_seen INTEGER, last_seen INTEGER, raw TEXT, rented_seen INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (machine_id, num_gpus)
);
CREATE TABLE IF NOT EXISTS history (
	machine_id INTEGER, num_gpus INTEGER, ts INTEGER, dph REAL, down_tb REAL, up_tb REAL
);
CREATE INDEX IF NOT EXISTS history_machine ON history(machine_id, num_gpus, ts);
CREATE TABLE IF NOT EXISTS polls (
	id INTEGER PRIMARY KEY AUTOINCREMENT, ts INTEGER, n INTEGER, err TEXT
);
CREATE TABLE IF NOT EXISTS settings (k TEXT PRIMARY KEY, v TEXT);
CREATE TABLE IF NOT EXISTS sessions (token_hash TEXT PRIMARY KEY, created INTEGER);
CREATE TABLE IF NOT EXISTS instance_actions (
	id INTEGER PRIMARY KEY AUTOINCREMENT, ts INTEGER, instance_id INTEGER, action TEXT, detail TEXT, err TEXT
);
CREATE TABLE IF NOT EXISTS rentals (
	id INTEGER PRIMARY KEY AUTOINCREMENT, ts INTEGER, machine_id INTEGER, num_gpus INTEGER,
	offer_id INTEGER, gpu_name TEXT, template_hash_id TEXT, template_name TEXT, disk REAL, dph REAL,
	label TEXT, instance_id INTEGER, err TEXT
);
`

const offerCols = `id, machine_id, host_id, gpu_name, num_gpus, gpu_ram, dph, min_bid, inet_down, inet_up,
	down_tb, up_tb, cpu_name, cpu_total, cpu_cores, storage_cost, storage_hour, cpu_ram, disk, disk_bw, reliability, cuda, ports, duration, geo, verification, dlperf, flops, power`

// Store 里 grace 是"在线"判定的宽限期（秒）：vast 单次搜索偶尔漏掉个别机器，
// 最近 grace 秒内出现过就算在线。
type Store struct {
	db    *sql.DB
	grace int64
}

// Row 的状态：Online = 宽限期内出现过能租的报价；Rented = 宽限期内没有能租的、
// 但看到过被人租着的；两个都不是 = 下架（主机撤了报价，或者很久没出现）。
// 能租优先：多卡机器每张卡一个单卡报价，租掉一张时同一台既有能租的也有被租的；
// vast 每次搜索又只随机给一部分报价，所以只要宽限期内见过能租的就算在线。
type Row struct {
	Offer
	FirstSeen  int64   `json:"first_seen"`
	LastSeen   int64   `json:"last_seen"`
	RentedSeen int64   `json:"rented_seen"`
	Online     bool    `json:"online"`
	Rented     bool    `json:"rented"`
	Mine       bool    `json:"mine"` // 被租走的是自己的实例
	New        bool    `json:"new"`
	EstDay     float64 `json:"est_day"`
}

func (r *Row) setState(online, rented int64) {
	r.Online = r.LastSeen >= online
	r.Rented = !r.Online && r.RentedSeen >= rented
}

type Poll struct {
	ID  int64  `json:"id"`
	TS  int64  `json:"ts"`
	N   int    `json:"n"`
	Err string `json:"err"`
}

type HistoryPoint struct {
	TS     int64   `json:"ts"`
	Dph    float64 `json:"dph"`
	DownTB float64 `json:"down_tb"`
	UpTB   float64 `json:"up_tb"`
}

func OpenStore(path string, grace int64) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema + favoritesSchema + blacklistSchema + benchmarksSchema + hostKeysSchema + setupSchema + baselinesSchema + downloadsSchema + notifySchema + scriptsSchema + rentWatchSchema); err != nil {
		return nil, err
	}
	if err := migrate(db); err != nil {
		return nil, err
	}
	st := &Store{db: db, grace: grace}
	if err := st.SeedBaselines(false); err != nil {
		return nil, err
	}
	return st, nil
}

// migrate 给旧库补上后加的列，并从已存的原始报价里回填。
func migrate(db *sql.DB) error {
	cols := map[string]bool{}
	rs, err := db.Query(`SELECT name FROM pragma_table_info('machines')`)
	if err != nil {
		return err
	}
	for rs.Next() {
		var name string
		if err := rs.Scan(&name); err != nil {
			rs.Close()
			return err
		}
		cols[name] = true
	}
	rs.Close()
	for _, c := range addedCols {
		if cols[c.name] {
			continue
		}
		if _, err := db.Exec(`ALTER TABLE machines ADD COLUMN ` + c.name + ` ` + c.def + `;
			UPDATE machines SET ` + c.name + ` = ` + c.backfill); err != nil {
			return err
		}
	}
	return nil
}

// addedCols 是建表之后才加的列：类型和从 raw 回填的表达式。
var addedCols = []struct{ name, def, backfill string }{
	{"cpu_name", "TEXT NOT NULL DEFAULT ''", "TRIM(COALESCE(json_extract(raw, '$.cpu_name'), ''))"},
	{"cpu_total", "REAL NOT NULL DEFAULT 0", "COALESCE(json_extract(raw, '$.cpu_cores'), 0)"},
	{"storage_cost", "REAL NOT NULL DEFAULT 0", "COALESCE(json_extract(raw, '$.storage_cost'), 0)"},
	{"storage_hour", "REAL NOT NULL DEFAULT 0", "COALESCE(json_extract(raw, '$.storage_total_cost'), 0)"},
	{"dlperf", "REAL NOT NULL DEFAULT 0", "COALESCE(json_extract(raw, '$.dlperf'), 0)"},
	{"flops", "REAL NOT NULL DEFAULT 0", "COALESCE(json_extract(raw, '$.total_flops'), 0)"},
	{"power", "REAL NOT NULL DEFAULT 0", "COALESCE(json_extract(raw, '$.gpu_max_power'), 0)"},
	{"rented_seen", "INTEGER NOT NULL DEFAULT 0", "0"},
}

func changed(a, b float64) bool { return math.Abs(a-b) > 1e-6 }

func (s *Store) SaveOffers(ts int64, offers []Offer) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, o := range offers {
		var dph, down, up float64
		var last int64
		err := tx.QueryRow(`SELECT dph, down_tb, up_tb, last_seen FROM machines WHERE machine_id = ? AND num_gpus = ?`,
			o.MachineID, o.NumGPUs).Scan(&dph, &down, &up, &last)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		// last == 0：之前只见过它被租着，这是第一次能租，价格历史从这里开始
		if err != nil || last == 0 || changed(dph, o.Dph) || changed(down, o.DownTB) || changed(up, o.UpTB) {
			if _, err := tx.Exec(`INSERT INTO history VALUES (?, ?, ?, ?, ?, ?)`,
				o.MachineID, o.NumGPUs, ts, o.Dph, o.DownTB, o.UpTB); err != nil {
				return err
			}
		}
		_, err = tx.Exec(`INSERT INTO machines (`+offerCols+`, first_seen, last_seen, raw)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(machine_id, num_gpus) DO UPDATE SET
				id=excluded.id, host_id=excluded.host_id, gpu_name=excluded.gpu_name,
				gpu_ram=excluded.gpu_ram, dph=excluded.dph, min_bid=excluded.min_bid,
				inet_down=excluded.inet_down, inet_up=excluded.inet_up, down_tb=excluded.down_tb, up_tb=excluded.up_tb,
				cpu_name=excluded.cpu_name, cpu_total=excluded.cpu_total, cpu_cores=excluded.cpu_cores,
				storage_cost=excluded.storage_cost, storage_hour=excluded.storage_hour, cpu_ram=excluded.cpu_ram, disk=excluded.disk, disk_bw=excluded.disk_bw,
				reliability=excluded.reliability, cuda=excluded.cuda, ports=excluded.ports, duration=excluded.duration,
				geo=excluded.geo, verification=excluded.verification, dlperf=excluded.dlperf, flops=excluded.flops, power=excluded.power, last_seen=excluded.last_seen, raw=excluded.raw`,
			o.ID, o.MachineID, o.HostID, o.GPUName, o.NumGPUs, o.GPURam, o.Dph, o.MinBid, o.InetDown, o.InetUp,
			o.DownTB, o.UpTB, o.CPUName, o.CPUTotal, o.CPUCores, o.StorageCost, o.StorageHour, o.CPURam, o.Disk, o.DiskBW, o.Reliability, o.CUDA, o.Ports, o.Duration,
			o.Geo, o.Verification, o.DLPerf, o.TFLOPS, o.PowerW, ts, ts, o.Raw)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SaveRented 记下这一轮看到被人租着的报价：没见过的机器新建一行（last_seen = 0，
// 从来没能租过）；宽限期内还能租的行只记 rented_seen，报价信息保留能租那条的；
// 其它行连报价信息一起更新。被租着的价格不进价格历史。
func (s *Store) SaveRented(ts int64, offers []Offer) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, o := range offers {
		var last int64
		err := tx.QueryRow(`SELECT last_seen FROM machines WHERE machine_id = ? AND num_gpus = ?`, o.MachineID, o.NumGPUs).Scan(&last)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			_, err = tx.Exec(`INSERT INTO machines (`+offerCols+`, first_seen, last_seen, raw, rented_seen)
				VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				o.ID, o.MachineID, o.HostID, o.GPUName, o.NumGPUs, o.GPURam, o.Dph, o.MinBid, o.InetDown, o.InetUp,
				o.DownTB, o.UpTB, o.CPUName, o.CPUTotal, o.CPUCores, o.StorageCost, o.StorageHour, o.CPURam, o.Disk, o.DiskBW, o.Reliability, o.CUDA, o.Ports, o.Duration,
				o.Geo, o.Verification, o.DLPerf, o.TFLOPS, o.PowerW, ts, 0, o.Raw, ts)
		case err != nil:
		case last >= ts-s.grace:
			_, err = tx.Exec(`UPDATE machines SET rented_seen = ? WHERE machine_id = ? AND num_gpus = ?`, ts, o.MachineID, o.NumGPUs)
		default:
			_, err = tx.Exec(`UPDATE machines SET id=?, host_id=?, gpu_name=?, gpu_ram=?, dph=?, min_bid=?, inet_down=?, inet_up=?,
				down_tb=?, up_tb=?, cpu_name=?, cpu_total=?, cpu_cores=?, storage_cost=?, storage_hour=?, cpu_ram=?, disk=?, disk_bw=?,
				reliability=?, cuda=?, ports=?, duration=?, geo=?, verification=?, dlperf=?, flops=?, power=?, raw=?, rented_seen=?
				WHERE machine_id = ? AND num_gpus = ?`,
				o.ID, o.HostID, o.GPUName, o.GPURam, o.Dph, o.MinBid, o.InetDown, o.InetUp,
				o.DownTB, o.UpTB, o.CPUName, o.CPUTotal, o.CPUCores, o.StorageCost, o.StorageHour, o.CPURam, o.Disk, o.DiskBW,
				o.Reliability, o.CUDA, o.Ports, o.Duration, o.Geo, o.Verification, o.DLPerf, o.TFLOPS, o.PowerW, o.Raw, ts,
				o.MachineID, o.NumGPUs)
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) AddPoll(ts int64, n int, errMsg string) error {
	_, err := s.db.Exec(`INSERT INTO polls (ts, n, err) VALUES (?, ?, ?)`, ts, n, errMsg)
	return err
}

func (s *Store) LastPoll() (Poll, error) {
	var p Poll
	err := s.db.QueryRow(`SELECT id, ts, n, err FROM polls ORDER BY id DESC LIMIT 1`).Scan(&p.ID, &p.TS, &p.N, &p.Err)
	if errors.Is(err, sql.ErrNoRows) {
		return p, nil
	}
	return p, err
}

func (s *Store) Polls(limit int) ([]Poll, error) {
	rs, err := s.db.Query(`SELECT id, ts, n, err FROM polls ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := []Poll{}
	for rs.Next() {
		var p Poll
		if err := rs.Scan(&p.ID, &p.TS, &p.N, &p.Err); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rs.Err()
}

// rentedWindow：多久内看到过被租着就算“被租走”。vast 每次只随机给一部分
// 被租着的报价（实测 RTX PRO 6000 WS 单卡约 150 台里每次给 30 台左右），
// 宽限期那 3 轮只能看到一半；租约一般按小时算，放宽到 1 小时差不多都能看到。
const rentedWindow = 3600

// windows 返回“在线”和“被租走”两个判定的起点（最近一次成功抓取往前推）。
func (s *Store) windows() (online, rented int64, err error) {
	var ts sql.NullInt64
	err = s.db.QueryRow(`SELECT MAX(ts) FROM polls WHERE err = ''`).Scan(&ts)
	if !ts.Valid {
		return math.MaxInt64, math.MaxInt64, err
	}
	return ts.Int64 - s.grace, ts.Int64 - max(s.grace, rentedWindow), err
}

func scanOffer(sc interface{ Scan(...any) error }, o *Offer, extra ...any) error {
	return sc.Scan(append([]any{&o.ID, &o.MachineID, &o.HostID, &o.GPUName, &o.NumGPUs, &o.GPURam, &o.Dph,
		&o.MinBid, &o.InetDown, &o.InetUp, &o.DownTB, &o.UpTB, &o.CPUName, &o.CPUTotal, &o.CPUCores, &o.StorageCost, &o.StorageHour, &o.CPURam, &o.Disk, &o.DiskBW,
		&o.Reliability, &o.CUDA, &o.Ports, &o.Duration, &o.Geo, &o.Verification, &o.DLPerf, &o.TFLOPS, &o.PowerW}, extra...)...)
}

func (s *Store) Rows() ([]Row, error) {
	since, rentedSince, err := s.windows()
	if err != nil {
		return nil, err
	}
	rs, err := s.db.Query(`SELECT ` + offerCols + `, first_seen, last_seen, rented_seen FROM machines`)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []Row
	for rs.Next() {
		var r Row
		if err := scanOffer(rs, &r.Offer, &r.FirstSeen, &r.LastSeen, &r.RentedSeen); err != nil {
			return nil, err
		}
		r.setState(since, rentedSince)
		out = append(out, r)
	}
	return out, rs.Err()
}

func (s *Store) Row(machineID int64, numGPUs int) (Row, error) {
	var r Row
	err := scanOffer(s.db.QueryRow(`SELECT `+offerCols+`, first_seen, last_seen, rented_seen, raw FROM machines
		WHERE machine_id = ? AND num_gpus = ?`, machineID, numGPUs), &r.Offer, &r.FirstSeen, &r.LastSeen, &r.RentedSeen, &r.Raw)
	if err != nil {
		return r, err
	}
	since, rentedSince, err := s.windows()
	r.setState(since, rentedSince)
	return r, err
}

func (s *Store) History(machineID int64, numGPUs int) ([]HistoryPoint, error) {
	rs, err := s.db.Query(`SELECT ts, dph, down_tb, up_tb FROM history
		WHERE machine_id = ? AND num_gpus = ? ORDER BY ts DESC`, machineID, numGPUs)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := []HistoryPoint{}
	for rs.Next() {
		var h HistoryPoint
		if err := rs.Scan(&h.TS, &h.Dph, &h.DownTB, &h.UpTB); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rs.Err()
}

func (s *Store) Get(k string, dest any) (bool, error) {
	var v string
	err := s.db.QueryRow(`SELECT v FROM settings WHERE k = ?`, k).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal([]byte(v), dest)
}

func (s *Store) Set(k string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO settings (k, v) VALUES (?, ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v`, k, string(b))
	return err
}

// Rental 是一次开机尝试，成功时 InstanceID 非 0，失败时 Err 是原因。
type Rental struct {
	ID           int64   `json:"id"`
	TS           int64   `json:"ts"`
	MachineID    int64   `json:"machine_id"`
	NumGPUs      int     `json:"num_gpus"`
	OfferID      int64   `json:"offer_id"`
	GPUName      string  `json:"gpu_name"`
	TemplateHash string  `json:"template_hash_id"`
	TemplateName string  `json:"template_name"`
	Disk         float64 `json:"disk"`
	Dph          float64 `json:"dph"`
	Label        string  `json:"label"`
	InstanceID   int64   `json:"instance_id"`
	Err          string  `json:"err"`
}

func (s *Store) AddRental(r Rental) error {
	_, err := s.db.Exec(`INSERT INTO rentals (ts, machine_id, num_gpus, offer_id, gpu_name, template_hash_id,
		template_name, disk, dph, label, instance_id, err) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.TS, r.MachineID, r.NumGPUs, r.OfferID, r.GPUName, r.TemplateHash, r.TemplateName, r.Disk, r.Dph,
		r.Label, r.InstanceID, r.Err)
	return err
}

func (s *Store) Rentals(limit int) ([]Rental, error) {
	rs, err := s.db.Query(`SELECT id, ts, machine_id, num_gpus, offer_id, gpu_name, template_hash_id, template_name,
		disk, dph, label, instance_id, err FROM rentals ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := []Rental{}
	for rs.Next() {
		var r Rental
		if err := rs.Scan(&r.ID, &r.TS, &r.MachineID, &r.NumGPUs, &r.OfferID, &r.GPUName, &r.TemplateHash,
			&r.TemplateName, &r.Disk, &r.Dph, &r.Label, &r.InstanceID, &r.Err); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rs.Err()
}

// InstanceAction 是面板对实例做过的一次操作（启动、停止、销毁等）。
type InstanceAction struct {
	ID         int64  `json:"id"`
	TS         int64  `json:"ts"`
	InstanceID int64  `json:"instance_id"`
	Action     string `json:"action"`
	Detail     string `json:"detail"`
	Err        string `json:"err"`
}

func (s *Store) AddAction(a InstanceAction) error {
	_, err := s.db.Exec(`INSERT INTO instance_actions (ts, instance_id, action, detail, err) VALUES (?, ?, ?, ?, ?)`,
		a.TS, a.InstanceID, a.Action, a.Detail, a.Err)
	return err
}

func (s *Store) Actions(limit int) ([]InstanceAction, error) {
	rs, err := s.db.Query(`SELECT id, ts, instance_id, action, detail, err FROM instance_actions ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := []InstanceAction{}
	for rs.Next() {
		var a InstanceAction
		if err := rs.Scan(&a.ID, &a.TS, &a.InstanceID, &a.Action, &a.Detail, &a.Err); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rs.Err()
}
