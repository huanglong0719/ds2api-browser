package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ds2api-browser/config"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

type Session struct {
	cfg                 *config.Config
	chromeCmd           *exec.Cmd
	allocCtx            context.Context
	allocCancel         context.CancelFunc
	browserCtx          context.Context
	browserCancel       context.CancelFunc
	ctxMu               sync.Mutex
	loggedIn            atomic.Bool
	port                int
	currentAccountIdx   int    // 当前使用的账号索引 (cfg.Accounts 中的位置)
	currentEmail        string // 当前登录的账号邮箱
	sessionRequestCount int    // 当前会话成功请求数
	stats               *StatsManager
	sortedIndices       []int // 根据质量排序后的账号索引列表
	currentSortedIdx    int   // 当前在排序列表中的位置
}

func NewSession(cfg *config.Config) *Session {
	s := &Session{
		cfg:   cfg,
		stats: NewStatsManager(),
	}
	s.reorderAccounts()
	return s
}

// reorderAccounts 根据历史统计对账号按质量降序排序
// 质量分 = 成功请求数 / (1 + 限制触发次数)
// 登录失败次数 > 0 的账号会被禁用并从排序列表中排除
func (s *Session) reorderAccounts() {
	s.sortedIndices = s.stats.GetSortedIndices(s.cfg.Accounts)
	log.Printf("[session] accounts reordered by quality: %v", s.sortedIndices)
	// 报告被禁用的账号，提醒用户处理
	s.stats.LogDisabledAccounts(s.cfg.Accounts)
}

func (s *Session) Start() error {
	profileDir, err := s.resolveProfileDir()
	if err != nil {
		return fmt.Errorf("resolve profile: %w", err)
	}

	s.port = 9222
	if s.isPortListening(s.port) {
		log.Printf("[session] killing existing Chrome on port %d", s.port)
		for _, proc := range s.findProcessOnPort(s.port) {
			if proc != 0 {
				log.Printf("[session] killing PID %d via taskkill", proc)
				exec.Command("taskkill", "/F", "/PID", fmt.Sprintf("%d", proc)).Run()
			}
		}
		time.Sleep(1 * time.Second)
	}

	s.clearProfileLocks(profileDir)

	chromePath := s.cfg.ChromePath
	if chromePath == "" {
		chromePath = s.findChromeExecutable()
	}

	args := []string{
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-popup-blocking",
		"--disable-extensions",
		"--no-sandbox",
		"--disable-dev-shm-usage",
		"--disable-gpu",
		"--disable-session-crashed-bubble",
		"--disable-infobars",
		"--disable-background-networking",
		"--disable-sync",
		"--disable-blink-features=AutomationControlled",
		"--disable-features=TranslateUI",
		fmt.Sprintf("--remote-debugging-port=%d", s.port),
		fmt.Sprintf("--user-data-dir=%s", profileDir),
		"--window-size=900,600",
	}
	s.chromeCmd = exec.Command(chromePath, args...)
	s.chromeCmd.Stdout = io.Discard
	s.chromeCmd.Stderr = io.Discard

	if err := s.chromeCmd.Start(); err != nil {
		return fmt.Errorf("start Chrome: %w", err)
	}
	log.Printf("[session] Chrome pid=%d, waiting for CDP...", s.chromeCmd.Process.Pid)

	if err := s.waitForCDP(10 * time.Second); err != nil {
		return fmt.Errorf("wait for CDP: %w", err)
	}

	return nil
}

func (s *Session) initContexts() error {
	wsURL, err := s.getBrowserWSURL()
	if err != nil {
		return fmt.Errorf("get WS URL: %w", err)
	}
	s.allocCtx, s.allocCancel = chromedp.NewRemoteAllocator(context.Background(), wsURL)
	log.Printf("[session] connected to Chrome")

	targetID := s.findDeepSeekTarget()
	if targetID != "" {
		s.browserCtx, s.browserCancel = s.newBrowserCtx(s.allocCtx, targetID)
		log.Printf("[session] reusing existing target: %s", targetID)
	} else {
		s.browserCtx, s.browserCancel = s.newBrowserCtx(s.allocCtx, "")
		log.Println("[session] created new target")
	}

	// 强制设置浏览器窗口大小（覆盖 Chrome 记忆的上次窗口状态）
	s.setWindowSize(900, 600)

	// 关闭多余的标签页（保留当前 DeepSeek 页，关闭其他所有页面标签）
	s.closeExtraTargets()

	return nil
}

