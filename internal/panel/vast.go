package panel

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

type Client struct {
	Base string // 如 https://console.vast.ai，路径里带 /api/v0 或 /api/v1
	Key  string
	HTTP *http.Client
}

type Offer struct {
	ID           int64   `json:"id"`
	MachineID    int64   `json:"machine_id"`
	HostID       int64   `json:"host_id"`
	GPUName      string  `json:"gpu_name"`
	NumGPUs      int     `json:"num_gpus"`
	GPURam       float64 `json:"gpu_ram"`
	Dph          float64 `json:"dph_total"`
	MinBid       float64 `json:"min_bid"`
	InetDown     float64 `json:"inet_down"`
	InetUp       float64 `json:"inet_up"`
	InetDownCost float64 `json:"inet_down_cost"`
	InetUpCost   float64 `json:"inet_up_cost"`
	DownTB       float64 `json:"internet_down_cost_per_tb"`
	UpTB         float64 `json:"internet_up_cost_per_tb"`
	CPUName      string  `json:"cpu_name"`
	CPUTotal     float64 `json:"cpu_cores"`
	CPUCores     float64 `json:"cpu_cores_effective"`
	CPURam       float64 `json:"cpu_ram"`
	StorageCost  float64 `json:"storage_cost"`       // $/GB/月
	StorageHour  float64 `json:"storage_total_cost"` // 按计价磁盘大小算的 $/h，已含在 Dph 里
	Disk         float64 `json:"disk_space"`
	DiskBW       float64 `json:"disk_bw"`
	Reliability  float64 `json:"reliability"`
	CUDA         float64 `json:"cuda_max_good"`
	Ports        int     `json:"direct_port_count"`
	Duration     float64 `json:"duration"`
	Geo          string  `json:"geolocation"`
	Verification string  `json:"verification"`
	DLPerf       float64 `json:"dlperf"`        // vast 的通用深度学习基准分（估算的参考值，不是实测）
	TFLOPS       float64 `json:"total_flops"`   // vast 标的整机 TFLOPS
	PowerW       float64 `json:"gpu_max_power"` // 单卡功耗上限（W），和卡上 nvidia-smi 的生效上限一致
	Raw          string  `json:"-"`
}

func (c *Client) do(method, path string, body any, out any) error {
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = b
	}
	// vast 对同一接口限速（约每秒一次），429 时等一下再试，最多再试两次。
	var resp *http.Response
	var data []byte
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequest(method, c.Base+path, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+c.Key)
		req.Header.Set("Content-Type", "application/json")
		resp, err = c.HTTP.Do(req)
		if err != nil {
			return err
		}
		data, err = io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusTooManyRequests || attempt == 2 {
			break
		}
		time.Sleep(time.Duration(attempt+1) * 1500 * time.Millisecond)
	}
	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
			Msg   string `json:"msg"`
		}
		if json.Unmarshal(data, &e) == nil && (e.Msg != "" || e.Error != "") {
			return fmt.Errorf("vast %d：%s（%s）", resp.StatusCode, e.Msg, e.Error)
		}
		if len(data) > 300 {
			data = data[:300]
		}
		return fmt.Errorf("vast %d：%s", resp.StatusCode, data)
	}
	return json.Unmarshal(data, out)
}

// Search 拉取按需、已认证、未出租的机器；gpus 为空时不限型号，numGPUs 为 0 时不限卡数。
// disk 决定计价用的磁盘大小（GB），同时要求机器可用磁盘不少于它。
// vast 每次只随机给一部分符合条件的报价（每台机器一条，卡数也随机），
// 所以列表按宽限期内出现过就算在线；指定卡数能让每次给的都是这个卡数的报价。
func (c *Client) Search(gpus []string, numGPUs int, disk float64) ([]Offer, error) {
	return c.search(marketQuery(true, gpus, numGPUs, disk))
}

