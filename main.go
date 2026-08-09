package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"ds2api-browser/api"
	"ds2api-browser/browser"
	"ds2api-browser/config"
)

// dailyRotateWriter 按天滚动日志文件，同时输出到控制台
type dailyRotateWriter struct {
	mu       sync.Mutex
	dir      string
	prefix   string
	file     *os.File
	curDate  string
	maxDays  int
}

func newDailyRotateWriter(dir, prefix string, maxDays int) (*dailyRotateWriter, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	w := &dailyRotateWriter{dir: dir, prefix: prefix, maxDays: maxDays}
	// 启动时清理过期日志
	w.cleanOldLogs()
	return w, nil
}

func (w *dailyRotateWriter) Write(p []byte) (n int, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	today := time.Now().Format("2006-01-02")
	if w.curDate != today {
		if w.file != nil {
			w.file.Close()
		}
		fname := filepath.Join(w.dir, w.prefix+"-"+today+".log")
		f, err := os.OpenFile(fname, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			return 0, err
		}
		w.file = f
		w.curDate = today
	}

	// 写入文件
	if w.file != nil {
		w.file.Write(p)
	}

	// 同时输出到控制台
	return os.Stderr.Write(p)
}

func (w *dailyRotateWriter) cleanOldLogs() {
	cutoff := time.Now().AddDate(0, 0, -w.maxDays)
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), w.prefix+"-") || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			path := filepath.Join(w.dir, e.Name())
			os.Remove(path)
			log.Printf("[main] removed old log: %s", e.Name())
		}
	}
}

func (w *dailyRotateWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file != nil {
		return w.file.Close()
	}
	return nil
}

func main() {
	configPath := flag.String("config", "browser_config.json", "config file path")
	flag.Parse()

	// 初始化日志：同时写入 logs/ 目录（按天滚动，保留5天）和控制台
	logWriter, err := newDailyRotateWriter("logs", "ds2api", 5)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[main] log writer init failed: %v\n", err)
	} else {
		log.SetOutput(logWriter)
		log.SetFlags(log.LstdFlags)
		defer logWriter.Close()
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("[main] config load failed: %v", err)
	}
	if len(cfg.Accounts) == 0 {
		log.Println("[main] no accounts in config, add them via browser_config.json")
	}

	session := browser.NewSession(cfg)
	if err := session.Start(); err != nil {
		log.Fatalf("[main] browser start failed: %v", err)
	}
	log.Println("[main] browser launched")

	ctx := context.Background()
	if len(cfg.Accounts) > 0 {
		acc := cfg.Accounts[0]
		if err := session.Login(ctx, acc.Email, acc.Password); err != nil {
			log.Printf("[main] initial login failed: %v", err)
		} else {
			log.Println("[main] initial login success")
				}
	}

	// 释放端口：如果端口已被其他进程占用，自动杀掉
	freePort(cfg.Port)

	chatHandler := browser.NewChatHandler(session, cfg.ResponseTimeoutSec)
	// 后台空闲保活刷新：长时间无请求时定期刷新页面，避免渲染进程休眠导致请求卡顿
	chatHandler.StartIdleRefresher()
	chatHandler.DiagnoseDOM(ctx)
		apiHandler := api.NewHandler(cfg, chatHandler)

	addr := fmt.Sprintf(":%d", cfg.Port)
	server := &http.Server{
		Addr:         addr,
		Handler:      apiHandler.Router(),
		ReadTimeout:  60 * time.Second,
		WriteTimeout: 180 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		log.Printf("[main] listening on http://127.0.0.1%s", addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-serverErr:
		log.Printf("[main] server error: %v", err)
		log.Println("[main] the window will close in 10 seconds...")
		time.Sleep(10 * time.Second)
	case sig := <-sigCh:
		log.Printf("[main] received signal: %v", sig)
	}

	log.Println("[main] shutting down...")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("[main] server shutdown error: %v", err)
	}
	session.Close()
}

// freePort 检查端口是否被占用，如果是则杀掉占用进程
// 解决双击 exe 时因旧进程未退出导致端口冲突的问题
func freePort(port int) {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		return // 端口空闲
	}
	conn.Close()

	log.Printf("[main] port %d is in use, trying to kill the owning process...", port)
	out, err := exec.Command("netstat", "-ano").Output()
	if err != nil {
		log.Printf("[main] netstat failed: %v", err)
		return
	}
	portSuffix := fmt.Sprintf(":%d", port)
	for _, line := range strings.Split(string(out), "\n") {
		cols := strings.Fields(line)
		if len(cols) < 5 || cols[3] != "LISTENING" {
			continue
		}
		if !strings.HasSuffix(cols[1], portSuffix) {
			continue
		}
		pid := 0
		if _, err := fmt.Sscanf(cols[4], "%d", &pid); err != nil || pid <= 0 || pid == os.Getpid() {
			continue
		}
		log.Printf("[main] killing PID %d on port %d", pid, port)
		exec.Command("taskkill", "/F", "/PID", fmt.Sprintf("%d", pid)).Run()
		time.Sleep(500 * time.Millisecond)
		return // 杀掉一个就够了
	}
}