// closeExtraTargets 关闭多余的标签页（保留当前正在使用的 DeepSeek 页，关闭其他所有页面标签）
// 解决多标签页导致的操作目标混乱问题
// 使用 Chrome DevTools Protocol HTTP API 而非 chromedp.Targets（后者常因 context 未就绪而失败）
func (s *Session) closeExtraTargets() {
	currentTargetID := target.ID("")
	if s.browserCtx != nil {
		if id := chromedp.FromContext(s.browserCtx); id != nil && id.Target != nil {
			currentTargetID = id.Target.TargetID
		}
	}
	if currentTargetID == "" {
		return
	}

	// 通过 HTTP API 获取所有标签页，避免 chromedp.Targets 的 context 有效性依赖
	apiURL := fmt.Sprintf("http://127.0.0.1:%d/json", s.port)
	httpClient := &http.Client{Timeout: 5 * time.Second}
	resp, err := httpClient.Get(apiURL)
	if err != nil {
		log.Printf("[session] closeExtraTargets: http get failed: %v", err)
		return
	}
	defer resp.Body.Close()

	var targets []struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		URL  string `json:"url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&targets); err != nil {
		log.Printf("[session] closeExtraTargets: decode failed: %v", err)
		return
	}

	closed := 0
	for _, t := range targets {
		if t.Type != "page" {
			continue
		}
		if target.ID(t.ID) == currentTargetID {
			continue
		}
		// 通过 HTTP API 关闭标签页
		closeURL := fmt.Sprintf("http://127.0.0.1:%d/json/close/%s", s.port, t.ID)
		closeResp, closeErr := httpClient.Get(closeURL)
		if closeErr != nil {
			log.Printf("[session] closeExtraTargets: close %s failed: %v", t.ID, closeErr)
			continue
		}
		closeResp.Body.Close()

		closed++
		if strings.Contains(t.URL, "chat.deepseek.com") {
			log.Printf("[session] closed extra DeepSeek tab: %s (url=%s)", t.ID, t.URL)
		} else {
			log.Printf("[session] closed non-DeepSeek tab: %s (url=%s)", t.ID, t.URL)
		}
	}
	if closed > 0 {
		log.Printf("[session] closed %d extra tab(s) total", closed)
	}
}

func (s *Session) setWindowSize(width, height int) {
	var windowID browser.WindowID
	err := chromedp.Run(s.browserCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		var targetID target.ID
		if s.browserCtx != nil {
			if id := chromedp.FromContext(s.browserCtx); id != nil && id.Target != nil {
				targetID = id.Target.TargetID
			}
		}
		wid, _, err := browser.GetWindowForTarget().WithTargetID(targetID).Do(ctx)
		if err != nil {
			return err
		}
		windowID = wid
		return nil
	}))
	if err != nil {
		log.Printf("[session] get window id warning: %v", err)
		return
	}
	err = chromedp.Run(s.browserCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		return browser.SetWindowBounds(windowID, &browser.Bounds{
			Left:        0,
			Top:         0,
			Width:       int64(width),
			Height:      int64(height),
			WindowState: browser.WindowStateNormal,
		}).Do(ctx)
	}))
	if err != nil {
		log.Printf("[session] set window size warning: %v", err)
	} else {
		log.Printf("[session] window size set to %dx%d", width, height)
	}
}

// newBrowserCtx 创建带断链诊断能力的 browserCtx：
// 1. 注册 chromedp 错误回调——连接断开时把底层原因（websocket 关闭码/读取错误）写入日志
// 2. 监听 target 事件——页面崩溃(TargetCrashed)/销毁(TargetDestroyed)/分离(DetachedFromTarget)写入日志
// 用途：区分"页面崩了"还是"连接断了"，为断链证据链闭环提供直接证据
func (s *Session) newBrowserCtx(allocCtx context.Context, targetID target.ID) (context.Context, context.CancelFunc) {
	opts := []chromedp.ContextOption{
		chromedp.WithErrorf(func(format string, args ...any) {
			log.Printf("[chromedp][diag] "+format, args...)
		}),
	}
	var ctx context.Context
	var cancel context.CancelFunc
	if targetID != "" {
		ctx, cancel = chromedp.NewContext(allocCtx, append(opts, chromedp.WithTargetID(targetID))...)
	} else {
		ctx, cancel = chromedp.NewContext(allocCtx, opts...)
	}
	chromedp.ListenTarget(ctx, func(ev any) {
		switch e := ev.(type) {
		case *target.EventTargetCrashed:
			log.Printf("[session][diag] TARGET CRASHED: targetId=%s status=%s errorCode=%d", e.TargetID, e.Status, e.ErrorCode)
		case *target.EventTargetDestroyed:
			log.Printf("[session][diag] TARGET DESTROYED: targetId=%s", e.TargetID)
		case *target.EventDetachedFromTarget:
			log.Printf("[session][diag] TARGET DETACHED: sessionId=%s", e.SessionID)
		}
	})
	return ctx, cancel
}

// resetCtxLocked 在已持有 ctxMu 锁的前提下重置浏览器上下文。
// 调用方必须先获取 s.ctxMu 锁。
func (s *Session) resetCtxLocked() {
	if s.browserCancel != nil {
		s.browserCancel()
	}
	if s.allocCancel != nil {
		s.allocCancel()
	}

	wsURL, err := s.getBrowserWSURL()
	if err != nil {
		log.Printf("[session] reset: get WS URL failed: %v", err)
		return
	}
	s.allocCtx, s.allocCancel = chromedp.NewRemoteAllocator(context.Background(), wsURL)

	targetID := s.findDeepSeekTarget()
	if targetID != "" {
		s.browserCtx, s.browserCancel = s.newBrowserCtx(s.allocCtx, targetID)
		log.Printf("[session] reset: reusing target %s", targetID)
	} else {
		s.browserCtx, s.browserCancel = s.newBrowserCtx(s.allocCtx, "")
		log.Println("[session] reset: new target")
	}

	s.setWindowSize(900, 600)
}

// resetCtx 重置浏览器上下文（内部自动加锁）。
func (s *Session) resetCtx() {
	s.ctxMu.Lock()
	defer s.ctxMu.Unlock()
	s.resetCtxLocked()
}

// AbortConnection 强制断开浏览器连接（取消 browserCtx/allocCtx）。
// [Fix 2026-08-10] 用于 chromedp 命令挂起（页面被 Memory Saver 冻结导致 TCP 半死、
// context 超时失效，实测卡 36 分钟）时解除阻塞——断开后卡住的命令立即返回错误。
// 断开后下一次 Session.Context() 调用会检测到连接断开，自动重启浏览器恢复（登录态保留在 profile）。
func (s *Session) AbortConnection() {
	s.ctxMu.Lock()
	defer s.ctxMu.Unlock()
	if s.browserCancel != nil {
		s.browserCancel()
	}
	if s.allocCancel != nil {
		s.allocCancel()
	}
	s.browserCtx, s.browserCancel = nil, nil
	s.allocCtx, s.allocCancel = nil, nil
	log.Println("[session] browser connection aborted (hung command watchdog)")
}

// RebuildPage 在页面被 Chrome Memory Saver 卸载（渲染进程销毁、CDP 命令挂起）后重建 DeepSeek 页面。
// 流程：新建标签页导航回 DeepSeek → 等待就绪 → 清理旧标签页。
// 注意顺序：必须先建新标签页再关旧标签页——脚本只保留一个 DeepSeek 标签页，
// 若先关闭旧标签页，浏览器就一个标签页都没有了，Chrome 进程会随之退出。
// 与重启浏览器（restartBrowserLocked）不同：Chrome 进程还活着，只重建页面，登录态保留在 profile 中。
// 调用方必须持有 h.mu（由 refreshPageLocked 触发，避免与 sendChat 冲突）。
func (s *Session) RebuildPage() error {
	s.ctxMu.Lock()
	defer s.ctxMu.Unlock()

	// 1. 断开旧浏览器上下文（只断开 CDP 连接，不关闭标签页）
	if s.browserCancel != nil {
		s.browserCancel()
	}
	if s.allocCancel != nil {
		s.allocCancel()
	}

	// 2. 重建浏览器上下文：newBrowserCtx 传入空 targetID 会让 chromedp 自动创建新标签页，
	//    此时浏览器仍保留旧标签页，进程不会退出
	wsURL, err := s.getBrowserWSURL()
	if err != nil {
		return fmt.Errorf("rebuild: get WS URL: %w", err)
	}
	s.allocCtx, s.allocCancel = chromedp.NewRemoteAllocator(context.Background(), wsURL)
	s.browserCtx, s.browserCancel = s.newBrowserCtx(s.allocCtx, "")
	s.setWindowSize(900, 600)

	// 3. 导航回 DeepSeek 首页（登录态保留在 profile，加载后仍处于登录状态）
	navCtx, navCancel := context.WithTimeout(s.browserCtx, 15*time.Second)
	defer navCancel()
	if err := chromedp.Run(navCtx, chromedp.Navigate("https://chat.deepseek.com/")); err != nil {
		return fmt.Errorf("rebuild: navigate DeepSeek: %w", err)
	}

	// 4. 等待聊天页面就绪（textarea 出现 = 加载完成 + 已登录）
	taDeadline := time.Now().Add(20 * time.Second)
	var taOK bool
	for time.Now().Before(taDeadline) {
		probeCtx, probeCancel := context.WithTimeout(s.browserCtx, 3*time.Second)
		err := chromedp.Run(probeCtx, chromedp.Evaluate(`!!document.querySelector('textarea')`, &taOK))
		probeCancel()
		if err == nil && taOK {
			log.Println("[session] rebuild: chat page ready (textarea found)")
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !taOK {
		return fmt.Errorf("rebuild: chat page not ready after 20s (login may be invalid)")
	}

	// 5. 新标签页就绪后，清理旧标签页（此时浏览器至少还有一个新标签页，关闭旧页不会退出进程）
	s.closeExtraTargets()

	// 6. 重新检测并开启深度思考（新页面默认关闭）
	toggleCtx, toggleCancel := context.WithTimeout(s.browserCtx, 15*time.Second)
	defer toggleCancel()
	s.checkToggleStatesLocked(toggleCtx)

	log.Println("[session] DeepSeek page rebuilt successfully")
	return nil
}

// diagnoseConnectionLoss 断链瞬间的诊断探针（2026-08-09 用户要求）：
// 在检测到与浏览器连接断开、准备重启之前，记录当时的确定性状态，用于区分根因：
//   - browserCtx 的取消原因（chromedp 层错误信息）
//   - Chrome 进程是否还活着（已退出=外部原因；活着=连接层问题）
//   - CDP 端口是否还在监听
//   - 系统物理内存（内存耗尽可能导致 Chrome 进程被系统回收）
// 调用时机：restartBrowserLocked 最开头、取消旧连接/杀 Chrome 之前，保证记录的是断链当时的真实状态。
func (s *Session) diagnoseConnectionLoss() {
	if s.browserCtx != nil {
		log.Printf("[diag] conn-lost: browserCtx.Err=%v", s.browserCtx.Err())
	}
	if s.chromeCmd != nil && s.chromeCmd.Process != nil {
		pid := s.chromeCmd.Process.Pid
		log.Printf("[diag] conn-lost: chrome pid=%d alive=%v", pid, s.isProcessAlive(pid))
	}
	log.Printf("[diag] conn-lost: port %d listening=%v", s.port, s.isPortListening(s.port))
	if m := windowsMemoryInfo(); m != nil {
		log.Printf("[diag] conn-lost: mem total=%dMB avail=%dMB load=%d%%", m.TotalMB, m.AvailMB, m.Load)
	}
}

// isProcessAlive 通过 tasklist 检查 PID 对应进程是否存活（Windows）
func (s *Session) isProcessAlive(pid int) bool {
	out, err := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/NH").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), fmt.Sprintf("%d", pid))
}

// restartBrowserLocked 彻底重启 Chrome 浏览器（保留用户数据目录，登录态不丢）。
// 在检测到与浏览器的连接真正断开时调用（如 CDP websocket 丢失，chromedp 会自动取消 context）。
// 调用方必须持有 s.ctxMu 锁（与 resetCtxLocked 相同）。
// 重启后创建全新连接上下文并导航回 DeepSeek 首页；登录态保存在 profile 目录中，无需重新登录。
func (s *Session) restartBrowserLocked() error {
	log.Println("[session] browser connection lost, restarting Chrome...")
	// [Fix 2026-08-09] 断链诊断探针：记录断链瞬间 Chrome 存活/端口/内存状态，
	// 用于下次断链时区分根因（外部原因 vs 连接层问题），必须在杀 Chrome 之前执行
	s.diagnoseConnectionLoss()

	// 1. 取消旧连接上下文
	if s.browserCancel != nil {
		s.browserCancel()
	}
	if s.allocCancel != nil {
		s.allocCancel()
	}
	s.browserCtx, s.browserCancel = nil, nil
	s.allocCtx, s.allocCancel = nil, nil

	// 2. 结束旧 Chrome 进程（先 Kill，再 taskkill 兜底清理端口占用）
	if s.chromeCmd != nil && s.chromeCmd.Process != nil {
		_ = s.chromeCmd.Process.Kill()
		_ = s.chromeCmd.Wait() // 回收进程资源
	}
	for _, proc := range s.findProcessOnPort(s.port) {
		if proc != 0 {
			log.Printf("[session] restart: killing PID %d via taskkill", proc)
			_ = exec.Command("taskkill", "/F", "/PID", fmt.Sprintf("%d", proc)).Run()
		}
	}

	// 3. 等待端口释放，避免新实例与旧实例冲突
	deadline := time.Now().Add(10 * time.Second)
	for s.isPortListening(s.port) && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond)

	// 4. 清理 profile 残留锁文件
	profileDir, err := s.resolveProfileDir()
	if err != nil {
		return fmt.Errorf("restart: resolve profile: %w", err)
	}
	s.clearProfileLocks(profileDir)

	// 5. 重新启动 Chrome（参数与 Start 一致，保留 profile 登录态）
	chromePath := s.cfg.ChromePath
	if chromePath == "" {
		chromePath = s.findChromeExecutable()
	}
	args := []string{
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-popup-blocking",
		"--disable-extensions",
		"--no-sandbox",
		"--disable-dev-shm-usage",
		"--disable-gpu",
		"--disable-session-crashed-bubble",
		"--disable-infobars",
		"--disable-background-networking",
		"--disable-sync",
		"--disable-blink-features=AutomationControlled",
		"--disable-features=TranslateUI",
		fmt.Sprintf("--remote-debugging-port=%d", s.port),
		fmt.Sprintf("--user-data-dir=%s", profileDir),
		"--window-size=900,600",
	}
	s.chromeCmd = exec.Command(chromePath, args...)
	s.chromeCmd.Stdout = io.Discard
	s.chromeCmd.Stderr = io.Discard
	if err := s.chromeCmd.Start(); err != nil {
		return fmt.Errorf("restart: start Chrome: %w", err)
	}
	log.Printf("[session] Chrome restarted pid=%d, waiting for CDP...", s.chromeCmd.Process.Pid)

	// 6. 等待 CDP 就绪
	if err := s.waitForCDP(15 * time.Second); err != nil {
		return fmt.Errorf("restart: wait for CDP: %w", err)
	}

	// 7. 重建连接上下文
	wsURL, err := s.getBrowserWSURL()
	if err != nil {
		return fmt.Errorf("restart: get WS URL: %w", err)
	}
	s.allocCtx, s.allocCancel = chromedp.NewRemoteAllocator(context.Background(), wsURL)
	s.browserCtx, s.browserCancel = s.newBrowserCtx(s.allocCtx, "")

	// 8. 设置窗口并导航回 DeepSeek 首页（登录态保留在 profile，加载后仍处于登录状态）
	s.setWindowSize(900, 600)
	navCtx, navCancel := context.WithTimeout(s.browserCtx, 15*time.Second)
	defer navCancel()
	// [Fix 2026-08-09] 导航失败必须报错，不能吞掉继续——否则重启"假成功"，
	// 会在页面未就绪的状态下继续被使用，导致重启后 1-2 秒又断连（2026-08-09 观察：循环 12 次）
	if err := chromedp.Run(navCtx, chromedp.Navigate("https://chat.deepseek.com/")); err != nil {
		return fmt.Errorf("restart: navigate DeepSeek: %w", err)
	}

	// [Fix 2026-08-09] 重启后健康检查：轮询等待聊天页面（textarea）出现。
	// textarea 出现 = 页面加载完成 + 已登录 + 连接可用（DeepSeek 未登录时只显示登录页，不会有输入框），
	// 一次验证三者。替代原固定 sleep 2s（页面加载慢时不够，导致"假成功"后立即被长文本输入击穿又断连）。
	taDeadline := time.Now().Add(20 * time.Second)
	var taOK bool
	for time.Now().Before(taDeadline) {
		if err := chromedp.Run(s.browserCtx,
			chromedp.Evaluate(`!!document.querySelector('textarea')`, &taOK),
		); err == nil && taOK {
			log.Println("[session] restart: chat page ready (textarea found)")
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !taOK {
		// 等不到聊天页面 = 登录状态失效。自动重新登录当前账号；
		// checkAndLogin 使用传入 ctx，不调 s.Context()，不会与已持有的 ctxMu 锁死锁
		log.Printf("[session] restart: chat page not ready in 20s, login state invalid, re-login %s", s.currentEmail)
		if s.currentEmail == "" {
			return fmt.Errorf("restart: chat page not ready and no current account to re-login")
		}
		reCtx, reCancel := context.WithTimeout(s.browserCtx, 40*time.Second)
		defer reCancel()
		acct := s.findAccount(s.currentEmail)
		if acct == nil {
			return fmt.Errorf("restart: account %s not found in config", s.currentEmail)
		}
		if err := s.checkAndLogin(reCtx, acct.Email, acct.Password); err != nil {
			return fmt.Errorf("restart: re-login %s: %w", acct.Email, err)
		}
		s.loggedIn.Store(true)
		log.Printf("[session] restart: re-login %s successful", acct.Email)
	}

	// 9. 检查并确保"深度思考"处于开启状态。
	// 重启浏览器=全新打开 DeepSeek 页面，深度思考默认关闭，必须重新开启，否则重启后回复将不带思考过程。
	// 直接用 s.browserCtx 调用 checkToggleStatesLocked——本函数已持有 s.ctxMu 锁，不能再调 s.Context()（会二次加锁死锁）。
	toggleCtx, toggleCancel := context.WithTimeout(s.browserCtx, 15*time.Second)
	defer toggleCancel()
	s.checkToggleStatesLocked(toggleCtx)

	log.Println("[session] browser restarted successfully")
	return nil
}

func (s *Session) Context() context.Context {
	s.ctxMu.Lock()
	defer s.ctxMu.Unlock()
	if s.browserCtx != nil && s.browserCtx.Err() == nil {
		return s.browserCtx
	}
	// 连接已断开（chromedp 检测到浏览器连接丢失会自动取消 context）。
	// 直接重启浏览器恢复：保留用户数据目录，登录态不丢；重启后页面自动回到 DeepSeek 首页。
	// 不采用轻量重建（resetCtxLocked）——连接断开时页面状态不可信，干净重启最可靠。
	if err := s.restartBrowserLocked(); err != nil {
		log.Printf("[session] restart browser failed: %v", err)
		// 兜底：返回一个已取消的占位 context，避免 nil 崩溃，让上层快速失败并可在下次请求时重试
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return ctx
	}
	return s.browserCtx
}

func (s *Session) findDeepSeekTarget() target.ID {
	infos, err := chromedp.Targets(s.allocCtx)
	if err != nil {
		return ""
	}
	for _, info := range infos {
		if info.Type == "page" && strings.Contains(info.URL, "chat.deepseek.com") {
			return info.TargetID
		}
	}
	return ""
}

func (s *Session) waitForCDP(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	url := fmt.Sprintf("http://127.0.0.1:%d/json/version", s.port)
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			log.Printf("[session] CDP ready on port %d", s.port)
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("CDP not ready on port %d after %v", s.port, timeout)
}

func (s *Session) getBrowserWSURL() (string, error) {
	url := fmt.Sprintf("http://127.0.0.1:%d/json/version", s.port)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "", fmt.Errorf("HTTP GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	var result struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode: %w", err)
	}
	if result.WebSocketDebuggerURL == "" {
		return "", fmt.Errorf("empty WebSocketDebuggerUrl")
	}
	return result.WebSocketDebuggerURL, nil
}

func (s *Session) clearProfileLocks(profileDir string) {
	for _, f := range []string{"SingletonLock", "SingletonSocket", "SingletonCookie", "Lockfile"} {
		path := filepath.Join(profileDir, f)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			log.Printf("[session] clearProfileLocks: remove %s failed: %v", f, err)
		}
	}
}

func (s *Session) isPortListening(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 500*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func (s *Session) findProcessOnPort(port int) []int {
	out, err := exec.Command("netstat", "-ano").Output()
	if err != nil {
		return nil
	}
	seen := map[int]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, fmt.Sprintf(":%d", port)) && strings.Contains(line, "LISTENING") {
			cols := strings.Fields(line)
			if len(cols) > 0 {
				pid := 0
				fmt.Sscanf(cols[len(cols)-1], "%d", &pid)
				if pid > 0 && !seen[pid] {
					seen[pid] = true
				}
			}
		}
	}
	var pids []int
	for pid := range seen {
		pids = append(pids, pid)
	}
	return pids
}

func (s *Session) findChromeExecutable() string {
	locations := []string{
		`C:\Program Files\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
		os.Getenv("LOCALAPPDATA") + `\Google\Chrome\Application\chrome.exe`,
	}
	for _, loc := range locations {
		if _, err := os.Stat(loc); err == nil {
			return loc
		}
	}
	return "chrome"
}