// SearchRented 拉取眼下被别人租着的机器（vast 的 rentable=false；rented 字段
// 只表示“被你自己租着”，按它过滤不起作用）。条件和 Search 一样。
func (c *Client) SearchRented(gpus []string, numGPUs int, disk float64) ([]Offer, error) {
	return c.search(marketQuery(false, gpus, numGPUs, disk))
}

func marketQuery(rentable bool, gpus []string, numGPUs int, disk float64) map[string]any {
	q := map[string]any{
		"rentable":          map[string]any{"eq": rentable},
		"verified":          map[string]any{"eq": true},
		"type":              "on-demand",
		"limit":             5000,
		"allocated_storage": disk,
		"disk_space":        map[string]any{"gte": disk},
		"order":             [][]string{{"dph_total", "asc"}},
	}
	if rentable {
		q["rented"] = map[string]any{"eq": false}
	}
	if len(gpus) > 0 {
		q["gpu_name"] = map[string]any{"in": gpus}
	}
	if numGPUs > 0 {
		q["num_gpus"] = map[string]any{"eq": numGPUs}
	}
	return q
}

// FindMachine 找这台机器眼下可租的报价（按价格升序），开机前用它拿最新的报价 ID。
func (c *Client) FindMachine(machineID int64, numGPUs int, disk float64) ([]Offer, error) {
	return c.search(map[string]any{
		"machine_id":        map[string]any{"eq": machineID},
		"num_gpus":          map[string]any{"eq": numGPUs},
		"rentable":          map[string]any{"eq": true},
		"rented":            map[string]any{"eq": false},
		"type":              "on-demand",
		"limit":             100,
		"allocated_storage": disk,
		"disk_space":        map[string]any{"gte": disk},
		"order":             [][]string{{"dph_total", "asc"}},
	})
}

func (c *Client) search(q map[string]any) ([]Offer, error) {
	var res struct {
		Offers []json.RawMessage `json:"offers"`
	}
	if err := c.do("POST", "/api/v0/bundles/", q, &res); err != nil {
		return nil, err
	}
	out := make([]Offer, 0, len(res.Offers))
	for _, raw := range res.Offers {
		var o Offer
		if err := json.Unmarshal(raw, &o); err != nil {
			return nil, err
		}
		if o.DownTB == 0 {
			o.DownTB = o.InetDownCost * 1024
		}
		if o.UpTB == 0 {
			o.UpTB = o.InetUpCost * 1024
		}
		o.CPUName = strings.TrimSpace(o.CPUName)
		o.Raw = string(raw)
		out = append(out, o)
	}
	return out, nil
}

// GPUNames 返回当前市场上（单卡、已认证）出现过的所有型号名。
func (c *Client) GPUNames(disk float64) ([]string, error) {
	offers, err := c.Search(nil, 0, disk)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var names []string
	for _, o := range offers {
		if !seen[o.GPUName] {
			seen[o.GPUName] = true
			names = append(names, o.GPUName)
		}
	}
	sort.Strings(names)
	return names, nil
}

type CreateRequest struct {
	TemplateHashID string  `json:"template_hash_id"`
	Disk           float64 `json:"disk"`
	Label          string  `json:"label,omitempty"`
}

// CreateInstance 接受报价开机，返回新实例 ID。
func (c *Client) CreateInstance(offerID int64, req CreateRequest) (int64, error) {
	var res struct {
		Success     bool   `json:"success"`
		NewContract int64  `json:"new_contract"`
		Error       string `json:"error"`
		Msg         string `json:"msg"`
	}
	if err := c.do("PUT", fmt.Sprintf("/api/v0/asks/%d/", offerID), req, &res); err != nil {
		return 0, err
	}
	if !res.Success || res.NewContract == 0 {
		return 0, fmt.Errorf("vast 没有开成：%s（%s）", res.Msg, res.Error)
	}
	return res.NewContract, nil
}

