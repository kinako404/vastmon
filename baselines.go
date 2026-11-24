package main

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// 性能基准数据库：每行是“一个 vast 型号 + 一个功耗档”。两类数据：
//   规格（官方 / TechPowerUp）：SM 数、显存、显存带宽、默认功耗、算力版本，用来核对身份；
//   实测参考（只放真测过的）：FP16 / BF16 / FP32 矩阵乘、显存拷贝，用来判断算力够不够。
// 算力检测结论“正常”时，前端可以把成绩“存为基准”（按型号 + 功耗档更新这一行）。

const baselinesSchema = `
CREATE TABLE IF NOT EXISTS perf_baselines (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	gpu_name TEXT NOT NULL, variant TEXT NOT NULL DEFAULT '', full_name TEXT NOT NULL DEFAULT '',
	power_w REAL NOT NULL DEFAULT 0, sms INTEGER NOT NULL DEFAULT 0, cc TEXT NOT NULL DEFAULT '',
	vram_gb REAL NOT NULL DEFAULT 0, mem_bw_gbs REAL NOT NULL DEFAULT 0, fp32_spec REAL NOT NULL DEFAULT 0,
	fp16 REAL NOT NULL DEFAULT 0, bf16 REAL NOT NULL DEFAULT 0, fp32 REAL NOT NULL DEFAULT 0, copy_gbs REAL NOT NULL DEFAULT 0,
	measured_source TEXT NOT NULL DEFAULT '', spec_source TEXT NOT NULL DEFAULT '', note TEXT NOT NULL DEFAULT '',
	builtin INTEGER NOT NULL DEFAULT 0, updated INTEGER NOT NULL DEFAULT 0
);
`

type Baseline struct {
	ID             int64   `json:"id"`
	GPUName        string  `json:"gpu_name"`  // vast 的型号名，比如 "RTX PRO 6000 WS"
	Variant        string  `json:"variant"`   // 同型号的不同版本，比如 "80GB"、"SXM2 32GB"
	FullName       string  `json:"full_name"` // 官方全名
	PowerW         float64 `json:"power_w"`   // 默认功耗 / 这组实测时的功耗上限
	SMs            int     `json:"sms"`
	CC             string  `json:"cc"`
	VRAMGB         float64 `json:"vram_gb"`
	MemBWGBs       float64 `json:"mem_bw_gbs"` // 规格显存带宽
	FP32Spec       float64 `json:"fp32_spec"`  // 规格 FP32 TFLOPS（非 tensor），只作参考
	FP16           float64 `json:"fp16"`       // 实测 FP16 矩阵乘 TFLOPS
	BF16           float64 `json:"bf16"`
	FP32           float64 `json:"fp32"`
	CopyGBs        float64 `json:"copy_gbs"`
	MeasuredSource string  `json:"measured_source"`
	SpecSource     string  `json:"spec_source"`
	Note           string  `json:"note"`
	Builtin        bool    `json:"builtin"`
	Updated        int64   `json:"updated"`
}

const baselineCols = `id, gpu_name, variant, full_name, power_w, sms, cc, vram_gb, mem_bw_gbs, fp32_spec,
	fp16, bf16, fp32, copy_gbs, measured_source, spec_source, note, builtin, updated`

func scanBaseline(sc interface{ Scan(...any) error }) (Baseline, error) {
	var b Baseline
	err := sc.Scan(&b.ID, &b.GPUName, &b.Variant, &b.FullName, &b.PowerW, &b.SMs, &b.CC, &b.VRAMGB, &b.MemBWGBs,
		&b.FP32Spec, &b.FP16, &b.BF16, &b.FP32, &b.CopyGBs, &b.MeasuredSource, &b.SpecSource, &b.Note, &b.Builtin, &b.Updated)
	return b, err
}

