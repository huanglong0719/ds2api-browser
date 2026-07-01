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
	"time"

	"ds2api-browser/config"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

type Session struct {
	cfg               *config.Config
	chromeCmd         *exec.Cmd
	allocCtx          context.Context
	allocCancel       context.CancelFunc
	browserCtx        context.Context
	browserCancel     context.CancelFunc
	ctxMu             sync.Mutex
	loggedIn          bool
	port              int
	currentAccountIdx int    // 当前使用的账号索引
	currentEmail      string // 当前登录的账号邮箱
}

func NewSession(cfg *config.Config) *Session {
	return &Session{cfg: cfg}
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
		s.browserCtx, s.browserCancel = chromedp.NewContext(s.allocCtx, chromedp.WithTargetID(targetID))
		log.Printf("[session] reusing existing target: %s", targetID)
	} else {
		s.browserCtx, s.browserCancel = chromedp.NewContext(s.allocCtx)
		log.Println("[session] created new target")
	}

	// 强制设置浏览器窗口大小（覆盖 Chrome 记忆的上次窗口状态）
	s.setWindowSize(900, 600)

	// 关闭 Chrome session restore 自动恢复的多余 DeepSeek 标签页
	// 根因：findDeepSeekTarget() 可能在 session restore 完成前执行，
	// 导致创建新标签页的同时 session restore 又恢复出旧标签页
	s.closeExtraDeepSeekTargets()

	return nil
}

// closeExtraDeepSeekTargets 关闭多余的 DeepSeek 标签页（保留当前正在使用的）
// 解决 Chrome session restore 自动恢复历史标签页导致出现多个 DeepSeek 页面的问题
func (s *Session) closeExtraDeepSeekTargets() {
	currentTargetID := target.ID("")
	if s.browserCtx != nil {
		if id := chromedp.FromContext(s.browserCtx); id != nil && id.Target != nil {
			currentTargetID = id.Target.TargetID
		}
	}
	if currentTargetID == "" {
		return
	}

	infos, err := chromedp.Targets(s.allocCtx)
	if err != nil {
		log.Printf("[session] closeExtraTargets: list targets failed: %v (skip, non-critical)", err)
		return
	}

	closed := 0
	for _, info := range infos {
		if info.Type != "page" {
			continue
		}
		if !strings.Contains(info.URL, "chat.deepseek.com") {
			continue
		}
		if info.TargetID == currentTargetID {
			continue
		}
		err := chromedp.Run(s.allocCtx, chromedp.ActionFunc(func(ctx context.Context) error {
			return target.CloseTarget(info.TargetID).Do(ctx)
		}))
		if err != nil {
			log.Printf("[session] closeExtraTargets: close %s failed: %v", info.TargetID, err)
		} else {
			closed++
			log.Printf("[session] closed extra DeepSeek target: %s (url=%s)", info.TargetID, info.URL)
		}
	}
	if closed > 0 {
		log.Printf("[session] closed %d extra DeepSeek tab(s)", closed)
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

func (s *Session) resetCtx() {
	s.ctxMu.Lock()
	defer s.ctxMu.Unlock()

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
		s.browserCtx, s.browserCancel = chromedp.NewContext(s.allocCtx, chromedp.WithTargetID(targetID))
		log.Printf("[session] reset: reusing target %s", targetID)
	} else {
		s.browserCtx, s.browserCancel = chromedp.NewContext(s.allocCtx)
		log.Println("[session] reset: new target")
	}

	s.setWindowSize(900, 600)
}

func (s *Session) Context() context.Context {
	s.ctxMu.Lock()
	if s.browserCtx != nil && s.browserCtx.Err() == nil {
		ctx := s.browserCtx
		s.ctxMu.Unlock()
		return ctx
	}
	s.ctxMu.Unlock()

	s.resetCtx()

	s.ctxMu.Lock()
	ctx := s.browserCtx
	s.ctxMu.Unlock()
	return ctx
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

	s.loggedIn = true
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

// SwitchAccount 切换到下一个账号并重新登录
// 返回切换后的账号邮箱，如果只有一个账号则返回空字符串表示无法切换
func (s *Session) SwitchAccount() (string, error) {
	if len(s.cfg.Accounts) <= 1 {
		log.Println("[session] only one account, cannot switch")
		return "", fmt.Errorf("only one account configured")
	}

	// 计算下一个账号索引
	nextIdx := (s.currentAccountIdx + 1) % len(s.cfg.Accounts)
	nextAccount := s.cfg.Accounts[nextIdx]

	log.Printf("[session] switching account from %s (idx=%d) to %s (idx=%d)",
		s.currentEmail, s.currentAccountIdx, nextAccount.Email, nextIdx)

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
			return "", fmt.Errorf("login to %s did not result in chat page", nextAccount.Email)
		}
	}

	s.currentAccountIdx = nextIdx
	s.currentEmail = nextAccount.Email
	s.loggedIn = true

	log.Printf("[session] successfully switched to account %s", nextAccount.Email)

	// 检查深度思考和智能搜索开关状态
	s.checkToggleStates()

	return nextAccount.Email, nil
}

// checkToggleStates 检查并确保"深度思考"和"智能搜索"处于开启状态
// 登录后 DeepSeek 默认关闭深度思考，需要自动点击开启
// DOM 特征：div.ds-toggle-button，aria-pressed="true" 表示开启，class 含 --selected 表示开启
// 注意：DeepSeek 的 React 按钮对 JS .click() 无效，必须用 chromedp.MouseClickXY 真实点击
func (s *Session) checkToggleStates() {
	// 第一步：用 JS 检测状态和获取按钮坐标
	var detectResult string
	chromedp.Run(s.Context(), chromedp.Evaluate(`(()=>{
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
	})()`, &detectResult))
	log.Printf("[session] toggle states: %s", detectResult)

	// 第二步：如果深度思考未开启，用真实鼠标点击
	if strings.Contains(detectResult, `"thinkingOn":false`) && strings.Contains(detectResult, `"thinkingPos":`) {
		var pos struct {
			ThinkingPos *struct {
				X int `json:"x"`
				Y int `json:"y"`
			} `json:"thinkingPos"`
		}
		json.Unmarshal([]byte(detectResult), &pos)
		if pos.ThinkingPos != nil {
			log.Printf("[session] clicking 深度思考 toggle at (%d,%d)", pos.ThinkingPos.X, pos.ThinkingPos.Y)
			chromedp.Run(s.Context(), chromedp.MouseClickXY(float64(pos.ThinkingPos.X), float64(pos.ThinkingPos.Y)))
			time.Sleep(500 * time.Millisecond)

			// 验证点击后是否已开启
			var verify string
			chromedp.Run(s.Context(), chromedp.Evaluate(`(()=>{
				var els = document.querySelectorAll('div.ds-toggle-button[aria-pressed], div.ds-toggle-button');
				for (var i = 0; i < els.length; i++) {
					if ((els[i].textContent||'').trim().includes('深度思考')) {
						return JSON.stringify({on: els[i].getAttribute('aria-pressed') === 'true' || els[i].className.includes('--selected')});
					}
				}
				return JSON.stringify({on: false, error: 'not_found'});
			})()`, &verify))
			log.Printf("[session] 深度思考 after click: %s", verify)
		}
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
	s.loggedIn = false
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

func (s *Session) NavigateHome(ctx context.Context) error {
	return chromedp.Run(s.Context(),
		chromedp.Navigate("https://chat.deepseek.com/"),
	)
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
