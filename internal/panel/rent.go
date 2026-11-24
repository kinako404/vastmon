package panel

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const templateTTL = 10 * time.Minute

// rentDefaults 记住上次成功开机用的模板和磁盘，下次弹窗预填。
type rentDefaults struct {
	TemplateHash string  `json:"template_hash_id"`
	Disk         float64 `json:"disk"`
	Label        string  `json:"label"`
	NoTmux       *bool   `json:"no_tmux,omitempty"`       // nil = 默认关掉自动 tmux
	AutoDownload *bool   `json:"auto_download,omitempty"` // nil = 默认开机后启动下载器
	DownloadSet  int     `json:"download_profile,omitempty"`
	AutoScripts  *bool   `json:"auto_scripts,omitempty"` // nil = 默认开机后运行开机脚本
}

var templateHashRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

func (s *Server) templateList() ([]Template, error) {
	s.tplMu.Lock()
	defer s.tplMu.Unlock()
	if s.tplCache != nil && time.Since(s.tplAt) < templateTTL {
		return s.tplCache, nil
	}
	ts, err := s.cl.Templates()
	if err != nil {
		return nil, err
	}
	s.tplCache, s.tplAt = ts, time.Now()
	return ts, nil
}

func (s *Server) defaults(ts []Template) (rentDefaults, error) {
	d := rentDefaults{Disk: s.mon.disk, Label: "vastmon"}
	ok, err := s.st.Get("rent_defaults", &d)
	if err != nil || ok {
		return d, err
	}
	for _, t := range ts {
		if t.Name == "ComfyUI" {
			d.TemplateHash = t.HashID
			d.Disk = math.Max(d.Disk, t.DiskGB)
		}
	}
	return d, nil
}

func (s *Server) templates(w http.ResponseWriter, r *http.Request) {
	ts, err := s.templateList()
	if err != nil {
		writeError(w, http.StatusBadGateway, "VAST", "读取模板失败："+err.Error())
		return
	}
	d, err := s.defaults(ts)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"templates": ts, "defaults": d, "priced_disk": s.mon.disk})
}

type rentRequest struct {
	MachineID    int64   `json:"machine_id"`
	NumGPUs      int     `json:"num_gpus"`
	TemplateHash string  `json:"template_hash_id"`
	TemplateName string  `json:"template_name"`
	Disk         float64 `json:"disk"`
	Label        string  `json:"label"`
	MaxDph       float64 `json:"max_dph"`          // 弹窗里看到的价格；现价更高就不开
	NoTmux       *bool   `json:"no_tmux"`          // 开机后关掉登录自动 tmux；不传 = 关
	AutoDownload bool    `json:"auto_download"`    // 实例跑起来后自动启动下载器
	DownloadSet  int     `json:"download_profile"` // 用哪套下载配置，0 = 默认
	AutoScripts  bool    `json:"auto_scripts"`     // 实例跑起来后运行勾了“开机自动”的脚本
}