func (s *Session) resolveProfileDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "ds2api-browser-profile"), nil
}

func (s *Session) Login(ctx context.Context, email, password string) error {
	if s.browserCtx == nil {
		if err := s.initContexts(); err != nil {
			return fmt.Errorf("init contexts: %w", err)
		}
	}

	account := s.findAccount(email)
	if account == nil {
		return fmt.Errorf("account %s not found in config", email)
	}
	if password == "" {
		password = account.Password
	}

	if err := s.checkAndLogin(s.Context(), account.Email, password); err != nil {
		return fmt.Errorf("check/login: %w", err)
	}

	s.loggedIn.Store(true)
	s.currentEmail = account.Email
	// 找到当前账号的索引
	for i, a := range s.cfg.Accounts {
		if a.Email == account.Email {
			s.currentAccountIdx = i
			break
		}
	}

	// 登录成功后确保窗口大小正确（覆盖 Chrome 记忆状态）
	s.setWindowSize(900, 600)

	// 检查并确保深度思考和智能搜索处于开启状态
	s.checkToggleStates()

	return nil
}

func (s *Session) checkAndLogin(ctx context.Context, email, password string) error {
	if err := s.ensureOnDeepSeek(ctx); err != nil {
		return err
	}

	var hasTextarea bool
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(`document.querySelector("textarea") !== null`, &hasTextarea),
	); err != nil {
		log.Printf("[session] check textarea error: %v, assuming not logged in", err)
	}

	if hasTextarea {
		log.Println("[session] already logged in")
		return nil
	}

	log.Println("[session] not logged in, navigating to sign_in...")
	if err := chromedp.Run(ctx,
		chromedp.Navigate("https://chat.deepseek.com/sign_in"),
	); err != nil {
		return fmt.Errorf("navigate sign_in: %w", err)
	}
	time.Sleep(5 * time.Second)

	if err := s.doLogin(ctx, email, password); err != nil {
		return err
	}

	time.Sleep(5 * time.Second)

	var hasInput bool
	_ = chromedp.Run(ctx,
		chromedp.Evaluate(`document.querySelector("textarea") !== null`, &hasInput),
	)
	if !hasInput {
		return fmt.Errorf("login did not result in chat page")
	}

	log.Println("[session] login successful")
	return nil
}

