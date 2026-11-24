package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// 面板通过 SSH 登实例（算力检测等）。私钥和它的密码只在服务器上用，
// 接口永远不把它们发给浏览器。没配置时用项目目录里的 private + passwd.txt。

type sshConfig struct {
	KeyPath    string `json:"key_path"`
	Passphrase string `json:"passphrase"`
}

// projectDirs 是“项目目录”：放 private + passwd.txt（默认 SSH 私钥）、backups/（ComfyUI
// input 备份、comfy_kit 的 sources.json）、d.sh 的地方。设了 VASTMON_PROJECT 就只用它
// （一键安装时设成数据目录），否则找工作目录的上一级和工作目录。
func projectDirs() []string {
	if d := strings.TrimSpace(os.Getenv("VASTMON_PROJECT")); d != "" {
		return []string{d}
	}
	return []string{"..", "."}
}

// projectKey 找项目目录里的 private 和 passwd.txt。
func projectKey() (path, pass string, ok bool) {
	for _, dir := range projectDirs() {
		p, err := filepath.Abs(filepath.Join(dir, "private"))
		if err != nil {
			continue
		}
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			b, _ := os.ReadFile(filepath.Join(filepath.Dir(p), "passwd.txt"))
			return p, strings.TrimSpace(string(b)), true
		}
	}
	return "", "", false
}

func (s *Server) sshConfig() (sshConfig, bool, error) {
	var c sshConfig
	ok, err := s.st.Get("ssh", &c)
	if err != nil {
		return c, false, err
	}
	if ok && c.KeyPath != "" {
		return c, false, nil
	}
	if p, pass, found := projectKey(); found {
		return sshConfig{KeyPath: p, Passphrase: pass}, true, nil
	}
	return c, false, nil
}

type sshTarget struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

func num(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case string:
		n, _ := strconv.Atoi(t)
		return n
	}
	return 0
}

// targetOf 取实例的 SSH 地址：优先直连（公网 IP + 22 端口映射），否则走 vast 的 SSH 代理。
func targetOf(x map[string]any) (sshTarget, error) {
	if ports, ok := x["ports"].(map[string]any); ok {
		if list, ok := ports["22/tcp"].([]any); ok && len(list) > 0 {
			if m, ok := list[0].(map[string]any); ok {
				ip, _ := x["public_ipaddr"].(string)
				if p := num(m["HostPort"]); p > 0 && ip != "" {
					return sshTarget{Host: strings.TrimSpace(ip), Port: p}, nil
				}
			}
		}
	}
	host, _ := x["ssh_host"].(string)
	if p := num(x["ssh_port"]); host != "" && p > 0 {
		return sshTarget{Host: host, Port: p}, nil
	}
	return sshTarget{}, errors.New("这台实例还没有 SSH 地址（可能还在启动）")
}

// signerFor 解开私钥；有密码的私钥没给密码时说清楚。
func signerFor(c sshConfig) (ssh.Signer, error) {
	if c.KeyPath == "" {
		return nil, errors.New("还没配置 SSH 私钥：到“设置”页配置")
	}
	pem, err := os.ReadFile(c.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("SSH 私钥文件读不到：%v", err)
	}
	if c.Passphrase == "" {
		sg, err := ssh.ParsePrivateKey(pem)
		var missing *ssh.PassphraseMissingError
		if errors.As(err, &missing) {
			return nil, errors.New("这把私钥有密码，到“设置”页填上")
		}
		if err != nil {
			return nil, fmt.Errorf("读不出私钥：%v", err)
		}
		return sg, nil
	}
	sg, err := ssh.ParsePrivateKeyWithPassphrase(pem, []byte(c.Passphrase))
	if err != nil {
		return nil, fmt.Errorf("私钥解不开（密码不对？）：%v", err)
	}
	return sg, nil
}

// publicKey 从私钥推出公钥（authorized_keys 格式）和 SHA256 指纹，顺便验证密码。
func publicKey(c sshConfig) (string, string, error) {
	sg, err := signerFor(c)
	if err != nil {
		return "", "", err
	}
	pub := sg.PublicKey()
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))), ssh.FingerprintSHA256(pub) + " (" + pub.Type() + ")", nil
}

// findInstance 在账户实例里找 id。
func (s *Server) findInstance(id int64) (map[string]any, error) {
	ins, err := s.cl.Instances()
	if err != nil {
		return nil, err
	}
	for _, x := range ins {
		if int64(num(x["id"])) == id {
			return x, nil
		}
	}
	return nil, fmt.Errorf("账户里没有实例 %d", id)
}

