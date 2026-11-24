package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 启动下载器的配置：可以有多套（比如“MiniMax 全套”“SDXL”），每套是一张下载列表
// （链接 + 目标目录）；HF / Civitai 的 token、工作目录、线程数各套共用。
// 配置存在 settings 表的 "downloads" 里；token 只在服务器上用，接口不回传。

const (
	defaultComfyRoot   = "/workspace/ComfyUI"
	defaultConnections = 16
	maxDLItems         = 200
)

type dlItem struct {
	ID      int    `json:"id"`
	URL     string `json:"url"`
	Dir     string `json:"dir"`
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	Enabled bool   `json:"enabled"`
	Note    string `json:"note"`
}

type dlProfile struct {
	ID    int      `json:"id"`
	Name  string   `json:"name"`
	Items []dlItem `json:"items"`
}

type dlConfig struct {
	Root         string      `json:"root"`
	Connections  int         `json:"connections"`
	SyncInput    bool        `json:"sync_input"`
	HFToken      string      `json:"hf_token"`
	CivitaiToken string      `json:"civitai_token"`
	Profiles     []dlProfile `json:"profiles"`
	Default      int         `json:"default"`
	Items        []dlItem    `json:"items,omitempty"` // 旧版只有一张列表，读出来后并进“默认”
}

// profile 按 ID 找一套配置，0 或找不到用默认的。
func (c dlConfig) profile(id int) dlProfile {
	for _, p := range c.Profiles {
		if p.ID == id {
			return p
		}
	}
	for _, p := range c.Profiles {
		if p.ID == c.Default {
			return p
		}
	}
	return c.Profiles[0]
}

func (c dlConfig) allItems() []dlItem {
	var all []dlItem
	for _, p := range c.Profiles {
		all = append(all, p.Items...)
	}
	return all
}

func (s *Server) dlConfig() (dlConfig, error) {
	c := dlConfig{Root: defaultComfyRoot, Connections: defaultConnections, SyncInput: true}
	if _, err := s.st.Get("downloads", &c); err != nil {
		return c, err
	}
	if len(c.Profiles) == 0 {
		c.Profiles = []dlProfile{{ID: 1, Name: "默认", Items: c.Items}}
		c.Default = 1
	}
	c.Items = nil
	for i := range c.Profiles {
		if c.Profiles[i].Items == nil {
			c.Profiles[i].Items = []dlItem{}
		}
	}
	c.Default = c.profile(c.Default).ID
	if c.Root == "" {
		c.Root = defaultComfyRoot
	}
	if c.Connections < 1 || c.Connections > 16 {
		c.Connections = defaultConnections
	}
	return c, nil
}

func tokenHint(t string) string {
	if t == "" {
		return ""
	}
	if len(t) <= 8 {
		return "已设置"
	}
	return "…" + t[len(t)-4:]
}

// publicConfig 是发给浏览器的配置：token 只给是否设置和末 4 位。
func (c dlConfig) public() map[string]any {
	return map[string]any{
		"root": c.Root, "connections": c.Connections, "sync_input": c.SyncInput, "profiles": c.Profiles, "default": c.Default,
		"hf_token": tokenHint(c.HFToken), "civitai_token": tokenHint(c.CivitaiToken),
		"input_archive": inputArchiveInfo(),
		// 有 comfy_kit 的 sources.json 或 d.sh 时才显示“从 comfy_kit 导入”
		"import_available": projectPath("backups/sources.json") != "" || projectPath("d.sh") != "",
	}
}

func (s *Server) dlConfigGet(w http.ResponseWriter, r *http.Request) {
	c, err := s.dlConfig()
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, c.public())
}

var badDirRE = regexp.MustCompile(`(^|/)\.\.(/|$)`)

