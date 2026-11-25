package panel

import (
	"embed"
	"encoding/json"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"sync"
	"time"
)

// web 是面板前端（原生 JS 模块，风格同 ConLoom 面板），打包进二进制。
//
//go:embed web
var webFS embed.FS

type Server struct {
	st *Store
	// dataDir 放数据库旁边的文件（粘贴进来的私钥）。
	dataDir string
	mon     *Monitor
	cl      *Client
	auth    *Auth
	rentMu  sync.Mutex
	// actionMu 让实例操作一次只做一个，防止连点。
	actionMu sync.Mutex

	bill billingCache
	pool sshPool
	dl   dlManager
	sc   scriptManager
	// inputMu 让 input 备份一次只合并一个。
	inputMu sync.Mutex

	notifyState notifyState
	vapidMu     sync.Mutex

	tplMu    sync.Mutex
	tplCache []Template
	tplAt    time.Time
}

func newServer(st *Store, mon *Monitor, cl *Client, auth *Auth, dataDir string) *Server {
	return &Server{st: st, mon: mon, cl: cl, auth: auth, dataDir: dataDir}
}

func init() {
	// Go 自带的类型表里没有 .webmanifest
	mime.AddExtensionType(".webmanifest", "application/manifest+json")
}

func (s *Server) routes() http.Handler {
	root, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err)
	}
	static := http.FileServerFS(root)
	mux := http.NewServeMux()
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		static.ServeHTTP(w, r)
	}))
	mux.HandleFunc("GET /api/state", s.state)
	mux.HandleFunc("PUT /api/filter", s.putFilter)
	mux.HandleFunc("POST /api/filter/reset", s.resetFilter)
	mux.HandleFunc("PUT /api/filter/presets", s.auth.require(s.savePreset))
	mux.HandleFunc("DELETE /api/filter/presets/{name}", s.auth.require(s.deletePreset))
	mux.HandleFunc("POST /api/poll", s.pollNow)
	mux.HandleFunc("GET /api/machines/{mid}/{n}", s.machine)
	mux.HandleFunc("GET /api/polls", s.polls)
	mux.HandleFunc("GET /api/offers", s.offers)
	mux.HandleFunc("GET /api/session", s.auth.session)
	mux.HandleFunc("POST /api/login", s.auth.login)
	mux.HandleFunc("POST /api/logout", s.auth.logout)
	mux.HandleFunc("GET /api/templates", s.auth.require(s.templates))
	mux.HandleFunc("POST /api/rent", s.auth.require(s.rent))
	mux.HandleFunc("GET /api/instances", s.auth.require(s.instances))
	mux.HandleFunc("POST /api/instances/{id}/action", s.auth.require(s.instanceAction))
	mux.HandleFunc("PUT /api/instances/{id}/label", s.auth.require(s.instanceLabel))
	mux.HandleFunc("POST /api/instances/{id}/logs", s.auth.require(s.instanceLogs))
	mux.HandleFunc("POST /api/instances/{id}/ssh", s.auth.require(s.instanceSSH))
	mux.HandleFunc("POST /api/instances/{id}/notmux", s.auth.require(s.noTmuxNow))
	mux.HandleFunc("GET /api/ssh-keys", s.auth.require(s.sshKeys))
	mux.HandleFunc("GET /api/billing", s.auth.require(s.billing))
	mux.HandleFunc("GET /api/favorites", s.favorites)
	mux.HandleFunc("POST /api/favorites", s.auth.require(s.addFavorite))
	mux.HandleFunc("POST /api/favorites/check", s.auth.require(s.checkFavorites))
	mux.HandleFunc("PUT /api/favorites/{mid}/note", s.auth.require(s.favoriteNote))
	mux.HandleFunc("DELETE /api/favorites/{mid}", s.auth.require(s.removeFavorite))
	mux.HandleFunc("GET /api/blacklist", s.blacklist)
	mux.HandleFunc("POST /api/instances/{id}/bench", s.auth.require(s.runBench))
	mux.HandleFunc("GET /api/benchmarks", s.benchmarks)
	mux.HandleFunc("GET /api/baselines", s.baselines)
	mux.HandleFunc("PUT /api/baselines", s.auth.require(s.saveBaseline))
	mux.HandleFunc("DELETE /api/baselines/{bid}", s.auth.require(s.deleteBaseline))
	mux.HandleFunc("POST /api/baselines/reset", s.auth.require(s.resetBaselines))
	mux.HandleFunc("GET /api/term/{id}/ws", s.auth.require(s.terminal))
	mux.HandleFunc("GET /api/term/{id}/check", s.auth.require(s.termCheck))
	mux.HandleFunc("GET /api/files/{id}/list", s.auth.require(s.listFiles))
	mux.HandleFunc("GET /api/files/{id}/raw", s.auth.require(s.rawFile))
	mux.HandleFunc("POST /api/files/{id}/upload", s.auth.require(s.uploadFiles))
	mux.HandleFunc("DELETE /api/files/{id}/file", s.auth.require(s.deleteFile))
	mux.HandleFunc("GET /api/files/{id}/text", s.auth.require(s.readText))
	mux.HandleFunc("PUT /api/files/{id}/text", s.auth.require(s.writeText))
	mux.HandleFunc("POST /api/settings/password", s.auth.require(s.auth.changePassword))
	mux.HandleFunc("GET /api/settings/notify", s.auth.require(s.notifySettings))
	mux.HandleFunc("PUT /api/settings/notify", s.auth.require(s.putNotifySettings))
	mux.HandleFunc("POST /api/push/subscribe", s.auth.require(s.subscribePush))
	mux.HandleFunc("POST /api/push/unsubscribe", s.auth.require(s.unsubscribePush))
	mux.HandleFunc("DELETE /api/push/subs/{sid}", s.auth.require(s.deletePushSub))
	mux.HandleFunc("POST /api/push/test", s.auth.require(s.testPush))
	mux.HandleFunc("GET /api/scripts", s.auth.require(s.scriptsGet))
	mux.HandleFunc("PUT /api/scripts", s.auth.require(s.scriptsPut))
	mux.HandleFunc("POST /api/scripts/files", s.auth.require(s.uploadScriptFile))
	mux.HandleFunc("DELETE /api/scripts/files/{name}", s.auth.require(s.deleteScriptFile))
	mux.HandleFunc("POST /api/scripts/files/{name}/token", s.auth.require(s.renewFileToken))
	mux.HandleFunc("GET /api/scripts/jobs", s.auth.require(s.scriptJobs))
	mux.HandleFunc("POST /api/scripts/jobs/{jid}/cancel", s.auth.require(s.cancelScripts))
	mux.HandleFunc("POST /api/instances/{id}/scripts", s.auth.require(s.startScripts))
	mux.HandleFunc("GET /f/{token}/{name}", s.publicFile)
	mux.HandleFunc("GET /api/settings/ssh", s.auth.require(s.sshSettings))
	mux.HandleFunc("PUT /api/settings/ssh", s.auth.require(s.putSSHSettings))
	mux.HandleFunc("POST /api/settings/ssh/test", s.auth.require(s.testSSH))
	mux.HandleFunc("GET /api/downloads/config", s.auth.require(s.dlConfigGet))
	mux.HandleFunc("PUT /api/downloads/config", s.auth.require(s.dlConfigPut))
	mux.HandleFunc("POST /api/downloads/resolve", s.auth.require(s.dlResolve))
	mux.HandleFunc("POST /api/downloads/import", s.auth.require(s.dlImport))
	mux.HandleFunc("GET /api/downloads/jobs", s.auth.require(s.downloadJobs))
	mux.HandleFunc("POST /api/downloads/jobs/{jid}/cancel", s.auth.require(s.cancelDownload))
	mux.HandleFunc("POST /api/instances/{id}/download", s.auth.require(s.startDownload))
	mux.HandleFunc("POST /api/instances/{id}/backup-input", s.auth.require(s.backupInputNow))
	mux.HandleFunc("POST /api/blacklist", s.auth.require(s.addBlacklist))
	mux.HandleFunc("PUT /api/blacklist/{mid}/note", s.auth.require(s.blacklistNote))
	mux.HandleFunc("DELETE /api/blacklist/{mid}", s.auth.require(s.removeBlacklist))
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "接口不存在")
	})
	return csrf(s.auth.guardAPI(mux))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("写响应出错: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	log.Printf("请求出错: %v", err)
	writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
}