func (s *Server) sshSettings(w http.ResponseWriter, r *http.Request) {
	c, fromProject, err := s.sshConfig()
	if err != nil {
		s.fail(w, err)
		return
	}
	_, _, projOK := projectKey()
	res := map[string]any{
		"key_path": c.KeyPath, "has_passphrase": c.Passphrase != "", "using_project": fromProject,
		"project_available": projOK, "key_exists": false,
	}
	if c.KeyPath != "" {
		if _, err := os.Stat(c.KeyPath); err == nil {
			res["key_exists"] = true
			pub, fp, err := publicKey(c)
			if err != nil {
				res["key_error"] = err.Error()
			} else {
				res["public_key"], res["fingerprint"] = pub, fp
				if keys, err := s.cl.SSHKeys(); err == nil {
					in := false
					f := strings.Fields(pub)
					for _, k := range keys {
						kf := strings.Fields(k.PublicKey)
						if len(f) > 1 && len(kf) > 1 && kf[1] == f[1] {
							in = true
						}
					}
					res["in_vast_account"] = in
				}
			}
		} else {
			res["key_error"] = "文件不存在：" + c.KeyPath
		}
	}
	writeJSON(w, res)
}

func (s *Server) putSSHSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		KeyPath         string `json:"key_path"`
		Passphrase      string `json:"passphrase"`
		ClearPassphrase bool   `json:"clear_passphrase"`
		KeyContent      string `json:"key_content"`
		UseProject      bool   `json:"use_project"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	cur, _, err := s.sshConfig()
	if err != nil {
		s.fail(w, err)
		return
	}
	next := cur
	switch {
	case body.UseProject:
		if err := s.st.Set("ssh", sshConfig{}); err != nil {
			s.fail(w, err)
			return
		}
		s.sshSettings(w, r)
		return
	case strings.TrimSpace(body.KeyContent) != "":
		key := strings.TrimSpace(body.KeyContent) + "\n"
		if !strings.HasPrefix(key, "-----BEGIN") || !strings.Contains(key, "PRIVATE KEY-----") {
			writeError(w, http.StatusBadRequest, "BAD_REQUEST", "这不像 SSH 私钥（应以 -----BEGIN … PRIVATE KEY----- 开头）")
			return
		}
		p := filepath.Join(s.dataDir, "ssh_key")
		if err := os.WriteFile(p, []byte(key), 0o600); err != nil {
			s.fail(w, err)
			return
		}
		next = sshConfig{KeyPath: p}
	case strings.TrimSpace(body.KeyPath) != "":
		p, err := filepath.Abs(strings.TrimSpace(body.KeyPath))
		if err != nil {
			writeError(w, http.StatusBadRequest, "BAD_REQUEST", "路径不对")
			return
		}
		if p != cur.KeyPath {
			next.Passphrase = ""
		}
		next.KeyPath = p
	}
	if body.Passphrase != "" {
		next.Passphrase = body.Passphrase
	}
	if body.ClearPassphrase {
		next.Passphrase = ""
	}
	if _, err := os.Stat(next.KeyPath); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "私钥文件读不到："+next.KeyPath)
		return
	}
	if _, _, err := publicKey(next); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_KEY", err.Error())
		return
	}
	if err := s.st.Set("ssh", next); err != nil {
		s.fail(w, err)
		return
	}
	s.sshSettings(w, r)
}

func (s *Server) testSSH(w http.ResponseWriter, r *http.Request) {
	var body struct {
		InstanceID int64 `json:"instance_id"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	x, err := s.findInstance(body.InstanceID)
	if err != nil {
		writeError(w, http.StatusBadGateway, "VAST", err.Error())
		return
	}
	t, err := targetOf(x)
	if err != nil {
		writeError(w, http.StatusConflict, "NO_SSH", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	out, errOut, err := s.runOn(ctx, body.InstanceID, "echo VASTMON_OK; hostname; nvidia-smi -L", nil)
	ok := err == nil && strings.Contains(out, "VASTMON_OK")
	msg := strings.TrimSpace(strings.ReplaceAll(out, "VASTMON_OK\n", ""))
	if !ok {
		msg = strings.TrimSpace(errOut + "\n" + out)
		if err != nil && msg == "" {
			msg = err.Error()
		}
	}
	writeJSON(w, map[string]any{"ok": ok, "target": t, "output": msg})
}
