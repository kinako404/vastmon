package panel

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// input 目录（ComfyUI 里上传的图片 / 视频）的同步：备份是项目目录的
// backups/input.tar.gz（和 comfy_kit.py 同一个文件；一键安装时在数据目录下）。开机时推上去，
// 关机前拉回来合并：只增加 / 更新文件，不删本地已有的，同时开几台也不会互相覆盖。

const remoteInputTmp = "/tmp/vastmon-input.tar.gz"

func inputArchivePath() string {
	if p := projectPath("backups/input.tar.gz"); p != "" {
		return p
	}
	if d := projectPath("backups"); d != "" {
		return filepath.Join(d, "input.tar.gz")
	}
	p, err := filepath.Abs(filepath.Join(projectDirs()[0], "backups", "input.tar.gz"))
	if err != nil {
		return ""
	}
	return p
}

func inputArchiveInfo() map[string]any {
	p := inputArchivePath()
	out := map[string]any{"path": p, "exists": false}
	if p == "" {
		return out
	}
	if st, err := os.Stat(p); err == nil {
		out["exists"], out["size"], out["mtime"] = true, st.Size(), st.ModTime().Unix()
	}
	return out
}

type progressReader struct {
	r  io.Reader
	fn func(n int64)
	n  int64
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.n += int64(n)
	if p.fn != nil {
		p.fn(p.n)
	}
	return n, err
}

// pushInput 把 input.tar.gz 传到实例上解开（实例上已有的同名文件不覆盖）。
// progress(已传字节, 总字节)；返回实例上 input 里的文件数。
func (s *Server) pushInput(ctx context.Context, id int64, root string, progress func(done, total int64)) (int, error) {
	p := inputArchivePath()
	st, err := os.Stat(p)
	if p == "" || err != nil {
		return 0, errors.New("面板机上还没有 ComfyUI input 备份（backups/input.tar.gz）")
	}
	f, err := os.Open(p)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	// 经 SSH 会话的标准输入流过去（SFTP 逐块等回包，延迟高时只有几十 KB/s）；
	// 先存成临时文件，传完整了再解压。
	sess, _, err := s.session(id)
	if err != nil {
		return 0, err
	}
	defer sess.Close()
	sess.Stdin = &progressReader{r: f, fn: func(n int64) {
		if progress != nil {
			progress(n, st.Size())
		}
	}}
	var out, errb strings.Builder
	sess.Stdout, sess.Stderr = &out, &errb
	q := shellQuote(root)
	cmd := "cat > " + remoteInputTmp + " && mkdir -p " + q + " && tar xzf " + remoteInputTmp + " -C " + q + " --skip-old-files 2>/tmp/vastmon-input.err; rc=$?; rm -f " + remoteInputTmp +
		"; [ $rc -le 1 ] || { cat /tmp/vastmon-input.err >&2; exit $rc; }; find " + q + "/input -type f | wc -l"
	done := make(chan error, 1)
	go func() { done <- sess.Run(cmd) }()
	select {
	case err = <-done:
	case <-ctx.Done():
		sess.Close()
		return 0, ctx.Err()
	}
	if err != nil {
		return 0, fmt.Errorf("上传 / 解压 ComfyUI input 失败：%s", firstLine(errb.String()+" "+err.Error()))
	}
	n := 0
	fmt.Sscan(strings.TrimSpace(out.String()), &n)
	return n, nil
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

type backupResult struct {
	Added   int   `json:"added"`
	Updated int   `json:"updated"`
	Total   int   `json:"total"`
	Size    int64 `json:"size"`
}

func (b backupResult) String() string {
	return fmt.Sprintf("新增 %d 个、更新 %d 个，备份里共 %d 个文件（%.1f MB）", b.Added, b.Updated, b.Total, float64(b.Size)/1048576)
}

// countWriter 把写过去的字节数报给 fn。
type countWriter struct {
	w  io.Writer
	fn func(n int64)
	n  int64
}

func (c *countWriter) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	c.n += int64(n)
	if c.fn != nil {
		c.fn(c.n)
	}
	return n, err
}

// backupInput 把实例的 input 目录打包拉回来，和本地备份合并。progress(已拉回的字节) 可以为空。
// 拉的时候不占 inputMu（网络慢时要很久），合并时才占。
func (s *Server) backupInput(ctx context.Context, id int64, progress func(n int64)) (backupResult, error) {
	c, err := s.dlConfig()
	if err != nil {
		return backupResult{}, err
	}
	dst := inputArchivePath()
	if dst == "" {
		return backupResult{}, errors.New("找不到项目的 backups 目录")
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return backupResult{}, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".input-remote-*.tar.gz")
	if err != nil {
		return backupResult{}, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	sess, _, err := s.session(id)
	if err != nil {
		return backupResult{}, err
	}
	defer sess.Close()
	var errb strings.Builder
	sess.Stdout, sess.Stderr = &countWriter{w: tmp, fn: progress}, &errb
	runErr := make(chan error, 1)
	q := shellQuote(c.Root)
	go func() {
		runErr <- sess.Run("test -d " + q + "/input || { echo 实例上没有 " + q + "/input 目录（不是 ComfyUI 实例？）>&2; exit 3; }; tar czf - -C " + q + " input")
	}()
	select {
	case err = <-runErr:
	case <-ctx.Done():
		sess.Close()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return backupResult{}, errors.New("拉取 ComfyUI input 超时")
		}
		return backupResult{}, errors.New("拉取 ComfyUI input 被取消")
	}
	if err != nil {
		return backupResult{}, fmt.Errorf("打包实例上 ComfyUI 的 input 失败：%s", firstLine(errb.String()+" "+err.Error()))
	}
	if _, err := tmp.Seek(0, 0); err != nil {
		return backupResult{}, err
	}
	s.inputMu.Lock()
	res, err := mergeArchives(dst, tmp)
	s.inputMu.Unlock()
	if err != nil {
		return res, fmt.Errorf("合并 ComfyUI input 备份失败：%v", err)
	}
	return res, nil
}

