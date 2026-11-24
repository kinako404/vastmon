package panel

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"os"
	"path"
	"sort"
	"strings"
	"time"
)

// 文件浏览：经 SFTP 读写实例上的文件。路径都是实例上的绝对路径（POSIX）。
// 所有文件响应都带 CSP sandbox + nosniff：实例上的 html/svg 就算被直接打开，
// 也不能在面板的域名下跑脚本。只有图片 / 视频 / 音频 / 纯文本允许在浏览器里内联显示。

const textLimit = 5 << 20

type fileEntry struct {
	Name   string `json:"name"`
	Dir    bool   `json:"dir"`
	Link   bool   `json:"link"`
	Size   int64  `json:"size"`
	Mtime  int64  `json:"mtime"`
	Mode   string `json:"mode"`
	Broken bool   `json:"broken,omitempty"`
}

func cleanRemote(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" || !strings.HasPrefix(p, "/") {
		return "", errors.New("路径要以 / 开头")
	}
	return path.Clean(p), nil
}

func (s *Server) fileErr(w http.ResponseWriter, err error) {
	var status int
	msg := err.Error()
	switch {
	case errors.Is(err, fs.ErrNotExist):
		status, msg = http.StatusNotFound, "文件或目录不存在"
	case errors.Is(err, fs.ErrPermission):
		status, msg = http.StatusForbidden, "没有权限"
	default:
		status = http.StatusBadGateway
	}
	writeError(w, status, "FILE", msg)
}

func (s *Server) listFiles(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	c, err := s.sftpClient(id)
	if err != nil {
		writeError(w, http.StatusBadGateway, "SSH", err.Error())
		return
	}
	dir := r.URL.Query().Get("path")
	if dir == "" {
		if wd, err := c.Getwd(); err == nil {
			dir = wd
		} else {
			dir = "/"
		}
	}
	if dir, err = cleanRemote(dir); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return
	}
	infos, err := c.ReadDir(dir)
	if err != nil {
		s.fileErr(w, err)
		return
	}
	out := make([]fileEntry, 0, len(infos))
	for _, fi := range infos {
		e := fileEntry{Name: fi.Name(), Dir: fi.IsDir(), Size: fi.Size(), Mtime: fi.ModTime().Unix(), Mode: fi.Mode().String()}
		if fi.Mode()&fs.ModeSymlink != 0 {
			e.Link = true
			if t, err := c.Stat(path.Join(dir, fi.Name())); err == nil {
				e.Dir, e.Size = t.IsDir(), t.Size()
			} else {
				e.Broken = true
			}
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	writeJSON(w, map[string]any{"path": dir, "entries": out})
}

// inlineOK 是允许在浏览器里直接打开的类型。
func inlineOK(ctype string) bool {
	for _, p := range []string{"image/", "video/", "audio/", "text/plain"} {
		if strings.HasPrefix(ctype, p) && !strings.HasPrefix(ctype, "image/svg") {
			return true
		}
	}
	return false
}

func (s *Server) rawFile(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	p, err := cleanRemote(r.URL.Query().Get("path"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return
	}
	c, err := s.sftpClient(id)
	if err != nil {
		writeError(w, http.StatusBadGateway, "SSH", err.Error())
		return
	}
	f, err := c.Open(p)
	if err != nil {
		s.fileErr(w, err)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		s.fileErr(w, err)
		return
	}
	if st.IsDir() {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "这是目录")
		return
	}
	name := path.Base(p)
	ext := strings.ToLower(path.Ext(name))
	ctype := mime.TypeByExtension(ext)
	if ext == ".webm" {
		ctype = "video/webm"
	}
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	disp := "inline"
	if r.URL.Query().Get("dl") != "" || !inlineOK(ctype) {
		disp = "attachment"
	}
	if !inlineOK(ctype) {
		ctype = "application/octet-stream"
	}
	h := w.Header()
	h.Set("Content-Type", ctype)
	h.Set("Content-Disposition", mime.FormatMediaType(disp, map[string]string{"filename": name}))
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'; img-src 'self'; media-src 'self'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	http.ServeContent(w, r, "", st.ModTime(), f)
}

// safeName 只留文件名本身，去掉路径和控制字符。
func safeName(n string) string {
	n = path.Base(strings.ReplaceAll(n, "\\", "/"))
	n = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, n)
	if n == "." || n == ".." || n == "/" {
		return ""
	}
	return n
}