func (s *Server) rent(w http.ResponseWriter, r *http.Request) {
	var req rentRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "请求格式不对："+err.Error())
		return
	}
	req.TemplateHash = strings.TrimSpace(strings.ToLower(req.TemplateHash))
	req.Label = strings.TrimSpace(req.Label)
	req.Disk = math.Round(req.Disk)
	switch {
	case req.MachineID <= 0 || req.NumGPUs <= 0:
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "缺少机器")
		return
	case !templateHashRE.MatchString(req.TemplateHash):
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "模板 hash_id 应该是 32 位十六进制")
		return
	case req.Disk < 10 || req.Disk > 16384:
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "磁盘要在 10–16384 GB 之间")
		return
	case len([]rune(req.Label)) > 64:
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "名称最多 64 个字符")
		return
	case req.MaxDph <= 0:
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "缺少确认过的价格")
		return
	}
	if !s.rentMu.TryLock() {
		writeError(w, http.StatusConflict, "BUSY", "上一次开机还没结束")
		return
	}
	defer s.rentMu.Unlock()

	offers, err := s.cl.FindMachine(req.MachineID, req.NumGPUs, req.Disk)
	if err != nil {
		writeError(w, http.StatusBadGateway, "VAST", "查询报价失败："+err.Error())
		return
	}
	if len(offers) == 0 {
		writeError(w, http.StatusConflict, "GONE",
			fmt.Sprintf("机器 %d 现在没有可租的 %d 卡报价（可能刚被租走），或可用磁盘不到 %.0f GB", req.MachineID, req.NumGPUs, req.Disk))
		return
	}
	o := offers[0]
	if o.Dph > req.MaxDph+0.005 {
		writeError(w, http.StatusConflict, "PRICE_CHANGED",
			fmt.Sprintf("价格变了：现在 $%.3f/h，确认时是 $%.3f/h。没有开机，请重新确认。", o.Dph, req.MaxDph))
		return
	}
	name := req.TemplateName
	if ts, err := s.templateList(); err == nil {
		for _, t := range ts {
			if t.HashID == req.TemplateHash {
				name = t.Name
			}
		}
	}
	rec := Rental{
		TS: time.Now().Unix(), MachineID: req.MachineID, NumGPUs: req.NumGPUs, OfferID: o.ID, GPUName: o.GPUName,
		TemplateHash: req.TemplateHash, TemplateName: name, Disk: req.Disk, Dph: o.Dph, Label: req.Label,
	}
	id, err := s.cl.CreateInstance(o.ID, CreateRequest{TemplateHashID: req.TemplateHash, Disk: req.Disk, Label: req.Label})
	if err != nil {
		rec.Err = err.Error()
	}
	rec.InstanceID = id
	if e := s.st.AddRental(rec); e != nil {
		log.Printf("记录开机失败: %v", e)
	}
	if err != nil {
		log.Printf("开机失败 机器 %d 报价 %d: %v", req.MachineID, o.ID, err)
		writeError(w, http.StatusBadGateway, "VAST", "开机失败："+err.Error())
		return
	}
	log.Printf("已开机 实例 %d 机器 %d 报价 %d $%.3f/h 模板 %s 磁盘 %.0fGB", id, req.MachineID, o.ID, o.Dph, name, req.Disk)
	noTmux := req.NoTmux == nil || *req.NoTmux
	if noTmux {
		if err := s.st.AddSetup(id, true); err != nil {
			log.Printf("记录开机设置失败: %v", err)
		}
	}
	autoMsg := ""
	if req.AutoDownload {
		cfg, err := s.dlConfig()
		if err == nil {
			_, err = s.createJob(id, req.DownloadSet, nil, cfg.SyncInput, true)
		}
		if err != nil {
			autoMsg = "没能安排自动下载：" + err.Error()
			log.Printf("实例 %d %s", id, autoMsg)
		}
	}
	if req.AutoScripts {
		if _, err := s.createScriptJob(id, nil, true); err != nil {
			msg := "没能安排开机脚本：" + err.Error()
			log.Printf("实例 %d %s", id, msg)
			if autoMsg != "" {
				autoMsg += "；"
			}
			autoMsg += msg
		}
	}
	if err := s.st.Set("rent_defaults", rentDefaults{TemplateHash: req.TemplateHash, Disk: req.Disk, Label: req.Label, NoTmux: &noTmux, AutoDownload: &req.AutoDownload, DownloadSet: req.DownloadSet, AutoScripts: &req.AutoScripts}); err != nil {
		log.Printf("保存开机默认值失败: %v", err)
	}
	writeJSON(w, map[string]any{"instance_id": id, "offer_id": o.ID, "dph": o.Dph, "auto_download_error": autoMsg})
}

func (s *Server) instances(w http.ResponseWriter, r *http.Request) {
	rentals, err := s.st.Rentals(50)
	if err != nil {
		s.fail(w, err)
		return
	}
	actions, err := s.st.Actions(100)
	if err != nil {
		s.fail(w, err)
		return
	}
	setups, err := s.st.Setups()
	if err != nil {
		s.fail(w, err)
		return
	}
	out := map[string]any{"rentals": rentals, "actions": actions, "setups": setups, "instances": []map[string]any{}, "now": time.Now().Unix()}
	ins, err := s.cl.Instances()
	if err != nil {
		out["error"] = "读取实例失败：" + err.Error()
	} else if ins != nil {
		out["instances"] = ins
	}
	writeJSON(w, out)
}