// cleanDir 校验目标目录：相对工作目录的路径或绝对路径，不能有 ..。
func cleanDir(d string) (string, error) {
	d = strings.TrimSpace(d)
	if d == "" {
		return "", errors.New("目标目录不能为空")
	}
	if badDirRE.MatchString(d) || strings.ContainsAny(d, "\x00\n\r") {
		return "", fmt.Errorf("目标目录不能含 ..：%s", d)
	}
	c := path.Clean(d)
	if c == "." || c == "/" {
		return "", fmt.Errorf("目标目录不对：%s", d)
	}
	return c, nil
}

var badNameRE = regexp.MustCompile(`[/\\\x00-\x1f]`)

func cleanName(n string) (string, error) {
	n = strings.TrimSpace(n)
	if n == "" {
		return "", nil
	}
	if badNameRE.MatchString(n) || n == "." || n == ".." || strings.HasPrefix(n, ".") {
		return "", fmt.Errorf("文件名不对：%s", n)
	}
	return n, nil
}

// normalizeURL 把网页链接换成直接下载的链接，并把 Civitai 链接里的 token 拿出来。
func normalizeURL(raw string) (string, string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", "", fmt.Errorf("链接不对：%s", raw)
	}
	host := strings.ToLower(u.Hostname())
	token := ""
	switch {
	case host == "huggingface.co" || strings.HasSuffix(host, ".huggingface.co") || host == "hf.co":
		u.Path = strings.Replace(u.Path, "/blob/", "/resolve/", 1)
	case isCivitai(host):
		q := u.Query()
		token = q.Get("token")
		q.Del("token")
		if m := regexp.MustCompile(`^/models/(\d+)`).FindStringSubmatch(u.Path); m != nil {
			if v := q.Get("modelVersionId"); v != "" {
				u.Path = "/api/download/models/" + v
				q.Del("modelVersionId")
			}
		}
		u.RawQuery = q.Encode()
	}
	return u.String(), token, nil
}

func isCivitai(host string) bool {
	host = strings.ToLower(host)
	return host == "civitai.com" || strings.HasSuffix(host, ".civitai.com")
}

func isHF(host string) bool {
	host = strings.ToLower(host)
	return host == "huggingface.co" || strings.HasSuffix(host, ".huggingface.co") || host == "hf.co"
}

