package panel

import (
	"sort"
	"time"
)

// Filter 既是面板的过滤条件，也决定抓取哪些型号（GPUs）。
// 数值为 0 表示不限。
type Filter struct {
	GPUs      []string `json:"gpus"`
	NumGPUs   int      `json:"num_gpus"`
	MinVRAM   float64  `json:"min_vram"`    // GB
	MinDown   float64  `json:"min_down"`    // Mbps
	MinUp     float64  `json:"min_up"`      // Mbps
	MaxDownTB float64  `json:"max_down_tb"` // $/TB
	MaxUpTB   float64  `json:"max_up_tb"`   // $/TB
	MinCPU    float64  `json:"min_cpu"`     // 分到的核数
	MinRAM    float64  `json:"min_ram"`     // GB
	MaxDph    float64  `json:"max_dph"`     // $/h
	MinRel    float64  `json:"min_rel"`
	MinCUDA   float64  `json:"min_cuda"`
	MinDisk   float64  `json:"min_disk"` // GB
	MinDays   float64  `json:"min_days"`
	MinPorts  int      `json:"min_ports"`
	ExpDownGB float64  `json:"exp_down_gb"` // 预计每天下行流量
	ExpUpGB   float64  `json:"exp_up_gb"`   // 预计每天上行流量
	ShowGone  bool     `json:"show_gone"`
	// ShowRented：也列出眼下被别人租着的机器（看行情、等它空出来）
	ShowRented bool   `json:"show_rented"`
	Sort       string `json:"sort"`
	Desc       bool   `json:"desc"`
}

// defaultFilter 对应 cloud.vast.ai/create 上用户常用的那组条件。
func defaultFilter() Filter {
	return Filter{
		GPUs:      []string{"RTX PRO 6000 WS"},
		NumGPUs:   1,
		MinVRAM:   32,
		MinDown:   1024,
		MinUp:     1024,
		MinRel:    0.95,
		MinCUDA:   13,
		MinDisk:   96,
		MinDays:   1,
		MinPorts:  7,
		ExpDownGB: 100,
		ExpUpGB:   20,
		Sort:      "est",
	}
}

func (f Filter) Has(name string) bool {
	for _, g := range f.GPUs {
		if g == name {
			return true
		}
	}
	return false
}

func (f Filter) Match(r Row) bool {
	o := r.Offer
	switch {
	case r.Rented && !f.ShowRented,
		!r.Online && !r.Rented && !f.ShowGone,
		len(f.GPUs) > 0 && !f.Has(o.GPUName),
		f.NumGPUs > 0 && o.NumGPUs != f.NumGPUs,
		f.MinVRAM > 0 && o.GPURam/1024 < f.MinVRAM,
		f.MinDown > 0 && o.InetDown < f.MinDown,
		f.MinUp > 0 && o.InetUp < f.MinUp,
		f.MaxDownTB > 0 && o.DownTB > f.MaxDownTB,
		f.MaxUpTB > 0 && o.UpTB > f.MaxUpTB,
		f.MinCPU > 0 && o.CPUCores < f.MinCPU,
		f.MinRAM > 0 && o.CPURam/1024 < f.MinRAM,
		f.MaxDph > 0 && o.Dph > f.MaxDph,
		f.MinRel > 0 && o.Reliability < f.MinRel,
		f.MinCUDA > 0 && o.CUDA < f.MinCUDA,
		f.MinDisk > 0 && o.Disk < f.MinDisk,
		f.MinDays > 0 && o.Duration/86400 < f.MinDays,
		f.MinPorts > 0 && o.Ports < f.MinPorts:
		return false
	}
	return true
}

func (f Filter) EstDay(o Offer) float64 {
	return o.Dph*24 + f.ExpDownGB*o.DownTB/1024 + f.ExpUpGB*o.UpTB/1024
}

var sortKeys = map[string]func(Row) float64{
	"est":     func(r Row) float64 { return r.EstDay },
	"traffic": func(r Row) float64 { return r.EstDay - r.Dph*24 },
	"dph":     func(r Row) float64 { return r.Dph },
	"disk_h":  func(r Row) float64 { return r.StorageHour },
	"down":    func(r Row) float64 { return r.InetDown },
	"up":      func(r Row) float64 { return r.InetUp },
	"down_tb": func(r Row) float64 { return r.DownTB },
	"up_tb":   func(r Row) float64 { return r.UpTB },
	"cpu":     func(r Row) float64 { return r.CPUCores },
	"ram":     func(r Row) float64 { return r.CPURam },
	"disk":    func(r Row) float64 { return r.Disk },
	"rel":     func(r Row) float64 { return r.Reliability },
	"days":    func(r Row) float64 { return r.Duration },
	"first":   func(r Row) float64 { return float64(r.FirstSeen) },
}

// Apply 过滤、算估算日费并排序。
func (f Filter) Apply(rows []Row) []Row {
	now := time.Now().Unix()
	out := []Row{}
	for _, r := range rows {
		if !f.Match(r) {
			continue
		}
		r.EstDay = f.EstDay(r.Offer)
		r.New = r.Online && now-r.FirstSeen < 3600
		out = append(out, r)
	}
	key, ok := sortKeys[f.Sort]
	if !ok {
		key = sortKeys["est"]
	}
	// 能租的在前，被租走的其次，下架的最后
	rank := func(r Row) int {
		switch {
		case r.Online:
			return 0
		case r.Rented:
			return 1
		}
		return 2
	}
	sort.SliceStable(out, func(i, j int) bool {
		if a, b := rank(out[i]), rank(out[j]); a != b {
			return a < b
		}
		if f.Desc {
			return key(out[i]) > key(out[j])
		}
		return key(out[i]) < key(out[j])
	})
	return out
}