func (s *Session) ensureOnDeepSeek(ctx context.Context) error {
	var url string
	if err := chromedp.Run(ctx, chromedp.Evaluate(`window.location.href`, &url)); err != nil {
		log.Printf("[session] get URL error: %v, will navigate anyway", err)
	}
	if strings.Contains(url, "chat.deepseek.com") {
		return nil
	}
	log.Printf("[session] not on DeepSeek (url=%s), navigating...", url)
	return chromedp.Run(ctx,
		chromedp.Navigate("https://chat.deepseek.com/"),
	)
}

// loginInterceptorJS 拦截 /api/v0/users/login 的响应，保存到 window.__dsLoginResult
// 用于区分登录失败类型：密码错误/账号封禁/网络波动
// 注意：__dsLoginResult = null 必须在 IIFE 外部，每次注入都重置旧结果
// 避免连续登录失败时第二次读到第一次的旧结果
const loginInterceptorJS = `
window.__dsLoginResult = null;
(function() {
	if (window.__dsLoginInterceptorInstalled) return;
	window.__dsLoginInterceptorInstalled = true;
	const origFetch = window.fetch;
	window.fetch = function(input, init) {
		const url = typeof input === 'string' ? input : (input && input.url) || '';
		const p = origFetch.apply(this, arguments);
		if (url.indexOf('/api/v0/users/login') !== -1) {
			p.then(function(resp) {
				const httpStatus = resp.status;
				resp.clone().json().then(function(data) {
					window.__dsLoginResult = {
						httpStatus: httpStatus,
						code: data.code,
						msg: data.msg || '',
						bizCode: (data.data && data.data.biz_code !== undefined) ? data.data.biz_code : -1,
						bizMsg: (data.data && data.data.biz_msg) ? data.data.biz_msg : '',
						networkError: false
					};
				}).catch(function(e) {
					window.__dsLoginResult = {
						httpStatus: httpStatus,
						networkError: true,
						errorMsg: 'json_parse_failed: ' + e.message
					};
				});
			}).catch(function(e) {
				window.__dsLoginResult = {
					networkError: true,
					errorMsg: 'fetch_failed: ' + e.message
				};
			});
		}
		return p;
	};
})();
`

// injectLoginInterceptor 在登录页注入 fetch 拦截器，捕获登录 API 响应
func (s *Session) injectLoginInterceptor(ctx context.Context) error {
	return chromedp.Run(ctx,
		chromedp.Evaluate(loginInterceptorJS, nil),
	)
}

// LoginResult 登录结果结构
type LoginResult struct {
	Success      bool   `json:"success"`
	FailType     string `json:"failType"` // password_error / banned / network_error / unknown
	BizMsg       string `json:"bizMsg"`
	HttpStatus   int    `json:"httpStatus"`
	NetworkError bool   `json:"networkError"`
}

// getLoginResult 读取拦截器捕获的登录结果，并判断失败类型
func (s *Session) getLoginResult(ctx context.Context) *LoginResult {
	var raw struct {
		HttpStatus   int    `json:"httpStatus"`
		Code         int    `json:"code"`
		Msg          string `json:"msg"`
		BizCode      int    `json:"bizCode"`
		BizMsg       string `json:"bizMsg"`
		NetworkError bool   `json:"networkError"`
		ErrorMsg     string `json:"errorMsg"`
	}
	_ = chromedp.Run(ctx,
		chromedp.Evaluate(`window.__dsLoginResult || null`, &raw),
	)

	result := &LoginResult{
		HttpStatus:   raw.HttpStatus,
		BizMsg:       raw.BizMsg,
		NetworkError: raw.NetworkError,
	}

	if raw.NetworkError {
		// 无 API 响应 = 网络波动
		result.FailType = "network_error"
		return result
	}

	// biz_code=0 表示登录成功
	if raw.BizCode == 0 {
		result.Success = true
		return result
	}

	// 根据 biz_msg 判断失败类型
	switch {
	case raw.BizMsg == "PASSWORD_OR_USER_NAME_IS_WRONG":
		result.FailType = "password_error"
	case strings.Contains(strings.ToUpper(raw.BizMsg), "BAN"),
		strings.Contains(strings.ToUpper(raw.BizMsg), "DISABLE"),
		strings.Contains(strings.ToUpper(raw.BizMsg), "FORBIDDEN"),
		strings.Contains(raw.BizMsg, "封禁"),
		strings.Contains(raw.BizMsg, "禁用"):
		result.FailType = "banned"
	default:
		result.FailType = "unknown"
	}
	return result
}