type state struct {
	Filter      Filter         `json:"filter"`
	Rows        []Row          `json:"rows"`
	Online      int            `json:"online"`
	Poll        Poll           `json:"poll"`
	GPUNames    []string       `json:"gpu_names"`
	Favorites   []FavoriteMark `json:"favorites"`
	Blacklist   []FavoriteMark `json:"blacklist"`
	Presets     []filterPreset `json:"presets"`
	IntervalSec int            `json:"interval_sec"`
	Disk        float64        `json:"disk"`
}

func (s *Server) buildState() (state, error) {
	f := loadFilter(s.st)
	rows, err := s.st.Rows()
	if err != nil {
		return state{}, err
	}
	online := 0
	for _, r := range rows {
		if r.Online && (len(f.GPUs) == 0 || f.Has(r.GPUName)) {
			online++
		}
	}
	poll, err := s.st.LastPoll()
	if err != nil {
		return state{}, err
	}
	var c catalog
	if _, err := s.st.Get("catalog", &c); err != nil {
		return state{}, err
	}
	names := slices.Clone(c.Names)
	for _, g := range f.GPUs {
		if !slices.Contains(names, g) {
			names = append(names, g)
		}
	}
	sort.Strings(names)
	favs, err := s.st.FavoriteMarks()
	if err != nil {
		return state{}, err
	}
	black, err := s.st.BlacklistMarks()
	if err != nil {
		return state{}, err
	}
	presets, err := s.filterPresets()
	if err != nil {
		return state{}, err
	}
	return state{Favorites: favs, Blacklist: black, Presets: presets,
		Filter: f, Rows: f.Apply(rows), Online: online, Poll: poll, GPUNames: names,
		IntervalSec: int(s.mon.interval.Seconds()), Disk: s.mon.disk,
	}, nil
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	s.writeState(w)
}