type Template struct {
	HashID      string  `json:"hash_id"`
	Name        string  `json:"name"`
	Image       string  `json:"image"`
	Tag         string  `json:"tag"`
	Desc        string  `json:"desc"`
	DiskGB      float64 `json:"recommended_disk_space"`
	CreatorID   int64   `json:"creator_id"`
	Recommended bool    `json:"recommended"`
	Mine        bool    `json:"mine"`
}

func (c *Client) UserID() (int64, error) {
	var u struct {
		ID int64 `json:"id"`
	}
	if err := c.do("GET", "/api/v0/users/current/", nil, &u); err != nil {
		return 0, err
	}
	return u.ID, nil
}

func (c *Client) templates(filters map[string]any) ([]Template, error) {
	f, err := json.Marshal(filters)
	if err != nil {
		return nil, err
	}
	var res struct {
		Templates []Template `json:"templates"`
	}
	if err := c.do("GET", "/api/v0/template/?select_filters="+url.QueryEscape(string(f)), nil, &res); err != nil {
		return nil, err
	}
	return res.Templates, nil
}

// Templates 返回自己的模板（排在前面）和 vast 推荐模板。
func (c *Client) Templates() ([]Template, error) {
	uid, err := c.UserID()
	if err != nil {
		return nil, err
	}
	mine, err := c.templates(map[string]any{"creator_id": map[string]any{"eq": uid}})
	if err != nil {
		return nil, err
	}
	rec, err := c.templates(map[string]any{"recommended": map[string]any{"eq": true}})
	if err != nil {
		return nil, err
	}
	out := []Template{}
	seen := map[string]bool{}
	for _, t := range mine {
		t.Mine = true
		seen[t.HashID] = true
		out = append(out, t)
	}
	sort.Slice(rec, func(i, j int) bool { return rec[i].Name < rec[j].Name })
	for _, t := range rec {
		if !seen[t.HashID] {
			t.Recommended = true
			out = append(out, t)
		}
	}
	return out, nil
}

// Instances 返回账户下的实例（vast 原样字段）。
func (c *Client) Instances() ([]map[string]any, error) {
	var res struct {
		Instances []map[string]any `json:"instances"`
	}
	if err := c.do("GET", "/api/v1/instances/", nil, &res); err != nil {
		return nil, err
	}
	return res.Instances, nil
}

// simple 调用只回 {success, msg} 的接口，success 为 false 时报错。
func (c *Client) simple(method, path string, body any) error {
	var res struct {
		Success *bool  `json:"success"`
		Error   string `json:"error"`
		Msg     string `json:"msg"`
	}
	if err := c.do(method, path, body, &res); err != nil {
		return err
	}
	if res.Success != nil && !*res.Success {
		return fmt.Errorf("vast：%s（%s）", res.Msg, res.Error)
	}
	return nil
}

func (c *Client) SetState(id int64, state string) error {
	return c.simple("PUT", fmt.Sprintf("/api/v0/instances/%d/", id), map[string]string{"state": state})
}

func (c *Client) SetLabel(id int64, label string) error {
	return c.simple("PUT", fmt.Sprintf("/api/v0/instances/%d/", id), map[string]string{"label": label})
}

func (c *Client) Reboot(id int64) error {
	return c.simple("PUT", fmt.Sprintf("/api/v0/instances/reboot/%d/", id), map[string]any{})
}

func (c *Client) Recycle(id int64) error {
	return c.simple("PUT", fmt.Sprintf("/api/v0/instances/recycle/%d/", id), map[string]any{})
}

func (c *Client) Destroy(id int64) error {
	return c.simple("DELETE", fmt.Sprintf("/api/v0/instances/%d/", id), nil)
}