func (s *Session) doLogin(ctx context.Context, email, password string) error {
	// 等待页面加载完成（等待 input 元素出现）
	var inputCount int
	for retry := 0; retry < 10; retry++ {
		_ = chromedp.Run(ctx,
			chromedp.Evaluate(`document.querySelectorAll('input').length`, &inputCount),
		)
		if inputCount > 0 {
			break
		}
		log.Printf("[session] waiting for login page to load... (retry %d)", retry+1)
		time.Sleep(1 * time.Second)
	}
	log.Printf("[session] login page loaded, input count=%d", inputCount)

	// [账号质量管理] 注入 fetch 拦截器，捕获 /api/v0/users/login 响应
	// 用于区分登录失败类型：密码错误/账号封禁/网络波动
	if err := s.injectLoginInterceptor(ctx); err != nil {
		log.Printf("[session] inject login interceptor warning: %v", err)
	}

	// 等待按钮出现（DeepSeek 登录页按钮由 React 渲染，可能需要额外时间）
	// 先检查"密码登录"文字是否出现，同时检查密码输入框是否已可见
	var debug string
	for retry := 0; retry < 10; retry++ {
		_ = chromedp.Run(ctx, chromedp.Evaluate(`(()=>{
			// 检查密码输入框是否已经可见（说明已在密码登录表单）
			const inputs = document.querySelectorAll('input');
			for (const inp of inputs) {
				if (inp.type === 'password' && inp.offsetParent !== null) {
					return 'PASSWORD_FORM_VISIBLE';
				}
			}
			// 搜索所有元素中是否包含"密码登录"文字
			const body = document.body;
			if (!body) return 'NO_BODY';
			const text = body.textContent || '';
			if (text.includes('密码登录')) return 'FOUND';
			// 也检查按钮
			var btns = document.querySelectorAll('button');
			if (btns.length > 0) {
				var texts = [];
				for (var i = 0; i < btns.length; i++) {
					texts.push('#' + i + ':' + (btns[i].textContent||'').trim().substring(0,20));
				}
				return texts.join(' | ');
			}
			return 'NO';
		})()`, &debug))
		if debug == "PASSWORD_FORM_VISIBLE" || debug != "NO" && debug != "NO_BODY" {
			break
		}
		log.Printf("[session] waiting for login form to render... (retry %d, result=%s)", retry+1, debug)
		time.Sleep(2 * time.Second)
	}
	log.Printf("[session] login form check: %s", debug)

	// 调试：如果按钮仍然没出现，打印页面 body 内容
	if debug == "NO" {
		var bodyHTML string
		_ = chromedp.Run(ctx,
			chromedp.Evaluate(`document.body ? document.body.innerHTML.substring(0, 500) : 'no body'`, &bodyHTML),
		)
		log.Printf("[session] page body (first 500 chars): %s", bodyHTML)
	}

	// 检查是否已经在密码登录表单（有邮箱/密码输入框直接可见）
	var hasPasswordForm bool
	_ = chromedp.Run(ctx, chromedp.Evaluate(`(()=>{
		const inputs = document.querySelectorAll('input');
		for (const inp of inputs) {
			if (inp.type === 'password' && inp.offsetParent !== null) {
				return true;
			}
		}
		return false;
	})()`, &hasPasswordForm))

	if !hasPasswordForm && (strings.Contains(debug, "密码登录") || strings.Contains(debug, "FOUND")) {
		log.Println("[session] switching to password login via JS click...")
		// 使用 JS 查找真正可点击的"密码登录"按钮（div role="button"），
		// chromedp.Click 会卡死因为找到的是隐藏测量元素
		var clicked bool
		_ = chromedp.Run(ctx, chromedp.Evaluate(`(()=>{
			const all = document.querySelectorAll('*');
			for (const el of all) {
				const t = (el.textContent || '').trim();
				if (t === '密码登录') {
					const p = el.parentElement;
					if (p && p.getAttribute('role') === 'button') {
						const r = p.getBoundingClientRect();
						const cx = r.x + r.width/2;
						const cy = r.y + r.height/2;
						p.dispatchEvent(new PointerEvent('pointerdown', {bubbles: true, cancelable: true, clientX: cx, clientY: cy}));
						p.dispatchEvent(new PointerEvent('pointerup', {bubbles: true, cancelable: true, clientX: cx, clientY: cy}));
						p.dispatchEvent(new MouseEvent('click', {bubbles: true, cancelable: true, clientX: cx, clientY: cy}));
						p.click();
						return true;
					}
				}
			}
			return false;
		})()`, &clicked))

		if !clicked {
			// 回退：用 MouseClickXY 找到可点击的按钮
			log.Println("[session] JS click failed, trying MouseClickXY...")
			var btnPos string
			_ = chromedp.Run(ctx, chromedp.Evaluate(`(()=>{
				const all = document.querySelectorAll('*');
				for (const el of all) {
					const t = (el.textContent || '').trim();
					if (t === '密码登录') {
						const p = el.parentElement;
						if (p && p.getAttribute('role') === 'button') {
							const r = p.getBoundingClientRect();
							return JSON.stringify({found: true, x: Math.round(r.x + r.width/2), y: Math.round(r.y + r.height/2)});
						}
					}
				}
				return JSON.stringify({found: false});
			})()`, &btnPos))
			if strings.Contains(btnPos, `"found":true`) {
				var pos struct {
					X int `json:"x"`
					Y int `json:"y"`
				}
				json.Unmarshal([]byte(btnPos), &pos)
				log.Printf("[session] clicking '密码登录' via MouseClickXY at (%d,%d)", pos.X, pos.Y)
				chromedp.Run(ctx, chromedp.MouseClickXY(float64(pos.X), float64(pos.Y)))
			}
		}
		log.Println("[session] '密码登录' clicked")
		time.Sleep(2 * time.Second)

		// 等待密码登录表单出现（邮箱输入框）
		var emailInputFound bool
		for retry := 0; retry < 5; retry++ {
			_ = chromedp.Run(ctx, chromedp.Evaluate(`(()=>{
				const inputs = document.querySelectorAll('input');
				for (const inp of inputs) {
					const t = inp.type || '';
					const p = (inp.placeholder || '').toLowerCase();
					if (t === 'email' || p.includes('邮箱') || p.includes('email') || p.includes('mail')) {
						return true;
					}
				}
				// 也检查是否有 text 类型的 input（密码登录后会出现）
				for (const inp of inputs) {
					if (inp.type === 'text' || inp.type === 'password') {
						return true;
					}
				}
				return false;
			})()`, &emailInputFound))
			if emailInputFound {
				break
			}
			log.Printf("[session] waiting for email/password inputs... (retry %d)", retry+1)
			time.Sleep(1 * time.Second)
		}
		log.Printf("[session] email/password inputs found: %v", emailInputFound)
	} else if hasPasswordForm {
		log.Println("[session] password login form already visible, skipping '密码登录' click")
	} else {
		log.Printf("[session] '密码登录' not found and password form not visible")
	}

	// 如果页面没有按钮也没有输入框，可能是页面加载失败
	if inputCount == 0 && debug == "NO" {
		// 尝试刷新页面
		log.Println("[session] login page seems empty, refreshing...")
		_ = chromedp.Run(ctx, chromedp.Reload())
		time.Sleep(3 * time.Second)
		_ = chromedp.Run(ctx,
			chromedp.Evaluate(`document.querySelectorAll('input').length`, &inputCount),
		)
		log.Printf("[session] after refresh, input count=%d", inputCount)
	}

	// 步骤0: 先清除所有输入框中的旧值（防止浏览器自动填充残留）
	chromedp.Run(ctx, chromedp.Evaluate(`(()=>{
		var s = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value').set;
		var inputs = document.querySelectorAll('input');
		for (var i = 0; i < inputs.length; i++) {
			inputs[i].focus();
			inputs[i].select();
			s.call(inputs[i], '');
			inputs[i].dispatchEvent(new Event('input', {bubbles: true}));
			inputs[i].dispatchEvent(new Event('change', {bubbles: true}));
		}
	})()`, nil))
	log.Println("[session] cleared old input values")
	time.Sleep(300 * time.Millisecond)

	// 使用 value setter 直接设置值（与 chat.go typeText 方式一致）
	// DeepSeek 的 React 表单需要通过 prototype setter 才能触发状态更新
	// 通过 placeholder 文字精确匹配输入框，防止填入错误位置
	fillEmail := `(()=>{
		var inputs = document.querySelectorAll('input');
		var setter = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value').set;
		for (var i = 0; i < inputs.length; i++) {
			var t = inputs[i].type || '';
			var p = (inputs[i].placeholder || '').toLowerCase();
			var autocomplete = (inputs[i].getAttribute('autocomplete') || '').toLowerCase();
			if (t === 'email' || p.includes('手机号') || p.includes('邮箱') || p.includes('email') || p.includes('mail') ||
				autocomplete.includes('email') || autocomplete.includes('username')) {
				inputs[i].focus();
				setter.call(inputs[i], window.__dsFillValue);
				inputs[i].dispatchEvent(new Event('input', {bubbles: true}));
				inputs[i].dispatchEvent(new Event('change', {bubbles: true}));
				return 'ok:' + (inputs[i].placeholder || '').substring(0, 30);
			}
		}
		return 'not_found';
	})()`
	encodedEmail, _ := json.Marshal(email)
	var emailResult string
	chromedp.Run(ctx,
		chromedp.Evaluate(fmt.Sprintf(`window.__dsFillValue=%s`, string(encodedEmail)), nil),
		chromedp.Evaluate(fillEmail, &emailResult),
		chromedp.Evaluate(`delete window.__dsFillValue`, nil),
	)
	log.Printf("[session] email fill result: %s", emailResult)
	time.Sleep(200 * time.Millisecond)

	fillPass := `(()=>{
		var inputs = document.querySelectorAll('input');
		var setter = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value').set;
		for (var i = 0; i < inputs.length; i++) {
			var p = (inputs[i].placeholder || '').toLowerCase();
			if (inputs[i].type === 'password' || p.includes('密码')) {
				inputs[i].focus();
				setter.call(inputs[i], window.__dsFillValue);
				inputs[i].dispatchEvent(new Event('input', {bubbles: true}));
				inputs[i].dispatchEvent(new Event('change', {bubbles: true}));
				return 'ok:' + (inputs[i].placeholder || '').substring(0, 30);
			}
		}
		return 'not_found';
	})()`
	encodedPass, _ := json.Marshal(password)
	var passResult string
	chromedp.Run(ctx,
		chromedp.Evaluate(fmt.Sprintf(`window.__dsFillValue=%s`, string(encodedPass)), nil),
		chromedp.Evaluate(fillPass, &passResult),
		chromedp.Evaluate(`delete window.__dsFillValue`, nil),
	)
	log.Printf("[session] password fill result: %s", passResult)

	time.Sleep(200 * time.Millisecond)

	// 尝试用 JS click 和真实鼠标点击两种方式点击登录按钮
	var clicked bool
	_ = chromedp.Run(ctx, chromedp.Evaluate(`(()=>{
		// 查找 button 标签和 [role="button"] 元素
		const btns = document.querySelectorAll('button, [role="button"]');
		for (let i = 0; i < btns.length; i++) {
			const t = (btns[i].textContent || '').trim();
			if (t === '登录' || t.includes('登录')) {
				btns[i].click();
				btns[i].dispatchEvent(new MouseEvent('mousedown', {bubbles: true, cancelable: true}));
				btns[i].dispatchEvent(new MouseEvent('mouseup', {bubbles: true, cancelable: true}));
				btns[i].dispatchEvent(new MouseEvent('click', {bubbles: true, cancelable: true}));
				return true;
			}
		}
		return false;
	})()`, &clicked))

	if !clicked {
		// 如果 JS click 没找到，尝试用真实鼠标点击
		var btnPos string
		_ = chromedp.Run(ctx, chromedp.Evaluate(`(()=>{
			// 先尝试 button 标签和 [role="button"]
			const btns = document.querySelectorAll('button, [role="button"]');
			for (const b of btns) {
				const t = (b.textContent || '').trim();
				if (t === '登录' || t.includes('登录')) {
					const r = b.getBoundingClientRect();
					return JSON.stringify({found: true, x: Math.round(r.x + r.width/2), y: Math.round(r.y + r.height/2)});
				}
			}
			// 再尝试任意元素
			const all = document.querySelectorAll('*');
			for (const el of all) {
				const t = (el.textContent || '').trim();
				if (t === '登录') {
					const r = el.getBoundingClientRect();
					if (r.width > 0 && r.height > 0 && r.width < 500) {
						return JSON.stringify({found: true, x: Math.round(r.x + r.width/2), y: Math.round(r.y + r.height/2)});
					}
				}
			}
			return JSON.stringify({found: false});
		})()`, &btnPos))
		if strings.Contains(btnPos, `"found":true`) {
			var pos struct {
				X int `json:"x"`
				Y int `json:"y"`
			}
			json.Unmarshal([]byte(btnPos), &pos)
			log.Printf("[session] clicking login button via MouseClickXY at (%d,%d)", pos.X, pos.Y)
			chromedp.Run(ctx, chromedp.MouseClickXY(float64(pos.X), float64(pos.Y)))
		} else {
			log.Printf("[session] WARNING: login button not found by any method")
		}
	}

	log.Println("[session] login form submitted")
	return nil
}

func (s *Session) findAccount(email string) *config.Account {
	for i := range s.cfg.Accounts {
		if s.cfg.Accounts[i].Email == email {
			return &s.cfg.Accounts[i]
		}
	}
	return nil
}

// ShouldRotate 检查当前账号是否处理了足够多的请求，应该轮换
// rotationInterval=0 表示不轮换
func (s *Session) ShouldRotate() bool {
	if s.cfg.RotationInterval <= 0 {
		return false
	}
	return s.sessionRequestCount >= s.cfg.RotationInterval
}

