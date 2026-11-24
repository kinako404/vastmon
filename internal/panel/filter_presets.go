package panel

import (
	"net/http"
	"strings"
)

// 过滤条件方案：把常用的一组过滤条件存个名字，在机器页一点就切换。
// 存在 settings 表的 "filter_presets"。切换方案就是 PUT /api/filter。

type filterPreset struct {
	Name   string `json:"name"`
	Filter Filter `json:"filter"`
}

func (s *Server) filterPresets() ([]filterPreset, error) {
	ps := []filterPreset{}
	_, err := s.st.Get("filter_presets", &ps)
	if ps == nil {
		ps = []filterPreset{}
	}
	return ps, err
}

func (s *Server) writePresets(w http.ResponseWriter, ps []filterPreset) {
	if err := s.st.Set("filter_presets", ps); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"presets": ps})
}

// savePreset 新建或覆盖一个方案；带 old_name 时是改名（同时可更新条件）。
func (s *Server) savePreset(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name    string  `json:"name"`
		OldName string  `json:"old_name"`
		Filter  *Filter `json:"filter"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	name := truncRunes(strings.TrimSpace(body.Name), 30)
	if name == "" {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "方案名不能为空")
		return
	}
	ps, err := s.filterPresets()
	if err != nil {
		s.fail(w, err)
		return
	}
	old := strings.TrimSpace(body.OldName)
	if old == "" {
		old = name
	}
	idx := -1
	for i, p := range ps {
		if p.Name == old {
			idx = i
		}
		if p.Name == name && old != name {
			writeError(w, http.StatusConflict, "EXISTS", "已经有叫“"+name+"”的方案")
			return
		}
	}
	switch {
	case idx >= 0:
		ps[idx].Name = name
		if body.Filter != nil {
			ps[idx].Filter = *body.Filter
		}
	case body.Filter == nil:
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "缺少过滤条件")
		return
	case len(ps) >= 30:
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "方案最多 30 个")
		return
	default:
		ps = append(ps, filterPreset{Name: name, Filter: *body.Filter})
	}
	for i := range ps {
		if ps[i].Filter.GPUs == nil {
			ps[i].Filter.GPUs = []string{}
		}
	}
	s.writePresets(w, ps)
}

func (s *Server) deletePreset(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	ps, err := s.filterPresets()
	if err != nil {
		s.fail(w, err)
		return
	}
	out := ps[:0]
	for _, p := range ps {
		if p.Name != name {
			out = append(out, p)
		}
	}
	s.writePresets(w, out)
}