// RequestLogs 让 vast 把日志传到 S3，返回下载地址；日志要过一会儿才出现在那里。
func (c *Client) RequestLogs(id int64, tail int, filter string, daemon bool) (string, error) {
	body := map[string]string{"tail": fmt.Sprint(tail)}
	if filter != "" {
		body["filter"] = filter
	}
	if daemon {
		body["daemon_logs"] = "true"
	}
	var res struct {
		Success   bool   `json:"success"`
		ResultURL string `json:"result_url"`
		Error     string `json:"error"`
		Msg       string `json:"msg"`
	}
	if err := c.do("PUT", fmt.Sprintf("/api/v0/instances/request_logs/%d/", id), body, &res); err != nil {
		return "", err
	}
	if !res.Success || res.ResultURL == "" {
		return "", fmt.Errorf("vast 没给日志地址：%s（%s）", res.Msg, res.Error)
	}
	return res.ResultURL, nil
}

// FetchLogs 轮询日志地址直到内容出现，最多 wait；只取最后 limit 字节。
func (c *Client) FetchLogs(raw string, wait time.Duration, limit int64) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return "", fmt.Errorf("日志地址不对：%q", raw)
	}
	deadline := time.Now().Add(wait)
	for {
		resp, err := c.HTTP.Get(raw)
		if err == nil {
			data, rerr := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
			resp.Body.Close()
			if rerr == nil && resp.StatusCode == http.StatusOK {
				if int64(len(data)) > limit {
					data = data[int64(len(data))-limit:]
				}
				return string(data), nil
			}
		}
		if time.Now().After(deadline) {
			if err != nil {
				return "", err
			}
			return "", errors.New("日志还没准备好，稍后再试")
		}
		time.Sleep(time.Second)
	}
}

type SSHKey struct {
	ID        int64   `json:"id"`
	PublicKey string  `json:"public_key"`
	CreatedAt float64 `json:"created_at"`
}

func (c *Client) SSHKeys() ([]SSHKey, error) {
	var keys []SSHKey
	if err := c.do("GET", "/api/v0/ssh/", nil, &keys); err != nil {
		return nil, err
	}
	return keys, nil
}

func (c *Client) AttachSSH(id int64, key string) error {
	return c.simple("POST", fmt.Sprintf("/api/v0/instances/%d/ssh/", id), map[string]string{"ssh_key": key})
}

// Charge 是 vast 账单里一个合同（实例）在查询时间段内的费用；Items 按
// gpu / disk / bwd（下行流量）/ bwu（上行流量）拆开。
type Charge struct {
	Start       int64          `json:"start"`
	End         int64          `json:"end"`
	Type        string         `json:"type"`
	Source      string         `json:"source"`
	Description string         `json:"description"`
	Amount      float64        `json:"amount"`
	Metadata    map[string]any `json:"metadata"`
	Items       []Charge       `json:"items"`
}

// Charges 返回 [from, to] 这几天（unix 秒，按 UTC 日计）里各实例的费用，自动翻页。
func (c *Client) Charges(from, to int64) ([]Charge, error) {
	f, err := json.Marshal(map[string]any{"day": map[string]int64{"gte": from, "lte": to}})
	if err != nil {
		return nil, err
	}
	out := []Charge{}
	token := ""
	for page := 0; page < 20; page++ {
		q := url.Values{"select_filters": {string(f)}, "limit": {"500"}}
		if token != "" {
			q.Set("after_token", token)
		}
		var res struct {
			Results   []Charge `json:"results"`
			NextToken *string  `json:"next_token"`
		}
		if err := c.do("GET", "/api/v0/charges?"+q.Encode(), nil, &res); err != nil {
			return nil, err
		}
		out = append(out, res.Results...)
		if res.NextToken == nil || *res.NextToken == "" {
			return out, nil
		}
		token = *res.NextToken
	}
	return out, nil
}

// Credit 是账户余额（美元）。
func (c *Client) Credit() (float64, error) {
	var u struct {
		Credit float64 `json:"credit"`
	}
	if err := c.do("GET", "/api/v0/users/current/", nil, &u); err != nil {
		return 0, err
	}
	return u.Credit, nil
}

func newClient(base, key string) *Client {
	return &Client{Base: strings.TrimRight(base, "/"), Key: key, HTTP: &http.Client{Timeout: 60 * time.Second}}
}