// SwitchAccount 切换到下一个账号并重新登录
// 返回切换后的账号邮箱，如果可用账号少于2个则返回错误
// 注意：登录失败次数 > 0 的账号已在 reorderAccounts 时从 sortedIndices 中排除
func (s *Session) SwitchAccount() (string, error) {
	if len(s.sortedIndices) <= 1 {
		log.Printf("[session] cannot switch: only %d available account(s) (config has %d, but some may be disabled due to login failures)",
			len(s.sortedIndices), len(s.cfg.Accounts))
		return "", fmt.Errorf("no alternative account available (disabled: %d, available: %d)",
			len(s.cfg.Accounts)-len(s.sortedIndices), len(s.sortedIndices))
	}

	// 根据质量排序列表计算下一个账号索引
	s.currentSortedIdx = (s.currentSortedIdx + 1) % len(s.sortedIndices)
	nextIdx := s.sortedIndices[s.currentSortedIdx]
	nextAccount := s.cfg.Accounts[nextIdx]

	log.Printf("[session] switching account based on quality: from %s (sortedIdx=%d) to %s (sortedIdx=%d, configIdx=%d)",
		s.currentEmail, (s.currentSortedIdx+len(s.sortedIndices)-1)%len(s.sortedIndices), nextAccount.Email, s.currentSortedIdx, nextIdx)

	// 记录当前账号的会话统计
	if s.currentEmail != "" {
		s.stats.RecordSessionEnd(s.currentEmail, s.sessionRequestCount)
		s.sessionRequestCount = 0
	}

	// 先登出当前账号（清除 cookies + 导航到登录页）
	if err := s.logout(); err != nil {
		log.Printf("[session] logout warning: %v", err)
	}

	// logout 内部已经导航到 sign_in 页面，这里直接执行登录
	// 不需要再次导航

	// 验证页面确实是登录页（没有 textarea）
	var hasTextarea bool
	_ = chromedp.Run(s.Context(),
		chromedp.Evaluate(`document.querySelector("textarea") !== null`, &hasTextarea),
	)
	if hasTextarea {
		log.Printf("[session] WARNING: textarea still exists after logout, re-navigating to sign_in")
		_ = chromedp.Run(s.Context(),
			chromedp.Navigate("https://chat.deepseek.com/sign_in"),
		)
		time.Sleep(2 * time.Second)
	}

	// 在登录页直接执行登录
	if err := s.doLogin(s.Context(), nextAccount.Email, nextAccount.Password); err != nil {
		// [账号质量管理] 根据登录失败类型决定是否禁用账号
		loginResult := s.getLoginResult(s.Context())
		log.Printf("[session] login to %s failed: %v, failType=%s, bizMsg=%s, httpStatus=%d, networkError=%v",
			nextAccount.Email, err, loginResult.FailType, loginResult.BizMsg, loginResult.HttpStatus, loginResult.NetworkError)
		s.handleLoginFailure(nextAccount.Email, loginResult)
		return "", fmt.Errorf("login to %s failed: %w", nextAccount.Email, err)
	}

	// 等待登录完成，验证 textarea 出现
	time.Sleep(5 * time.Second)
	_ = chromedp.Run(s.Context(),
		chromedp.Evaluate(`document.querySelector("textarea") !== null`, &hasTextarea),
	)
	if !hasTextarea {
		// 再等 5 秒
		time.Sleep(5 * time.Second)
		_ = chromedp.Run(s.Context(),
			chromedp.Evaluate(`document.querySelector("textarea") !== null`, &hasTextarea),
		)
		if !hasTextarea {
			// [账号质量管理] textarea 未出现可能是登录失败或页面加载慢，根据拦截器结果判断
			loginResult := s.getLoginResult(s.Context())
			log.Printf("[session] login to %s did not result in chat page, failType=%s, bizMsg=%s",
				nextAccount.Email, loginResult.FailType, loginResult.BizMsg)
			s.handleLoginFailure(nextAccount.Email, loginResult)
			return "", fmt.Errorf("login to %s did not result in chat page", nextAccount.Email)
		}
	}

	// [账号质量管理] 登录成功后清零登录失败计数（清除之前的网络波动记录）
	s.stats.RecordLoginSuccess(nextAccount.Email)

	s.currentAccountIdx = nextIdx
	s.currentEmail = nextAccount.Email
	s.loggedIn.Store(true)

	log.Printf("[session] successfully switched to account %s", nextAccount.Email)

	// 检查深度思考和智能搜索开关状态
	s.checkToggleStates()

	return nextAccount.Email, nil
}

// handleLoginFailure 根据登录失败类型决定是否禁用账号
// 密码错误/账号封禁 → 禁用账号并重新排序 sortedIndices
// 网络波动/未知原因 → 不禁用，可重试
// 禁用后需修正 currentSortedIdx，避免越界并指向当前账号在新列表中的位置
func (s *Session) handleLoginFailure(email string, result *LoginResult) {
	switch result.FailType {
	case "password_error":
		s.stats.RecordLoginFailure(email)
		log.Printf("[session] ⚠️ 账号 %s 密码错误，已禁用", email)
		s.reorderAccounts()
		s.fixCurrentSortedIdx()
	case "banned":
		s.stats.RecordLoginFailure(email)
		log.Printf("[session] ⚠️ 账号 %s 已被封禁，已禁用", email)
		s.reorderAccounts()
		s.fixCurrentSortedIdx()
	case "network_error":
		log.Printf("[session] 账号 %s 登录失败（网络波动），不禁用", email)
	default:
		log.Printf("[session] ⚠️ 账号 %s 登录失败（未知原因: %s），不禁用", email, result.BizMsg)
	}
}

// fixCurrentSortedIdx 修正 currentSortedIdx，使其指向 currentEmail 在新 sortedIndices 中的位置
// 在账号被禁用导致 sortedIndices 变化后调用，避免越界
func (s *Session) fixCurrentSortedIdx() {
	if len(s.sortedIndices) == 0 {
		s.currentSortedIdx = 0
		return
	}
	// 查找当前账号在新列表中的位置
	for i, idx := range s.sortedIndices {
		if s.cfg.Accounts[idx].Email == s.currentEmail {
			s.currentSortedIdx = i
			return
		}
	}
	// 当前账号不在列表中（可能也被禁用），重置为0
	s.currentSortedIdx = 0
}

// reDetectToggleJS 重新检测"深度思考"toggle 的点击坐标。
// 用于点击失败后重新定位——页面从"欢迎页"切到"对话中"时 toggle 会从 y≈320 跳到 y≈447，
// 必须每次点击前重新取坐标，否则点到空位置。
const reDetectToggleJS = `(()=>{
	var info = {thinkingPos: null};
	var toggles = document.querySelectorAll('div.ds-toggle-button, [aria-pressed]');
	for (var i = 0; i < toggles.length; i++) {
		var el = toggles[i];
		var text = (el.textContent || '').trim();
		if (text.includes('深度思考') && !info.thinkingPos) {
			var r = el.getBoundingClientRect();
			if (r.width > 0 && r.height > 0) {
				info.thinkingPos = {x: Math.round(r.x + r.width/2), y: Math.round(r.y + r.height/2)};
			}
		}
	}
	return JSON.stringify(info);
})()`

// clickThinkingToggleJS 直接调用 React 的 onClick 切换"深度思考"toggle。
// 实测（08-13 15:45）切账号后真实鼠标点击（MouseClickXY）无法触发 React 切换
// （React 17+ 事件委托在页面刚重建时未正确分发真实鼠标事件），但直接调用
// toggle 元素 __reactProps 上的 onClick 100% 生效、可稳定翻转（true↔false）。
// 传一个最小 event 对象（含 preventDefault/stopPropagation/target）即可满足 onClick。
const clickThinkingToggleJS = `(()=>{
	const toggles = document.querySelectorAll('div.ds-toggle-button, [aria-pressed]');
	for (const el of toggles) {
		if ((el.textContent||'').trim().includes('深度思考')) {
			const rk = Object.keys(el).filter(k => k.startsWith('__reactProps'));
			if (rk.length === 0) return 'no_react';
			const props = el[rk[0]];
			if (!(props && props.onClick)) return 'no_onclick';
			props.onClick({ preventDefault(){}, stopPropagation(){}, target: el });
			return 'clicked';
		}
	}
	return 'not_found';
})()`

// checkToggleStates 检查并确保"深度思考"和"智能搜索"处于开启状态（使用 s.Context()，供非持锁场景调用）。
func (s *Session) checkToggleStates() {
	s.checkToggleStatesLocked(s.Context())
}