type tarEntry struct {
	mtime time.Time
	size  int64
}

func readIndex(p string) (map[string]tarEntry, error) {
	idx := map[string]tarEntry{}
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return idx, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(zr)
	for {
		hd, err := tr.Next()
		if err == io.EOF {
			return idx, nil
		}
		if err != nil {
			return nil, err
		}
		idx[normTarName(hd.Name)] = tarEntry{hd.ModTime, hd.Size}
	}
}

func normTarName(n string) string {
	return strings.TrimPrefix(path.Clean("/"+strings.TrimPrefix(n, "./")), "/")
}

// mergeArchives 写出 本地 ∪ 远端：同名文件谁新用谁，本地独有的保留。
func mergeArchives(local string, remote io.Reader) (backupResult, error) {
	var res backupResult
	old, err := readIndex(local)
	if err != nil {
		return res, fmt.Errorf("读本地备份：%v", err)
	}
	out, err := os.CreateTemp(filepath.Dir(local), ".input-merged-*.tar.gz")
	if err != nil {
		return res, err
	}
	defer os.Remove(out.Name())
	zw := gzip.NewWriter(out)
	tw := tar.NewWriter(zw)
	written := map[string]bool{}

	zr, err := gzip.NewReader(remote)
	if err != nil {
		out.Close()
		return res, fmt.Errorf("实例传回的不是 tar.gz：%v", err)
	}
	tr := tar.NewReader(zr)
	for {
		hd, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			out.Close()
			return res, err
		}
		name := normTarName(hd.Name)
		prev, had := old[name]
		if had && prev.mtime.After(hd.ModTime) {
			continue
		}
		if hd.Typeflag == tar.TypeReg {
			switch {
			case !had:
				res.Added++
			case prev.size != hd.Size || !prev.mtime.Equal(hd.ModTime):
				res.Updated++
			}
		}
		if err := tw.WriteHeader(hd); err != nil {
			out.Close()
			return res, err
		}
		if _, err := io.Copy(tw, tr); err != nil {
			out.Close()
			return res, err
		}
		written[name] = true
	}
	if lf, err := os.Open(local); err == nil {
		lz, err := gzip.NewReader(lf)
		if err != nil {
			lf.Close()
			out.Close()
			return res, err
		}
		lt := tar.NewReader(lz)
		for {
			hd, err := lt.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				lf.Close()
				out.Close()
				return res, err
			}
			if written[normTarName(hd.Name)] {
				continue
			}
			if err := tw.WriteHeader(hd); err != nil {
				lf.Close()
				out.Close()
				return res, err
			}
			if _, err := io.Copy(tw, lt); err != nil {
				lf.Close()
				out.Close()
				return res, err
			}
			written[normTarName(hd.Name)] = true
		}
		lf.Close()
	}
	if err := tw.Close(); err != nil {
		out.Close()
		return res, err
	}
	if err := zw.Close(); err != nil {
		out.Close()
		return res, err
	}
	if err := out.Close(); err != nil {
		return res, err
	}
	res.Total = countFiles(out.Name())
	if st, err := os.Stat(out.Name()); err == nil {
		res.Size = st.Size()
	}
	if _, err := os.Stat(local); err == nil {
		if err := os.Rename(local, local+".prev"); err != nil {
			return res, err
		}
	}
	if err := os.Rename(out.Name(), local); err != nil {
		return res, err
	}
	return res, os.Chmod(local, 0o644)
}

func countFiles(p string) int {
	f, err := os.Open(p)
	if err != nil {
		return 0
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return 0
	}
	tr := tar.NewReader(zr)
	n := 0
	for {
		hd, err := tr.Next()
		if err != nil {
			return n
		}
		if hd.Typeflag == tar.TypeReg {
			n++
		}
	}
}

// backupInputNow 是实例卡片上的“备份 ComfyUI input”：开一个后台任务就返回，进度在
// GET /api/input/jobs 里。
func (s *Server) backupInputNow(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	j, err := s.startInputJob(id, "", nil)
	if err != nil {
		writeError(w, http.StatusConflict, "BUSY", err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "job": j})
}