func (s *Server) dlConfigPut(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Root         *string     `json:"root"`
		Connections  *int        `json:"connections"`
		SyncInput    *bool       `json:"sync_input"`
		HFToken      string      `json:"hf_token"`
		CivitaiToken string      `json:"civitai_token"`
		ClearHF      bool        `json:"clear_hf_token"`
		ClearCivitai bool        `json:"clear_civitai_token"`
		Profiles     []dlProfile `json:"profiles"`
		Default      *int        `json:"default"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "请求格式不对："+err.Error())
		return
	}
	c, err := s.dlConfig()
	if err != nil {
		s.fail(w, err)
		return
	}
	if body.Root != nil {
		root := path.Clean(strings.TrimSpace(*body.Root))
		if !strings.HasPrefix(root, "/") || root == "/" || badDirRE.MatchString(root) {
			writeError(w, http.StatusBadRequest, "BAD_REQUEST", "工作目录要写绝对路径，比如 /workspace/ComfyUI")
			return
		}
		c.Root = root
	}
	if body.Connections != nil {
		if *body.Connections < 1 || *body.Connections > 16 {
			writeError(w, http.StatusBadRequest, "BAD_REQUEST", "线程数要在 1–16 之间（aria2 每台服务器最多 16 条连接）")
			return
		}
		c.Connections = *body.Connections
	}
	if body.SyncInput != nil {
		c.SyncInput = *body.SyncInput
	}
	if t := strings.TrimSpace(body.HFToken); t != "" {
		c.HFToken = t
	}
	if t := strings.TrimSpace(body.CivitaiToken); t != "" {
		c.CivitaiToken = t
	}
	if body.ClearHF {
		c.HFToken = ""
	}
	if body.ClearCivitai {
		c.CivitaiToken = ""
	}
	if body.Profiles != nil {
		if len(body.Profiles) == 0 {
			writeError(w, http.StatusBadRequest, "BAD_REQUEST", "至少要留一套配置")
			return
		}
		next := id0(c.allItems())
		nextP := 0
		for _, p := range c.Profiles {
			nextP = max(nextP, p.ID)
		}
		seenName := map[string]bool{}
		seenItem := map[int]bool{}
		total := 0
		profiles := make([]dlProfile, 0, len(body.Profiles))
		for _, p := range body.Profiles {
			p.Name = truncRunes(strings.TrimSpace(p.Name), 40)
			if p.Name == "" {
				writeError(w, http.StatusBadRequest, "BAD_REQUEST", "配置名不能为空")
				return
			}
			if seenName[p.Name] {
				writeError(w, http.StatusBadRequest, "BAD_REQUEST", "配置名重复："+p.Name)
				return
			}
			seenName[p.Name] = true
			if p.ID <= 0 {
				nextP++
				p.ID = nextP
			}
			total += len(p.Items)
			items := make([]dlItem, 0, len(p.Items))
			for _, it := range p.Items {
				u, tok, err := normalizeURL(it.URL)
				if err == nil {
					it.Dir, err = cleanDir(it.Dir)
				}
				if err == nil {
					it.Name, err = cleanName(it.Name)
				}
				if err != nil {
					writeError(w, http.StatusBadRequest, "BAD_REQUEST", p.Name+"："+err.Error())
					return
				}
				if tok != "" && c.CivitaiToken == "" {
					c.CivitaiToken = tok
				}
				it.URL = u
				it.Note = truncRunes(strings.TrimSpace(it.Note), 200)
				if it.Size < 0 {
					it.Size = 0
				}
				if it.ID <= 0 || seenItem[it.ID] {
					next++
					it.ID = next
				}
				seenItem[it.ID] = true
				items = append(items, it)
			}
			p.Items = items
			profiles = append(profiles, p)
		}
		if total > maxDLItems {
			writeError(w, http.StatusBadRequest, "BAD_REQUEST", fmt.Sprintf("所有配置加起来最多 %d 个文件", maxDLItems))
			return
		}
		c.Profiles = profiles
	}
	if body.Default != nil {
		c.Default = *body.Default
	}
	c.Default = c.profile(c.Default).ID
	if err := s.st.Set("downloads", c); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, c.public())
}

func id0(items []dlItem) int {
	m := 0
	for _, it := range items {
		m = max(m, it.ID)
	}
	return m
}

func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// ---------------------------------------------------------------- 解析链接

type dlResolved struct {
	URL      string `json:"-"`    // 交给 aria2 的地址
	Header   string `json:"-"`    // 交给 aria2 的认证头（没跳转到别的域名时才需要）
	Name     string `json:"name"` // 文件名
	Size     int64  `json:"size"` // -1 = 不知道
	Host     string `json:"host"` // 最终下载的域名
	Redirect bool   `json:"redirect"`
}

var dlHTTP = &http.Client{Timeout: 40 * time.Second}

// resolveDL 用 token 请求一次（只要第 1 个字节），跟随跳转拿到真正的下载地址、
// 文件名和大小。Go 跳转到别的域名时会自动去掉 Authorization 头，Civitai 的
// token 在查询参数里，跳转后的签名地址不带它——所以 token 一般不会出现在实例上。
func resolveDL(ctx context.Context, raw string, c dlConfig) (dlResolved, error) {
	norm, tok, err := normalizeURL(raw)
	if err != nil {
		return dlResolved{}, err
	}
	u, _ := url.Parse(norm)
	host := u.Hostname()
	if isCivitai(host) {
		if m := regexp.MustCompile(`^/models/(\d+)/?$`).FindStringSubmatch(u.Path); m != nil {
			v, err := civitaiLatestVersion(ctx, m[1], c.CivitaiToken)
			if err != nil {
				return dlResolved{}, err
			}
			u, _ = url.Parse("https://civitai.com/api/download/models/" + v)
		}
		if tok == "" {
			tok = c.CivitaiToken
		}
		if tok != "" {
			q := u.Query()
			q.Set("token", tok)
			u.RawQuery = q.Encode()
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return dlResolved{}, err
	}
	authHeader := ""
	if isHF(host) && c.HFToken != "" {
		authHeader = "Authorization: Bearer " + c.HFToken
		req.Header.Set("Authorization", "Bearer "+c.HFToken)
	}
	req.Header.Set("Range", "bytes=0-0")
	req.Header.Set("User-Agent", "vastmon/1 (aria2 launcher)")
	resp, err := dlHTTP.Do(req)
	if err != nil {
		return dlResolved{}, fmt.Errorf("请求链接失败：%v", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		what := "token"
		if isCivitai(host) {
			what = "Civitai token"
		} else if isHF(host) {
			what = "Hugging Face token（受限模型要先在网页上同意协议）"
		}
		return dlResolved{}, fmt.Errorf("没有权限（HTTP %d）：检查%s", resp.StatusCode, what)
	case resp.StatusCode == 404:
		return dlResolved{}, errors.New("文件不存在（HTTP 404）")
	case resp.StatusCode != 200 && resp.StatusCode != 206:
		return dlResolved{}, fmt.Errorf("链接返回 HTTP %d", resp.StatusCode)
	}
	final := resp.Request.URL
	res := dlResolved{URL: final.String(), Size: -1, Host: final.Hostname(), Redirect: final.Host != u.Host}
	if resp.StatusCode == 206 {
		if cr := resp.Header.Get("Content-Range"); cr != "" {
			if i := strings.LastIndexByte(cr, '/'); i >= 0 {
				if n, err := strconv.ParseInt(cr[i+1:], 10, 64); err == nil {
					res.Size = n
				}
			}
		}
	} else if resp.ContentLength >= 0 {
		res.Size = resp.ContentLength
	}
	res.Name = nameFromDisposition(resp.Header.Get("Content-Disposition"))
	if res.Name == "" {
		res.Name = nameFromDisposition(final.Query().Get("response-content-disposition"))
	}
	if res.Name == "" {
		res.Name = path.Base(u.Path)
		if isCivitai(host) {
			res.Name = ""
		}
	}
	if n, err := cleanName(res.Name); err != nil || n == "" {
		res.Name = ""
	}
	if !res.Redirect {
		// 没跳转：aria2 直接下原地址，要带上凭据
		res.URL = u.String()
		res.Header = authHeader
	}
	return res, nil
}

func nameFromDisposition(v string) string {
	if v == "" {
		return ""
	}
	_, params, err := mime.ParseMediaType(v)
	if err != nil {
		if i := strings.Index(v, "filename="); i >= 0 {
			return path.Base(strings.Trim(strings.SplitN(v[i+9:], ";", 2)[0], `"' `))
		}
		return ""
	}
	return path.Base(params["filename"])
}