// checkToggleStatesLocked 检查并确保"深度思考"和"智能搜索"处于开启状态（接受外部 ctx）。
// 登录后 DeepSeek 默认关闭深度思考，需要自动点击开启
// DOM 特征：div.ds-toggle-button，aria-pressed="true" 表示开启，class 含 --selected 表示开启
// 注意：DeepSeek 的 React 按钮对原生 el.click() 无效，且真实鼠标点击（MouseClickXY）
// 在页面重建后 React 事件委托未分发时也无效，必须直接调用 __reactProps.onClick（见 clickThinkingToggleJS）
// 注意：本函数直接使用传入的 ctx，不再调用 s.Context()——供已持有 s.ctxMu 锁的场景（如 restartBrowserLocked）使用，避免重复加锁死锁。
func (s *Session) checkToggleStatesLocked(ctx context.Context) {
	// [Fix 2026-08-13] 前置等待页面真正就绪。两个条件缺一不可：
	// 1. JS 引擎事件循环已恢复（应对 Memory Saver 冻结后唤醒：DOM 未销毁、
	//    __reactProps 一直在，但事件循环冻结，写入 window 随机值读回比对一致才算恢复）
	// 2. React 已挂载"深度思考"toggle 元素（应对切账号/新对话后全新登录：
	//    window 可写≠React 组件树已挂载，必须等到 toggle 元素上有 __reactProps
	//    且 props.onClick 存在，说明 React 事件委托已建立，MouseClickXY 点击才生效）
	// 实测（08-13 15:24）：切账号后 toggle 检测到关、React 未挂载时 3 次点击全失败；
	// React 挂载后（hasClick=true）一次点击即点亮。最多等 15 秒。
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		probe := fmt.Sprintf("dsProbe%d", time.Now().UnixNano())
		var ready string
		err := chromedp.Run(ctx,
			chromedp.Evaluate(fmt.Sprintf(`window.__dsWakeProbe = %q;`, probe), nil),
			chromedp.Evaluate(fmt.Sprintf(`(()=>{
				const ta = document.querySelector('textarea');
				if (!ta) return 'no_ta';
				if (document.readyState !== 'complete') return 'loading';
				if (window.__dsWakeProbe !== %q) return 'wake_mismatch';
				const toggles = document.querySelectorAll('div.ds-toggle-button, [aria-pressed]');
				for (const el of toggles) {
					if ((el.textContent||'').trim().includes('深度思考')) {
						const rk = Object.keys(el).filter(k => k.startsWith('__reactProps'));
						if (rk.length === 0) return 'no_react';
						const props = el[rk[0]];
						if (!(props && props.onClick)) return 'no_onclick';
						return 'ready';
					}
				}
				return 'no_toggle';
			})()`, probe), &ready),
		)
		if err == nil && ready == "ready" {
			break // JS 引擎恢复 + React 已挂载 toggle，点击才有效
		}
		time.Sleep(300 * time.Millisecond)
	}

	// 第一步：用 JS 检测状态和获取按钮坐标
	var detectResult string
	if err := chromedp.Run(ctx, chromedp.Evaluate(`(()=>{
		var info = {thinkingFound: false, thinkingOn: false, searchFound: false, searchOn: false, thinkingPos: null};

		var toggles = document.querySelectorAll('div.ds-toggle-button, [aria-pressed]');
		for (var i = 0; i < toggles.length; i++) {
			var el = toggles[i];
			var text = (el.textContent || '').trim();
			var isOn = el.getAttribute('aria-pressed') === 'true' ||
				el.className.includes('ds-toggle-button--selected');

			if (text.includes('深度思考') && !info.thinkingFound) {
				info.thinkingFound = true;
				info.thinkingOn = isOn;
				var r = el.getBoundingClientRect();
				info.thinkingDetail = {
					ariaPressed: el.getAttribute('aria-pressed'),
					hasSelectedClass: el.className.includes('ds-toggle-button--selected'),
				};
				if (r.width > 0 && r.height > 0) {
					info.thinkingPos = {x: Math.round(r.x + r.width/2), y: Math.round(r.y + r.height/2)};
				}
			}

			if (text.includes('智能搜索') && !info.searchFound) {
				info.searchFound = true;
				info.searchOn = isOn;
				info.searchDetail = {
					ariaPressed: el.getAttribute('aria-pressed'),
					hasSelectedClass: el.className.includes('ds-toggle-button--selected'),
				};
			}
		}
		return JSON.stringify(info);
	})()`, &detectResult)); err != nil {
		log.Printf("[session] checkToggleStates detect error: %v", err)
		return
	}
	log.Printf("[session] toggle states: %s", detectResult)

	// 第二步：如果深度思考未开启，用 React onClick 直接调用切换，循环尝试直到点亮或超时。
	// [Fix 2026-08-13] 切账号/新对话后真实鼠标点击（MouseClickXY）无法触发 React 切换
	// （React 17+ 事件委托未正确分发真实鼠标事件），改为直接调用 toggle 元素的
	// __reactProps.onClick，实测 100% 生效。循环最长 20 秒，每次失败后等待 1.5 秒
	// 让 React 完成 hydrate 再重试。
	if strings.Contains(detectResult, `"thinkingOn":false`) && strings.Contains(detectResult, `"thinkingPos":`) {
		clickDeadline := time.Now().Add(20 * time.Second)
		attempt := 0
		for time.Now().Before(clickDeadline) {
			attempt++
			var pos struct {
				ThinkingPos *struct {
					X int `json:"x"`
					Y int `json:"y"`
				} `json:"thinkingPos"`
			}
			json.Unmarshal([]byte(detectResult), &pos)
			if pos.ThinkingPos == nil {
				// 无坐标，重新检测后继续
				if err := chromedp.Run(ctx, chromedp.Evaluate(reDetectToggleJS, &detectResult)); err != nil {
					log.Printf("[session] checkToggleStates re-detect error: %v", err)
					return
				}
				time.Sleep(500 * time.Millisecond)
				continue
			}
			log.Printf("[session] clicking 深度思考 toggle at (%d,%d) (attempt %d)", pos.ThinkingPos.X, pos.ThinkingPos.Y, attempt)
			var clickResult string
			if err := chromedp.Run(ctx, chromedp.Evaluate(clickThinkingToggleJS, &clickResult)); err != nil {
				log.Printf("[session] checkToggleStates click error: %v", err)
				return
			}
			log.Printf("[session] click result: %s", clickResult)
			time.Sleep(800 * time.Millisecond)

			// 验证点击后是否已开启
			var verify string
			if err := chromedp.Run(ctx, chromedp.Evaluate(`(()=>{
			var toggles = document.querySelectorAll('div.ds-toggle-button, [aria-pressed]');
				for (var i = 0; i < toggles.length; i++) {
					if ((toggles[i].textContent||'').trim().includes('深度思考')) {
						return JSON.stringify({on: toggles[i].getAttribute('aria-pressed') === 'true' || toggles[i].className.includes('--selected')});
					}
				}
				return JSON.stringify({on: false, error: 'not_found'});
			})()`, &verify)); err != nil {
				log.Printf("[session] checkToggleStates verify error: %v", err)
				return
			}
			log.Printf("[session] 深度思考 after click: %s", verify)
			if strings.Contains(verify, `"on":true`) {
				log.Printf("[session] 深度思考 toggle enabled (attempt %d)", attempt)
				return
			}
			// 未开启：等待 React 事件委托就绪后重新检测坐标再试
			time.Sleep(1500 * time.Millisecond)
			if err := chromedp.Run(ctx, chromedp.Evaluate(reDetectToggleJS, &detectResult)); err != nil {
				log.Printf("[session] checkToggleStates re-detect error: %v", err)
				return
			}
		}
		log.Printf("[session] ⚠️ 深度思考 toggle failed to enable after %d attempts", attempt)
	}
}

// logout 登出当前账号
// 使用真实鼠标点击：左下角头像 → 弹出菜单 → 点击"退出登录"
func (s *Session) logout() error {
	log.Println("[session] logging out via mouse clicks...")

	// 步骤 1: 找到左下角用户头像并点击（真实鼠标点击）
	avatarClicked := s.clickAvatar()
	if !avatarClicked {
		log.Printf("[session] avatar click failed, falling back to direct navigation")
	}

	// 步骤 2: 等待弹出菜单，找到并点击"退出登录"（真实鼠标点击）
	if avatarClicked {
		logoutClicked := s.clickLogoutButton()
		if logoutClicked {
			log.Println("[session] logout button clicked via mouse")
			time.Sleep(2 * time.Second)
		} else {
			log.Printf("[session] logout button not found in popup menu")
		}
	}

	// 步骤 3: 强制清除 cookies（使用 CDP 协议）
	err := chromedp.Run(s.Context(), chromedp.ActionFunc(func(ctx context.Context) error {
		if err := network.ClearBrowserCookies().Do(ctx); err != nil {
			return fmt.Errorf("clear cookies: %w", err)
		}
		return nil
	}))
	if err != nil {
		log.Printf("[session] clearBrowserCookies warning: %v", err)
	} else {
		log.Println("[session] cookies cleared via CDP")
	}

	// 步骤 4: 清除 localStorage / sessionStorage
	_ = chromedp.Run(s.Context(),
		chromedp.Evaluate(`(()=>{
			try { localStorage.clear(); } catch(e) {}
			try { sessionStorage.clear(); } catch(e) {}
			return 'storage_cleared';
		})()`, nil),
	)
	log.Println("[session] localStorage/sessionStorage cleared")

	// 步骤 5: 导航到 about:blank 然后再到登录页面（确保完全退出当前会话）
	if err := chromedp.Run(s.Context(),
		chromedp.Navigate("about:blank"),
	); err != nil {
		log.Printf("[session] navigate about:blank warning: %v", err)
	}
	time.Sleep(1 * time.Second)

	if err := chromedp.Run(s.Context(),
		chromedp.Navigate("https://chat.deepseek.com/sign_in"),
	); err != nil {
		return fmt.Errorf("navigate to sign_in: %w", err)
	}
	log.Println("[session] navigated to sign_in page")

	time.Sleep(5 * time.Second)
	s.loggedIn.Store(false)
	return nil
}

