package panel

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// 快捷算力检测：把 bench.py 经 SSH 喂给实例上的 Python 跑，结果存进 benchmarks 表。
// 是否“假卡”的判断在前端做（和参考值、同型号历史最好成绩比）。

//go:embed bench.py
var benchScript []byte

const benchmarksSchema = `
CREATE TABLE IF NOT EXISTS benchmarks (
	id INTEGER PRIMARY KEY AUTOINCREMENT, ts INTEGER, instance_id INTEGER, machine_id INTEGER,
	gpu_name TEXT, num_gpus INTEGER, vast_gpu_ram REAL, vast_mem_bw REAL NOT NULL DEFAULT 0,
	vast_tflops REAL NOT NULL DEFAULT 0, geo TEXT, result TEXT NOT NULL DEFAULT '',
	err TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS benchmarks_machine ON benchmarks(machine_id, ts);
`

// benchRunner 先挑一个能在这张卡上跑 CUDA 的 Python（/opt/vastmon 留给检测专用的
// PyTorch，再试镜像自带的）；都不行就用第一个装了 torch 的跑，让脚本报出具体错误。
const benchRunner = `F=%d
for p in /opt/vastmon/bin/python /venv/main/bin/python python3; do
  command -v "$p" >/dev/null 2>&1 || continue
  if "$p" -c 'import torch; (torch.ones(2, device="cuda") * 2).sum().item()' </dev/null >/dev/null 2>&1; then
    exec env VASTMON_FORCE=$F "$p" -
  fi
done
for p in /venv/main/bin/python python3; do
  command -v "$p" >/dev/null 2>&1 && "$p" -c 'import torch' </dev/null >/dev/null 2>&1 && exec env VASTMON_FORCE=$F "$p" -
done
echo 'VASTMON_BENCH {"ok": false, "error": "实例上没找到装了 PyTorch 的 Python"}'
`

type Benchmark struct {
	ID         int64           `json:"id"`
	TS         int64           `json:"ts"`
	InstanceID int64           `json:"instance_id"`
	MachineID  int64           `json:"machine_id"`
	GPUName    string          `json:"gpu_name"`
	NumGPUs    int             `json:"num_gpus"`
	VastGPURam float64         `json:"vast_gpu_ram"`
	VastMemBW  float64         `json:"vast_mem_bw"`
	VastTFLOPS float64         `json:"vast_tflops"`
	Geo        string          `json:"geolocation"`
	Result     json.RawMessage `json:"result"`
	Err        string          `json:"err"`
}

func (s *Store) AddBenchmark(b Benchmark) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO benchmarks (ts, instance_id, machine_id, gpu_name, num_gpus, vast_gpu_ram,
		vast_mem_bw, vast_tflops, geo, result, err) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		b.TS, b.InstanceID, b.MachineID, b.GPUName, b.NumGPUs, b.VastGPURam, b.VastMemBW, b.VastTFLOPS, b.Geo,
		string(b.Result), b.Err)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) Benchmarks(machineID int64, limit int) ([]Benchmark, error) {
	q := `SELECT id, ts, instance_id, machine_id, gpu_name, num_gpus, vast_gpu_ram, vast_mem_bw, vast_tflops, geo,
		result, err FROM benchmarks`
	args := []any{}
	if machineID > 0 {
		q += ` WHERE machine_id = ?`
		args = append(args, machineID)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rs, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := []Benchmark{}
	for rs.Next() {
		var b Benchmark
		var result string
		if err := rs.Scan(&b.ID, &b.TS, &b.InstanceID, &b.MachineID, &b.GPUName, &b.NumGPUs, &b.VastGPURam,
			&b.VastMemBW, &b.VastTFLOPS, &b.Geo, &result, &b.Err); err != nil {
			return nil, err
		}
		if result == "" {
			result = "null"
		}
		b.Result = json.RawMessage(result)
		out = append(out, b)
	}
	return out, rs.Err()
}

// benchRunning 记着哪些实例正在测，同一台不并发跑。
var benchRunning sync.Map

func str(v any) string {
	s, _ := v.(string)
	return s
}

func (s *Server) runBench(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Force bool `json:"force"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if _, busy := benchRunning.LoadOrStore(id, true); busy {
		writeError(w, http.StatusConflict, "BUSY", "这台实例正在检测")
		return
	}
	defer benchRunning.Delete(id)

	x, err := s.findInstance(id)
	if err != nil {
		writeError(w, http.StatusBadGateway, "VAST", err.Error())
		return
	}
	if st := str(x["actual_status"]); st != "running" {
		writeError(w, http.StatusConflict, "NOT_RUNNING", fmt.Sprintf("实例还没运行（%s），起来后再测", st))
		return
	}
	if _, err := targetOf(x); err != nil {
		writeError(w, http.StatusConflict, "NO_SSH", err.Error())
		return
	}
	b := Benchmark{
		TS: time.Now().Unix(), InstanceID: id, MachineID: int64(num(x["machine_id"])), GPUName: str(x["gpu_name"]),
		NumGPUs: num(x["num_gpus"]), Geo: str(x["geolocation"]),
	}
	b.VastGPURam, _ = x["gpu_ram"].(float64)
	b.VastMemBW, _ = x["gpu_mem_bw"].(float64)
	b.VastTFLOPS, _ = x["total_flops"].(float64)
	force := 0
	if body.Force {
		force = 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	out, errOut, runErr := s.runOn(ctx, id, fmt.Sprintf(benchRunner, force), benchScript)
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "VASTMON_BENCH "); ok && json.Valid([]byte(rest)) {
			b.Result = json.RawMessage(rest)
		}
	}
	if b.Result == nil {
		msg := strings.TrimSpace(errOut)
		if msg == "" && runErr != nil {
			msg = runErr.Error()
		}
		if len(msg) > 800 {
			msg = msg[len(msg)-800:]
		}
		b.Err = "没拿到检测结果：" + msg
	}
	bid, err := s.st.AddBenchmark(b)
	if err != nil {
		s.fail(w, err)
		return
	}
	b.ID = bid
	log.Printf("实例 %d 算力检测完成（机器 %d）%s", id, b.MachineID, b.Err)
	if e := s.st.AddAction(InstanceAction{TS: b.TS, InstanceID: id, Action: "算力检测", Err: b.Err}); e != nil {
		log.Printf("记录实例操作失败: %v", e)
	}
	writeJSON(w, b)
}

func (s *Server) benchmarks(w http.ResponseWriter, r *http.Request) {
	var mid int64
	if v := r.URL.Query().Get("machine_id"); v != "" {
		fmt.Sscan(v, &mid)
	}
	list, err := s.st.Benchmarks(mid, 500)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"benchmarks": list})
}
