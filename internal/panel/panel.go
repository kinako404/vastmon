// Package panel 是 vastmon 面板本体：vast.ai 机器监控、开机、实例管理、
// 算力检测、网页终端、启动下载器、浏览器通知。前端在 web/，打包进二进制。
//
// 定时抓取指定 GPU 型号的可租机器存进 SQLite，网页上按带宽、流量费、
// CPU 核数等条件过滤（vast 官网不能按流量费过滤）。
//
//	go run ./cmd/vastmon [-addr 0.0.0.0:8090] [-interval 2m] [-disk 96]
//	vastmon passwd    设置面板登录密码（面板所有内容都要登录）
//	vastmon version   显示版本
package panel

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func readKey(path string) (string, error) {
	if k := strings.TrimSpace(os.Getenv("VAST_API_KEY")); k != "" {
		return k, nil
	}
	candidates := []string{path}
	if path == "" {
		candidates = []string{"API.txt", "../API.txt"}
	}
	for _, p := range candidates {
		b, err := os.ReadFile(p)
		if err == nil && strings.TrimSpace(string(b)) != "" {
			return strings.TrimSpace(string(b)), nil
		}
	}
	return "", errors.New("找不到 API key：设置 VAST_API_KEY，或用 -key 指定文件（默认找 API.txt、../API.txt）")
}

// setPassword 从标准输入读面板密码（输入两次），设好后所有已登录会话失效。
func setPassword(a *Auth) error {
	in := bufio.NewReader(os.Stdin)
	read := func(prompt string) (string, error) {
		fmt.Fprint(os.Stderr, prompt)
		line, err := in.ReadString('\n')
		if err != nil && line == "" {
			return "", err
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	pw, err := read("新密码（输入会显示在屏幕上）: ")
	if err != nil {
		return err
	}
	if len([]rune(pw)) < 8 {
		return errors.New("密码至少 8 个字符")
	}
	again, err := read("再输入一次: ")
	if err != nil {
		return err
	}
	if pw != again {
		return errors.New("两次输入不一样")
	}
	if err := a.SetPassword(pw); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "密码已设置，之前登录的会话都已失效。")
	return nil
}

// version 由 Main 设置（cmd/vastmon 发布时用 -ldflags "-X main.version=v0.1.0" 写进去）。
var version = "dev"

// Main 解析命令行并运行面板（或 passwd / version 等子命令）。
func Main(v string) {
	version = v
	showVersion := flag.Bool("version", false, "显示版本后退出")
	addr := flag.String("addr", "0.0.0.0:8090", "面板监听地址")
	dbPath := flag.String("db", "vastmon.db", "SQLite 数据库文件")
	keyFile := flag.String("key", "", "API key 文件")
	interval := flag.Duration("interval", 2*time.Minute, "抓取间隔")
	disk := flag.Float64("disk", 96, "按多少 GB 磁盘计价（也是最低可用磁盘）")
	apiHost := flag.String("api", "https://console.vast.ai", "vast API 地址")
	flag.Parse()
	if *showVersion || flag.Arg(0) == "version" {
		fmt.Println("vastmon", version)
		return
	}

	absDB, err := filepath.Abs(*dbPath)
	if err != nil {
		log.Fatal(err)
	}
	dataDir := filepath.Dir(absDB)
	st, err := OpenStore(*dbPath, int64((3 * *interval).Seconds()))
	if err != nil {
		log.Fatalf("打开数据库失败: %v", err)
	}
	switch flag.Arg(0) {
	case "passwd":
		if err := setPassword(newAuth(st)); err != nil {
			log.Fatal(err)
		}
		return
	case "password-set": // 安装脚本用：设过密码退出码 0，没设过 1
		h, err := newAuth(st).passwordHash()
		if err != nil {
			log.Fatal(err)
		}
		if h == "" {
			fmt.Println("no")
			os.Exit(1)
		}
		fmt.Println("yes")
		return
	}
	key, err := readKey(*keyFile)
	if err != nil {
		log.Fatal(err)
	}
	cl := newClient(*apiHost, key)
	mon := &Monitor{st: st, cl: cl, disk: *disk, interval: *interval, trigger: make(chan struct{}, 1)}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go mon.Run(ctx)

	app := newServer(st, mon, cl, newAuth(st), dataDir)
	go app.setupLoop(ctx)
	app.downloadInit(ctx)
	go app.downloadLoop(ctx)
	app.scriptsInit()
	go app.scriptsLoop(ctx)
	go app.notifyLoop(ctx)
	go app.rentWatchLoop(ctx)
	srv := &http.Server{Addr: *addr, Handler: app.routes()}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Printf("vastmon %s，面板: http://%s", version, *addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