// clickAvatar 找到左下角用户头像并用真实鼠标点击
func (s *Session) clickAvatar() bool {
	// 先检查当前页面 URL，确保在 DeepSeek 聊天页面上
	var url string
	_ = chromedp.Run(s.Context(), chromedp.Evaluate(`window.location.href`, &url))
	log.Printf("[session] clickAvatar: current URL=%s", url)

	if !strings.Contains(url, "chat.deepseek.com") || strings.Contains(url, "sign_in") {
		log.Printf("[session] not on chat page, skipping avatar click")
		return false
	}

	var posJSON string
	err := chromedp.Run(s.Context(),
		chromedp.Evaluate(`(()=>{
			const vw = window.innerWidth;
			const vh = window.innerHeight;

			// 策略1: 查找左下角的小图片（通常是头像）
			const allImgs = document.querySelectorAll('img');
			for (const img of allImgs) {
				const r = img.getBoundingClientRect();
				if (r.width === 0 || r.height === 0) continue;
				if (r.width > 50 || r.height > 50) continue; // 头像通常很小

				const cx = r.x + r.width / 2;
				const cy = r.y + r.height / 2;

				// 左下角：左侧 50% 宽度，底部 200px 区域
				if (cx < vw * 0.5 && cy > vh - 200) {
					return JSON.stringify({
						found: true,
						x: Math.round(cx),
						y: Math.round(cy),
						tag: 'img',
						cls: (img.getAttribute('class') || '').substring(0, 60)
					});
				}
			}

			// 策略2: 查找左下角的按钮（含 avatar/user 类名）
			const elements = document.querySelectorAll('button, [role="button"], div[class], span[class]');
			for (const el of elements) {
				const r = el.getBoundingClientRect();
				if (r.width === 0 || r.height === 0) continue;
				if (r.width > 200 || r.height > 200) continue;

				const cx = r.x + r.width / 2;
				const cy = r.y + r.height / 2;
				const cls = (el.getAttribute('class') || '').toLowerCase();

				if (cx < vw * 0.5 && cy > vh - 200 &&
					(cls.includes('avatar') || cls.includes('user') || cls.includes('profile'))) {
					return JSON.stringify({
						found: true,
						x: Math.round(cx),
						y: Math.round(cy),
						tag: el.tagName,
						cls: cls.substring(0, 60)
					});
				}
			}

			// 策略3: 查找左下角任意可点击的小元素
			const clickables = document.querySelectorAll('button, [role="button"], div[tabindex], span[tabindex]');
			for (const el of clickables) {
				const r = el.getBoundingClientRect();
				if (r.width === 0 || r.height === 0) continue;
				if (r.width > 60 || r.height > 60) continue;

				const cx = r.x + r.width / 2;
				const cy = r.y + r.height / 2;

				if (cx < vw * 0.5 && cy > vh - 200) {
					return JSON.stringify({
						found: true,
						x: Math.round(cx),
						y: Math.round(cy),
						tag: el.tagName,
						cls: (el.getAttribute('class') || '').substring(0, 60)
					});
				}
			}

			return JSON.stringify({found: false});
		})()`, &posJSON),
	)

	if err != nil || !strings.Contains(posJSON, `"found":true`) {
		log.Printf("[session] avatar not found, err=%v, result=%s", err, posJSON)
		return false
	}

	var pos struct {
		X   int    `json:"x"`
		Y   int    `json:"y"`
		Tag string `json:"tag"`
		Cls string `json:"cls"`
	}
	json.Unmarshal([]byte(posJSON), &pos)

	log.Printf("[session] clicking avatar: tag=%s cls=%s pos=(%d,%d)", pos.Tag, pos.Cls, pos.X, pos.Y)
	if err := chromedp.Run(s.Context(),
		chromedp.MouseClickXY(float64(pos.X), float64(pos.Y)),
	); err != nil {
		log.Printf("[session] avatar MouseClickXY error: %v", err)
		return false
	}

	// 等待弹出菜单出现
	time.Sleep(1500 * time.Millisecond)
	return true
}

// clickLogoutButton 在弹出菜单中找到"退出登录"按钮并用真实鼠标点击
func (s *Session) clickLogoutButton() bool {
	var posJSON string
	err := chromedp.Run(s.Context(),
		chromedp.Evaluate(`(()=>{
			// 遍历可见的文本元素，找文本为"退出登录"的元素
			const allElements = document.querySelectorAll('button, [role="button"], [role="menuitem"], div[class], span[class], a[class]');
			for (const el of allElements) {
				// 只检查叶子节点或有直接文本内容的元素
				const text = (el.textContent || '').trim();
				if (text === '退出登录' || text === '退出' || text === '登出' ||
					text === 'Log out' || text === 'Sign out') {
					// 确保元素可见
					if (el.offsetParent !== null) {
						const r = el.getBoundingClientRect();
						if (r.width > 0 && r.height > 0 && r.width < 500) {
							return JSON.stringify({
								found: true,
								x: Math.round(r.x + r.width / 2),
								y: Math.round(r.y + r.height / 2),
								tag: el.tagName,
								text: text
							});
						}
					}
				}
			}
			return JSON.stringify({found: false});
		})()`, &posJSON),
	)

	if err != nil || !strings.Contains(posJSON, `"found":true`) {
		log.Printf("[session] logout button not found")
		return false
	}

	var pos struct {
		X    int    `json:"x"`
		Y    int    `json:"y"`
		Tag  string `json:"tag"`
		Text string `json:"text"`
	}
	json.Unmarshal([]byte(posJSON), &pos)

	log.Printf("[session] clicking logout button: tag=%s text=%s pos=(%d,%d)", pos.Tag, pos.Text, pos.X, pos.Y)
	if err := chromedp.Run(s.Context(),
		chromedp.MouseClickXY(float64(pos.X), float64(pos.Y)),
	); err != nil {
		log.Printf("[session] logout button MouseClickXY error: %v", err)
		return false
	}

	return true
}

// CurrentAccount 返回当前账号邮箱
func (s *Session) CurrentAccount() string {
	return s.currentEmail
}

// CurrentIndex 返回当前账号索引
func (s *Session) CurrentIndex() int {
	return s.currentAccountIdx
}

// AccountCount 返回配置的账号数量
func (s *Session) AccountCount() int {
	return len(s.cfg.Accounts)
}

// AvailableAccountCount 返回可用账号数量（排除登录失败被禁用的账号）
func (s *Session) AvailableAccountCount() int {
	return len(s.sortedIndices)
}

// NavigateHome 导航回 DeepSeek 首页。
// [Fix 2026-08-12] 页面全新加载会重置深度思考开关（DeepSeek 默认关闭），
// 必须等待页面就绪后重新检查并开启，否则后续请求回复将不带思考过程。
// 与 restart/rebuild 后 checkToggleStates 一致；连续对话（页面未重建）时
// 深度思考状态保留，不走此路径，不需要重复检查。
func (s *Session) NavigateHome(ctx context.Context) error {
	if err := chromedp.Run(s.Context(),
		chromedp.Navigate("https://chat.deepseek.com/"),
	); err != nil {
		return err
	}
	// 等待聊天页面就绪（textarea 出现 = 已登录 + 页面加载完成），
	// 就绪后再检查深度思考开关，避免检测时页面尚未渲染出 toggle。
	taDeadline := time.Now().Add(15 * time.Second)
	var taOK bool
	for time.Now().Before(taDeadline) {
		if err := chromedp.Run(s.Context(),
			chromedp.Evaluate(`!!document.querySelector('textarea')`, &taOK),
		); err == nil && taOK {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !taOK {
		log.Printf("[session] NavigateHome: chat page not ready in 15s, still checking toggles")
	}
	// 深度思考/智能搜索开关检查（内部带日志，失败不中断）
	s.checkToggleStates()
	return nil
}

func (s *Session) NewConversation(ctx context.Context) error {
	ctxT := s.Context()

	s.ensureOnDeepSeek(ctxT)

	var result string
	_ = chromedp.Run(ctxT,
		chromedp.Evaluate(`(()=>{
			const btns = document.querySelectorAll('button, [role="button"], div');
			const kw = ['新聊天', '新对话', 'new chat', 'new conversation'];
			for (const b of btns) {
				const t = (b.textContent || '').trim().toLowerCase();
				for (const k of kw) {
					if (t.includes(k)) { b.click(); return 'clicked:'+k; }
				}
			}
			return 'not_found';
		})()`, &result),
	)

	if strings.Contains(result, "clicked") {
		log.Printf("[session] new conversation via UI: %s", result)
		time.Sleep(300 * time.Millisecond)
		return nil
	}

	log.Println("[session] Ctrl+J for new conversation")
	chromedp.Run(ctxT, chromedp.KeyEvent("j", chromedp.KeyModifiers(2)))
	time.Sleep(300 * time.Millisecond)
	return nil
}

func (s *Session) Close() {
	if s.browserCancel != nil {
		s.browserCancel()
	}
	if s.allocCancel != nil {
		s.allocCancel()
	}
	if s.chromeCmd != nil && s.chromeCmd.Process != nil {
		s.chromeCmd.Process.Kill()
		log.Println("[session] Chrome process killed")
	}
}

// RunEval 在浏览器中执行 JavaScript 并返回结果
func RunEval(ctx context.Context, js string, result interface{}) error {
	return chromedp.Run(ctx, chromedp.Evaluate(js, result))
}
