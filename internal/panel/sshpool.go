package panel

import (
	"bytes"
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// SSH 连接池：每台实例一条 SSH 连接，终端、文件浏览、算力检测共用。
// 主机指纹按实例 ID 记（host_keys 表）：vast 会把同一个 IP:端口 分给别的新实例，
// 按地址记会误报“密钥变了”；同一个实例的指纹变了才拒绝连接。

const hostKeysSchema = `
CREATE TABLE IF NOT EXISTS host_keys (instance_id INTEGER PRIMARY KEY, key BLOB, first_seen INTEGER);
`

const (
	sshIdle      = 15 * time.Minute
	sshKeepalive = 30 * time.Second
)

type pooledSSH struct {
	client *ssh.Client
	sftp   *sftp.Client
	target sshTarget
	users  int // 正在用的终端数
	last   time.Time
}

type sshPool struct {
	mu    sync.Mutex
	conns map[int64]*pooledSSH
	dial  sync.Map // instance id -> *sync.Mutex，同一台不并发拨号
}

func (s *Server) signer() (ssh.Signer, error) {
	c, _, err := s.sshConfig()
	if err != nil {
		return nil, err
	}
	return signerFor(c)
}

// hostKeyCheck 第一次连某个实例时记下它的主机指纹，之后必须一致。
func (s *Server) hostKeyCheck(instanceID int64) ssh.HostKeyCallback {
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		got := key.Marshal()
		var want []byte
		err := s.st.db.QueryRow(`SELECT key FROM host_keys WHERE instance_id = ?`, instanceID).Scan(&want)
		if errors.Is(err, sql.ErrNoRows) {
			_, err = s.st.db.Exec(`INSERT INTO host_keys (instance_id, key, first_seen) VALUES (?, ?, ?)`,
				instanceID, got, time.Now().Unix())
			return err
		}
		if err != nil {
			return err
		}
		if subtle.ConstantTimeCompare(want, got) != 1 {
			return fmt.Errorf("实例 %d 的 SSH 主机指纹和第一次连接时不一样，可能被中间人劫持，已拒绝连接", instanceID)
		}
		return nil
	}
}

func (s *Server) dialInstance(id int64) (*pooledSSH, error) {
	x, err := s.findInstance(id)
	if err != nil {
		return nil, err
	}
	if st := str(x["actual_status"]); st != "running" {
		return nil, fmt.Errorf("实例 %d 没在运行（%s）", id, st)
	}
	t, err := targetOf(x)
	if err != nil {
		return nil, err
	}
	sg, err := s.signer()
	if err != nil {
		return nil, err
	}
	client, err := ssh.Dial("tcp", net.JoinHostPort(t.Host, strconv.Itoa(t.Port)), &ssh.ClientConfig{
		User:            "root",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(sg)},
		HostKeyCallback: s.hostKeyCheck(id),
		Timeout:         15 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("SSH 连不上 %s:%d：%v", t.Host, t.Port, err)
	}
	p := &pooledSSH{client: client, target: t, last: time.Now()}
	go s.keepalive(id, p)
	return p, nil
}

func (s *Server) keepalive(id int64, p *pooledSSH) {
	t := time.NewTicker(sshKeepalive)
	defer t.Stop()
	for range t.C {
		_, _, err := p.client.SendRequest("keepalive@openssh.com", true, nil)
		s.pool.mu.Lock()
		idle := p.users == 0 && time.Since(p.last) > sshIdle
		cur := s.pool.conns[id] == p
		if err != nil || idle || !cur {
			if cur {
				delete(s.pool.conns, id)
			}
			s.pool.mu.Unlock()
			if p.sftp != nil {
				p.sftp.Close()
			}
			p.client.Close()
			return
		}
		s.pool.mu.Unlock()
	}
}

// conn 返回实例的 SSH 连接，没有就拨一条。
func (s *Server) conn(id int64) (*pooledSSH, error) {
	m, _ := s.pool.dial.LoadOrStore(id, &sync.Mutex{})
	dm := m.(*sync.Mutex)
	dm.Lock()
	defer dm.Unlock()
	s.pool.mu.Lock()
	if s.pool.conns == nil {
		s.pool.conns = map[int64]*pooledSSH{}
	}
	p := s.pool.conns[id]
	if p != nil {
		p.last = time.Now()
	}
	s.pool.mu.Unlock()
	if p != nil {
		return p, nil
	}
	p, err := s.dialInstance(id)
	if err != nil {
		return nil, err
	}
	s.pool.mu.Lock()
	s.pool.conns[id] = p
	s.pool.mu.Unlock()
	return p, nil
}

// drop 关掉实例的连接（出错后重连用）。
func (s *Server) drop(id int64, p *pooledSSH) {
	s.pool.mu.Lock()
	if s.pool.conns[id] == p {
		delete(s.pool.conns, id)
	}
	s.pool.mu.Unlock()
	if p.sftp != nil {
		p.sftp.Close()
	}
	p.client.Close()
}

// sftpClient 返回实例的 SFTP 客户端，挂在同一条 SSH 连接上。
func (s *Server) sftpClient(id int64) (*sftp.Client, error) {
	for attempt := 0; attempt < 2; attempt++ {
		p, err := s.conn(id)
		if err != nil {
			return nil, err
		}
		s.pool.mu.Lock()
		c := p.sftp
		s.pool.mu.Unlock()
		if c != nil {
			if _, err := c.Getwd(); err == nil {
				return c, nil
			}
			s.drop(id, p)
			continue
		}
		c, err = sftp.NewClient(p.client)
		if err != nil {
			s.drop(id, p)
			if attempt == 1 {
				return nil, fmt.Errorf("开 SFTP 失败：%v", err)
			}
			continue
		}
		s.pool.mu.Lock()
		p.sftp = c
		s.pool.mu.Unlock()
		return c, nil
	}
	return nil, errors.New("SSH 连接断了，重连也没成功")
}

// session 开一个会话；连接坏了就重拨一次。
func (s *Server) session(id int64) (*ssh.Session, *pooledSSH, error) {
	for attempt := 0; attempt < 2; attempt++ {
		p, err := s.conn(id)
		if err != nil {
			return nil, nil, err
		}
		sess, err := p.client.NewSession()
		if err == nil {
			return sess, p, nil
		}
		s.drop(id, p)
	}
	return nil, nil, errors.New("SSH 连接断了，重连也没成功")
}

// runOn 在实例上执行 cmd，stdin 作为标准输入，返回标准输出和标准错误。
func (s *Server) runOn(ctx context.Context, id int64, cmd string, stdin []byte) (string, string, error) {
	sess, _, err := s.session(id)
	if err != nil {
		return "", "", err
	}
	defer sess.Close()
	var out, errb bytes.Buffer
	sess.Stdin = bytes.NewReader(stdin)
	sess.Stdout, sess.Stderr = &out, &errb
	done := make(chan error, 1)
	go func() { done <- sess.Run(cmd) }()
	select {
	case err = <-done:
	case <-ctx.Done():
		sess.Signal(ssh.SIGKILL)
		sess.Close()
		err = errors.New("SSH 执行超时")
	}
	return out.String(), errb.String(), err
}