func civitaiLatestVersion(ctx context.Context, modelID, token string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://civitai.com/api/v1/models/"+modelID, nil)
	if err != nil {
		return "", err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := dlHTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("查 Civitai 模型失败：%v", err)
	}
	defer resp.Body.Close()
	var m struct {
		ModelVersions []struct {
			ID int64 `json:"id"`
		} `json:"modelVersions"`
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("查 Civitai 模型 %s 返回 HTTP %d", modelID, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil || len(m.ModelVersions) == 0 {
		return "", fmt.Errorf("Civitai 模型 %s 没有可下载的版本", modelID)
	}
	return strconv.FormatInt(m.ModelVersions[0].ID, 10), nil
}

func (s *Server) dlResolve(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL string `json:"url"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	c, err := s.dlConfig()
	if err != nil {
		s.fail(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	res, err := resolveDL(ctx, body.URL, c)
	if err != nil {
		writeError(w, http.StatusBadGateway, "RESOLVE", err.Error())
		return
	}
	norm, _, _ := normalizeURL(body.URL)
	writeJSON(w, map[string]any{"name": res.Name, "size": res.Size, "host": res.Host, "redirect": res.Redirect, "url": norm})
}

// ---------------------------------------------------------------- 从 comfy_kit 导入

// projectPath 找项目目录（见 projectDirs）里的文件，找不到返回空。
func projectPath(rel string) string {
	for _, dir := range projectDirs() {
		p, err := filepath.Abs(filepath.Join(dir, rel))
		if err != nil {
			continue
		}
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

var shVarRE = regexp.MustCompile(`(?m)^\s*(API_KEY|HF_TOKEN)="([^"$\x60]*)"`)

// dlImport 把 comfy_kit 的 backups/sources.json 并进下载列表（按文件名去重），
// 没设置 token 时从 d.sh 读 Civitai（API_KEY）和 HF（HF_TOKEN）token。
func (s *Server) dlImport(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ProfileID int `json:"profile_id"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	c, err := s.dlConfig()
	if err != nil {
		s.fail(w, err)
		return
	}
	pi := 0
	for i, p := range c.Profiles {
		if p.ID == c.profile(body.ProfileID).ID {
			pi = i
		}
	}
	prof := &c.Profiles[pi]
	var notes []string
	if p := projectPath("backups/sources.json"); p != "" {
		var src struct {
			Files map[string]struct {
				Folder string   `json:"folder"`
				Size   int64    `json:"size"`
				URLs   []string `json:"urls"`
			} `json:"files"`
		}
		b, err := os.ReadFile(p)
		if err == nil {
			err = json.Unmarshal(b, &src)
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, "BAD_SOURCES", "读不了 sources.json："+err.Error())
			return
		}
		have := map[string]bool{}
		for _, it := range prof.Items {
			have[it.Name] = true
		}
		names := make([]string, 0, len(src.Files))
		for n := range src.Files {
			names = append(names, n)
		}
		sort.Strings(names)
		next, added := id0(c.allItems()), 0
		for _, n := range names {
			f := src.Files[n]
			if have[n] || len(f.URLs) == 0 || f.Folder == "" {
				continue
			}
			u, tok, err := normalizeURL(f.URLs[0])
			if err != nil {
				continue
			}
			if tok != "" && c.CivitaiToken == "" {
				c.CivitaiToken = tok
			}
			next++
			added++
			prof.Items = append(prof.Items, dlItem{ID: next, URL: u, Dir: "models/" + f.Folder, Name: n, Size: f.Size,
				Enabled: !strings.HasPrefix(n, "Minimax-h3_Singularity"), Note: "从 sources.json 导入"})
		}
		notes = append(notes, fmt.Sprintf("sources.json 往“%s”里新增 %d 个文件", prof.Name, added))
	} else {
		notes = append(notes, "没找到 backups/sources.json")
	}
	if p := projectPath("d.sh"); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			for _, m := range shVarRE.FindAllStringSubmatch(string(b), -1) {
				switch {
				case m[1] == "API_KEY" && c.CivitaiToken == "" && m[2] != "":
					c.CivitaiToken = m[2]
					notes = append(notes, "从 d.sh 读到 Civitai token")
				case m[1] == "HF_TOKEN" && c.HFToken == "" && m[2] != "":
					c.HFToken = m[2]
					notes = append(notes, "从 d.sh 读到 HF token")
				}
			}
		}
	}
	if err := s.st.Set("downloads", c); err != nil {
		s.fail(w, err)
		return
	}
	out := c.public()
	out["notes"] = notes
	writeJSON(w, out)
}
