package panel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// 快捷命令（终端底栏“命令”，仿 FinalShell）：分类 + 命令，点一下把命令发到当前终端。
// 存在 settings 表 quick_cmds，所有浏览器共用；没存过时给一组 vast / ComfyUI 常用命令。

const (
	maxQuickGroups   = 50
	maxQuickCmds     = 500
	maxQuickCmdBytes = 64 << 10
)

type quickCmd struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Cmd   string `json:"cmd"`
	Enter bool   `json:"enter"` // 发完再按回车（直接执行）；否则只填进命令行
}

type quickGroup struct {
	ID   int        `json:"id"`
	Name string     `json:"name"`
	Cmds []quickCmd `json:"cmds"`
}

func defaultQuickCmds() []quickGroup {
	return []quickGroup{{ID: 1, Name: "常用", Cmds: []quickCmd{
		{ID: 1, Name: "ComfyUI 日志", Cmd: "tail -n 200 -f /var/log/portal/comfyui.log", Enter: true},
		{ID: 2, Name: "服务状态", Cmd: "supervisorctl status", Enter: true},
		{ID: 3, Name: "开机初始化日志", Cmd: "tail -n 100 /var/log/portal/provisioning.log", Enter: true},
		{ID: 4, Name: "显卡", Cmd: "nvidia-smi", Enter: true},
		{ID: 5, Name: "磁盘", Cmd: "df -h /workspace", Enter: true},
		{ID: 6, Name: "模型占用", Cmd: "du -sh /workspace/ComfyUI/models/* 2>/dev/null | sort -h", Enter: true},
		{ID: 7, Name: "进 ComfyUI 目录", Cmd: "cd /workspace/ComfyUI", Enter: true},
	}}}
}

func (s *Server) quickCmds() ([]quickGroup, error) {
	var gs []quickGroup
	ok, err := s.st.Get("quick_cmds", &gs)
	if err != nil {
		return nil, err
	}
	if !ok || gs == nil {
		return defaultQuickCmds(), nil
	}
	return gs, nil
}

func (s *Server) quickCmdsGet(w http.ResponseWriter, r *http.Request) {
	gs, err := s.quickCmds()
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"groups": gs})
}

// quickCmdsPut 整体替换；id <= 0 或重复的分类 / 命令分配新 id。
func (s *Server) quickCmdsPut(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Groups []quickGroup `json:"groups"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "请求格式不对："+err.Error())
		return
	}
	gs, err := normalizeQuickCmds(body.Groups)
	if err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return
	}
	if err := s.st.Set("quick_cmds", gs); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"groups": gs})
}

func normalizeQuickCmds(in []quickGroup) ([]quickGroup, error) {
	if len(in) > maxQuickGroups {
		return nil, fmt.Errorf("最多 %d 个分类", maxQuickGroups)
	}
	nextG, nextC, total := 0, 0, 0
	for _, g := range in {
		nextG = max(nextG, g.ID)
		for _, c := range g.Cmds {
			nextC = max(nextC, c.ID)
		}
		total += len(g.Cmds)
	}
	if total > maxQuickCmds {
		return nil, fmt.Errorf("最多 %d 条命令", maxQuickCmds)
	}
	seenG, seenC := map[int]bool{}, map[int]bool{}
	out := make([]quickGroup, 0, len(in))
	for _, g := range in {
		g.Name = truncRunes(strings.TrimSpace(g.Name), 30)
		if g.Name == "" {
			return nil, fmt.Errorf("分类名不能为空")
		}
		if g.ID <= 0 || seenG[g.ID] {
			nextG++
			g.ID = nextG
		}
		seenG[g.ID] = true
		cmds := make([]quickCmd, 0, len(g.Cmds))
		for _, c := range g.Cmds {
			c.Name = truncRunes(strings.TrimSpace(c.Name), 40)
			c.Cmd = strings.TrimRight(strings.ReplaceAll(c.Cmd, "\r\n", "\n"), "\n")
			if c.Name == "" {
				return nil, fmt.Errorf("命令名不能为空")
			}
			if strings.TrimSpace(c.Cmd) == "" {
				return nil, fmt.Errorf("%s：命令不能为空", c.Name)
			}
			if len(c.Cmd) > maxQuickCmdBytes {
				return nil, fmt.Errorf("%s：命令太长（最多 64 KB）", c.Name)
			}
			if c.ID <= 0 || seenC[c.ID] {
				nextC++
				c.ID = nextC
			}
			seenC[c.ID] = true
			cmds = append(cmds, c)
		}
		g.Cmds = cmds
		out = append(out, g)
	}
	return out, nil
}