func (s *Server) writeState(w http.ResponseWriter) {
	st, err := s.buildState()
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, st)
}

// saveFilter 保存条件；型号列表变化时立即触发一次抓取。
func (s *Server) saveFilter(w http.ResponseWriter, f Filter) {
	before := loadFilter(s.st)
	if f.GPUs == nil {
		f.GPUs = []string{}
	}
	if err := s.st.Set("filter", f); err != nil {
		s.fail(w, err)
		return
	}
	if !slices.Equal(before.GPUs, f.GPUs) {
		s.mon.Poke()
	}
	s.writeState(w)
}

func (s *Server) putFilter(w http.ResponseWriter, r *http.Request) {
	var f Filter
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&f); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "过滤条件格式不对："+err.Error())
		return
	}
	if _, ok := sortKeys[f.Sort]; !ok {
		f.Sort = "est"
	}
	s.saveFilter(w, f)
}

func (s *Server) resetFilter(w http.ResponseWriter, r *http.Request) {
	s.saveFilter(w, defaultFilter())
}

// pollNow 触发一次抓取并等它结束（最多 30 秒），返回最新状态。
func (s *Server) pollNow(w http.ResponseWriter, r *http.Request) {
	before, err := s.st.LastPoll()
	if err != nil {
		s.fail(w, err)
		return
	}
	s.mon.Poke()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(300 * time.Millisecond):
		}
		p, err := s.st.LastPoll()
		if err != nil {
			s.fail(w, err)
			return
		}
		if p.ID != before.ID {
			break
		}
	}
	s.writeState(w)
}

func (s *Server) machine(w http.ResponseWriter, r *http.Request) {
	mid, err1 := strconv.ParseInt(r.PathValue("mid"), 10, 64)
	n, err2 := strconv.Atoi(r.PathValue("n"))
	if err1 != nil || err2 != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "机器不存在")
		return
	}
	row, err := s.st.Row(mid, n)
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "机器不存在")
		return
	}
	hist, err := s.st.History(mid, n)
	if err != nil {
		s.fail(w, err)
		return
	}
	f := loadFilter(s.st)
	row.EstDay = f.EstDay(row.Offer)
	var raw any
	if err := json.Unmarshal([]byte(row.Raw), &raw); err != nil {
		raw = nil
	}
	writeJSON(w, map[string]any{"machine": row, "history": hist, "raw": raw, "filter": f, "disk": s.mon.disk})
}

func (s *Server) polls(w http.ResponseWriter, r *http.Request) {
	ps, err := s.st.Polls(200)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"polls": ps, "interval_sec": int(s.mon.interval.Seconds())})
}

func (s *Server) offers(w http.ResponseWriter, r *http.Request) {
	st, err := s.buildState()
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, st.Rows)
}