func (s *Server) uploadFiles(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	dir, err := cleanRemote(r.URL.Query().Get("dir"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return
	}
	overwrite := r.URL.Query().Get("overwrite") == "1"
	c, err := s.sftpClient(id)
	if err != nil {
		writeError(w, http.StatusBadGateway, "SSH", err.Error())
		return
	}
	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "要用 multipart/form-data 上传")
		return
	}
	done := []map[string]any{}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, "BAD_REQUEST", "上传中断："+err.Error())
			return
		}
		name := safeName(part.FileName())
		if name == "" {
			part.Close()
			continue
		}
		dest := path.Join(dir, name)
		if !overwrite {
			if _, err := c.Lstat(dest); err == nil {
				part.Close()
				writeError(w, http.StatusConflict, "EXISTS", name+" 已经存在")
				return
			}
		}
		tmp := dest + ".vastmon-upload"
		f, err := c.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
		if err != nil {
			part.Close()
			s.fileErr(w, err)
			return
		}
		n, err := io.Copy(f, part)
		cerr := f.Close()
		part.Close()
		if err == nil {
			err = cerr
		}
		if err == nil {
			err = c.PosixRename(tmp, dest)
		}
		if err != nil {
			_ = c.Remove(tmp)
			writeError(w, http.StatusBadGateway, "FILE", name+" 上传失败："+err.Error())
			return
		}
		log.Printf("实例 %d 上传 %s（%d 字节）", id, dest, n)
		done = append(done, map[string]any{"name": name, "size": n})
	}
	writeJSON(w, map[string]any{"uploaded": done})
}

func (s *Server) readText(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	p, err := cleanRemote(r.URL.Query().Get("path"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return
	}
	c, err := s.sftpClient(id)
	if err != nil {
		writeError(w, http.StatusBadGateway, "SSH", err.Error())
		return
	}
	st, err := c.Stat(p)
	if err != nil {
		s.fileErr(w, err)
		return
	}
	if st.IsDir() {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "这是目录")
		return
	}
	if st.Size() > textLimit {
		writeError(w, http.StatusRequestEntityTooLarge, "TOO_BIG", fmt.Sprintf("文件 %.1f MB，超过 5 MB 不在网页里编辑，请下载", float64(st.Size())/(1<<20)))
		return
	}
	f, err := c.Open(p)
	if err != nil {
		s.fileErr(w, err)
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, textLimit+1))
	if err != nil {
		s.fileErr(w, err)
		return
	}
	if bytes.IndexByte(data, 0) >= 0 {
		writeError(w, http.StatusUnsupportedMediaType, "BINARY", "这是二进制文件，不能当文本打开")
		return
	}
	writeJSON(w, map[string]any{"path": p, "content": string(data), "mtime": st.ModTime().UnixNano(), "size": st.Size(), "mode": st.Mode().String()})
}

func (s *Server) writeText(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Path    string `json:"path"`
		Content string `json:"content"`
		Mtime   int64  `json:"mtime"` // 打开时的修改时间；文件被别人改过就拒绝，除非 force
		Force   bool   `json:"force"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, textLimit+(64<<10))).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "请求格式不对："+err.Error())
		return
	}
	p, err := cleanRemote(body.Path)
	if err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return
	}
	c, err := s.sftpClient(id)
	if err != nil {
		writeError(w, http.StatusBadGateway, "SSH", err.Error())
		return
	}
	var mode fs.FileMode = 0o644
	if st, err := c.Stat(p); err == nil {
		if !body.Force && body.Mtime != 0 && st.ModTime().UnixNano() != body.Mtime {
			writeError(w, http.StatusConflict, "CHANGED", "文件在你打开之后被改过（"+st.ModTime().Format("15:04:05")+"）")
			return
		}
		mode = st.Mode().Perm()
	} else if !errors.Is(err, fs.ErrNotExist) {
		s.fileErr(w, err)
		return
	}
	tmp := p + ".vastmon-save"
	f, err := c.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		s.fileErr(w, err)
		return
	}
	_, err = io.Copy(f, strings.NewReader(body.Content))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = c.Chmod(tmp, mode)
	}
	// 记下自己写好的那一份的修改时间：改名之后再查，可能已经混进别人的改动。
	var st fs.FileInfo
	if err == nil {
		st, err = c.Lstat(tmp)
	}
	if err == nil {
		err = c.PosixRename(tmp, p)
	}
	if err != nil {
		_ = c.Remove(tmp)
		s.fileErr(w, err)
		return
	}
	log.Printf("实例 %d 保存 %s（%d 字节）", id, p, st.Size())
	writeJSON(w, map[string]any{"path": p, "mtime": st.ModTime().UnixNano(), "size": st.Size(), "saved_at": time.Now().Unix()})
}