func (s *Store) Baselines() ([]Baseline, error) {
	rs, err := s.db.Query(`SELECT ` + baselineCols + ` FROM perf_baselines ORDER BY gpu_name, power_w DESC, variant`)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := []Baseline{}
	for rs.Next() {
		b, err := scanBaseline(rs)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rs.Err()
}

func (s *Store) SaveBaseline(b Baseline) (int64, error) {
	b.Updated = time.Now().Unix()
	args := []any{b.GPUName, b.Variant, b.FullName, b.PowerW, b.SMs, b.CC, b.VRAMGB, b.MemBWGBs, b.FP32Spec,
		b.FP16, b.BF16, b.FP32, b.CopyGBs, b.MeasuredSource, b.SpecSource, b.Note, b.Builtin, b.Updated}
	if b.ID > 0 {
		res, err := s.db.Exec(`UPDATE perf_baselines SET gpu_name=?, variant=?, full_name=?, power_w=?, sms=?, cc=?,
			vram_gb=?, mem_bw_gbs=?, fp32_spec=?, fp16=?, bf16=?, fp32=?, copy_gbs=?, measured_source=?, spec_source=?,
			note=?, builtin=?, updated=? WHERE id=?`, append(args, b.ID)...)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return 0, sql.ErrNoRows
		}
		return b.ID, nil
	}
	res, err := s.db.Exec(`INSERT INTO perf_baselines (gpu_name, variant, full_name, power_w, sms, cc, vram_gb,
		mem_bw_gbs, fp32_spec, fp16, bf16, fp32, copy_gbs, measured_source, spec_source, note, builtin, updated)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, args...)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// SeedBaselines 在内置数据换了版本（或要求恢复）时，把内置行整批换掉；
// 自己加的、改过的行（builtin = 0）一律保留。
func (s *Store) SeedBaselines(reset bool) error {
	var v int
	if _, err := s.Get("baselines_version", &v); err != nil {
		return err
	}
	if v == builtinBaselinesVersion && !reset {
		return nil
	}
	if _, err := s.db.Exec(`DELETE FROM perf_baselines WHERE builtin = 1`); err != nil {
		return err
	}
	for _, b := range builtinBaselines {
		b.Builtin = true
		if _, err := s.SaveBaseline(b); err != nil {
			return err
		}
	}
	return s.Set("baselines_version", builtinBaselinesVersion)
}

func validBaseline(b *Baseline) error {
	b.GPUName = strings.TrimSpace(b.GPUName)
	b.Variant = strings.TrimSpace(b.Variant)
	if b.GPUName == "" {
		return errors.New("要填 vast 型号名")
	}
	for _, v := range []float64{b.PowerW, b.VRAMGB, b.MemBWGBs, b.FP32Spec, b.FP16, b.BF16, b.FP32, b.CopyGBs} {
		if v < 0 || v > 1e6 {
			return errors.New("数值不对")
		}
	}
	return nil
}

func (s *Server) baselines(w http.ResponseWriter, r *http.Request) {
	list, err := s.st.Baselines()
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"baselines": list})
}

func (s *Server) saveBaseline(w http.ResponseWriter, r *http.Request) {
	var b Baseline
	if !decodeBody(w, r, &b) {
		return
	}
	if err := validBaseline(&b); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return
	}
	b.Builtin = false
	if _, err := s.st.SaveBaseline(b); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "这一行已经不在了")
			return
		}
		s.fail(w, err)
		return
	}
	s.baselines(w, r)
}

func (s *Server) deleteBaseline(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("bid"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "ID 不对")
		return
	}
	if _, err := s.st.db.Exec(`DELETE FROM perf_baselines WHERE id = ?`, id); err != nil {
		s.fail(w, err)
		return
	}
	s.baselines(w, r)
}

func (s *Server) resetBaselines(w http.ResponseWriter, r *http.Request) {
	if err := s.st.SeedBaselines(true); err != nil {
		s.fail(w, err)
		return
	}
	s.baselines(w, r)
}
