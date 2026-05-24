package browser

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

	"ds2api-browser/config"

	"github.com/chromedp/chromedp"
)

type Session struct {
	cfg           *config.Config
	allocCtx      context.Context
	allocCancel   context.CancelFunc
	browserCtx    context.Context
	browserCancel context.CancelFunc
	loggedIn      bool
	ready         bool
	userDir       string
}

func NewSession(cfg *config.Config) *Session {
	return &Session{cfg: cfg}
}

func (s *Session) Start() error {
	profileDir, err := s.resolveProfileDir()
	if err != nil {
		return fmt.Errorf("resolve profile: %w", err)
	}
	port, err := s.findBrowserPort()
	if err != nil {
		port = 9222
	}

	opts := []chromedp.ExecAllocatorOption{
		chromedp.Flag("no-first-run", true),
		chromedp.Flag("no-default-browser-check", true),
		chromedp.Flag("disable-popup-blocking", true),
		chromedp.Flag("disable-extensions", true),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("remote-debugging-port", fmt.Sprintf("%d", port)),
		chromedp.Flag("user-data-dir", profileDir),
		chromedp.WindowSize(1280, 900),
	}
	if s.cfg.ChromePath != "" {
		opts = append(opts, chromedp.ExecPath(s.cfg.ChromePath))
	}

	s.allocCtx, s.allocCancel = chromedp.NewExecAllocator(context.Background(), opts...)
	s.browserCtx, s.browserCancel = chromedp.NewContext(s.allocCtx)
	return nil
}

func (s *Session) resolveProfileDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return home + "\\ds2api-browser-profile", nil
}

func (s *Session) findBrowserPort() (int, error) {
	out, err := exec.Command("netstat", "-ano").Output()
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, ":9222") && strings.Contains(line, "LISTENING") {
			return 9222, nil
		}
	}
	return 0, fmt.Errorf("no browser debug port found")
}

func (s *Session) Login(ctx context.Context, email, password string) error {
	if s.browserCtx == nil {
		return fmt.Errorf("browser not started")
	}

	account := s.findAccount(email)
	if account == nil {
		return fmt.Errorf("account %s not found in config", email)
	}
	if password == "" {
		password = account.Password
	}

	tctx, cancel := context.WithTimeout(s.browserCtx, 30*time.Second)
	defer cancel()

	if err := chromedp.Run(tctx,
		chromedp.ActionFunc(func(ctx context.Context) error {
			return chromedp.Evaluate(`window.location.href = "https://chat.deepseek.com/"`, nil).Do(ctx)
		}),
	); err != nil {
		return fmt.Errorf("navigate home: %w", err)
	}

	time.Sleep(3 * time.Second)

	if err := s.checkAndLogin(tctx, account.Email, password); err != nil {
		return fmt.Errorf("check/login: %w", err)
	}

	s.loggedIn = true
	s.ready = true
	log.Printf("[session] login successful, ctx err=%v", s.browserCtx.Err())
	return nil
}

func (s *Session) checkAndLogin(ctx context.Context, email, password string) error {
	var hasTextarea bool
	_ = chromedp.Run(ctx,
		chromedp.Evaluate(`document.querySelector("textarea") !== null`, &hasTextarea),
	)

	if hasTextarea {
		log.Println("[session] already logged in (textarea found)")
		return nil
	}

	log.Println("[session] not logged in, navigating to sign_in...")

	if err := chromedp.Run(ctx,
		chromedp.ActionFunc(func(ctx context.Context) error {
			return chromedp.Evaluate(`window.location.href = "https://chat.deepseek.com/sign_in"`, nil).Do(ctx)
		}),
	); err != nil {
		return fmt.Errorf("navigate sign_in: %w", err)
	}

	time.Sleep(3 * time.Second)

	if err := s.doLogin(ctx, email, password); err != nil {
		return err
	}

	time.Sleep(8 * time.Second)

	var title, currentURL string
	var hasInput bool
	_ = chromedp.Run(ctx,
		chromedp.Evaluate("document.title", &title),
		chromedp.Evaluate("window.location.href", &currentURL),
		chromedp.Evaluate(`document.querySelector("textarea") !== null`, &hasInput),
	)

	log.Printf("[session] after login: url=%q title=%q textarea=%v", currentURL, title, hasInput)

	if !hasInput {
		return fmt.Errorf("login did not result in chat page")
	}

	return nil
}

func (s *Session) doLogin(ctx context.Context, email, password string) error {
	log.Println("[session] checking page layout...")

	var debug string

	_ = chromedp.Run(ctx, chromedp.Evaluate(`(()=>{
		var btns = document.querySelectorAll('button');
		var texts = [];
		for (var i = 0; i < btns.length; i++) {
			texts.push('#' + i + ':' + (btns[i].textContent||'').trim().substring(0,20));
		}
		return texts.join(' | ') || 'NO';
	})()`, &debug))
	log.Printf("[session] buttons: %s", debug)

	if strings.Contains(debug, "密码登录") {
		log.Println("[session] phone mode, switching to password login...")
		clickPwd := `(()=>{
			var btns = document.querySelectorAll('button');
			for (var i = 0; i < btns.length; i++) {
				if (btns[i].textContent.includes('密码登录')) {
					btns[i].click();
					return 'ok';
				}
			}
			return 'nf';
		})()`

		var result string
		if err := chromedp.Run(ctx, chromedp.Evaluate(clickPwd, &result)); err != nil {
			return fmt.Errorf("click password login tab: %w", err)
		}
		log.Printf("[session] click 密码登录: %s", result)
		time.Sleep(1 * time.Second)
	}

	_ = chromedp.Run(ctx, chromedp.Evaluate(`(()=>{
		var inputs = document.querySelectorAll('input');
		var result = [];
		for (var i = 0; i < inputs.length; i++) {
			result.push('#' + i + ': type=' + inputs[i].type + ' ph=' + (inputs[i].placeholder||'').substring(0,20));
		}
		return result.join(' | ') || 'NO_INPUTS';
	})()`, &debug))
	log.Printf("[session] inputs: %s", debug)

	fillScript := fmt.Sprintf(`(()=>{
		var nativeSetter = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value').set;
		var inputs = document.querySelectorAll('input');
		for (var i = 0; i < inputs.length; i++) {
			var inp = inputs[i];
			var t = inp.type || '';
			var p = (inp.placeholder || '').toLowerCase();
			if (t === 'email' || t === 'text' || p.includes('邮箱') || p.includes('email') || p.includes('mail')) {
				inp.focus();
				nativeSetter.call(inp, %q);
				inp.dispatchEvent(new Event('input', {bubbles: true}));
				inp.dispatchEvent(new Event('change', {bubbles: true}));
				return 'email_ok:' + i;
			}
		}
		return 'email_nf';
	})()`, email)

	var result string
	if err := chromedp.Run(ctx, chromedp.Evaluate(fillScript, &result)); err != nil {
		return fmt.Errorf("fill email: %w", err)
	}
	log.Printf("[session] fill email: %s", result)

	passScript := fmt.Sprintf(`(()=>{
		var nativeSetter = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value').set;
		var inputs = document.querySelectorAll('input');
		for (var i = 0; i < inputs.length; i++) {
			if (inputs[i].type === 'password') {
				inputs[i].focus();
				nativeSetter.call(inputs[i], %q);
				inputs[i].dispatchEvent(new Event('input', {bubbles: true}));
				inputs[i].dispatchEvent(new Event('change', {bubbles: true}));
				return 'pass_ok:' + i;
			}
		}
		return 'pass_nf';
	})()`, password)

	if err := chromedp.Run(ctx, chromedp.Evaluate(passScript, &result)); err != nil {
		return fmt.Errorf("fill password: %w", err)
	}
	log.Printf("[session] fill password: %s", result)

	time.Sleep(200 * time.Millisecond)

	clickLogin := `(()=>{
		var btns = document.querySelectorAll('button');
		for (var i = 0; i < btns.length; i++) {
			var t = btns[i].textContent || '';
			if (t.trim() === '登录' || t.includes('登录')) {
				btns[i].click();
				return 'btn:' + i;
			}
		}
		return 'no_btn';
	})()`

	if err := chromedp.Run(ctx, chromedp.Evaluate(clickLogin, &result)); err != nil {
		return fmt.Errorf("click login: %w", err)
	}
	log.Printf("[session] button click: %s", result)

	return nil
}

func (s *Session) findAccount(email string) *config.Account {
	for _, a := range s.cfg.Accounts {
		if a.Email == email {
			return &a
		}
	}
	return nil
}

func (s *Session) Context() context.Context {
	if s.allocCtx == nil || s.allocCtx.Err() != nil {
		log.Println("[session] allocator dead, full restart...")
		s.restartBrowser()
		return s.browserCtx
	}

	if s.browserCtx == nil || s.browserCtx.Err() != nil {
		if s.browserCancel != nil {
			s.browserCancel()
		}
		s.browserCtx, s.browserCancel = chromedp.NewContext(s.allocCtx)
		log.Println("[session] created new browser context from allocator")
	}

	return s.browserCtx
}

func (s *Session) restartBrowser() {
	if s.browserCancel != nil {
		s.browserCancel()
	}
	if s.allocCancel != nil {
		s.allocCancel()
	}

	port, err := s.findBrowserPort()
	if err != nil {
		port = 9222
	}

	profileDir, _ := s.resolveProfileDir()

	opts := []chromedp.ExecAllocatorOption{
		chromedp.Flag("no-first-run", true),
		chromedp.Flag("no-default-browser-check", true),
		chromedp.Flag("disable-popup-blocking", true),
		chromedp.Flag("disable-extensions", true),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("remote-debugging-port", fmt.Sprintf("%d", port)),
		chromedp.Flag("user-data-dir", profileDir),
		chromedp.WindowSize(1280, 900),
	}
	if s.cfg.ChromePath != "" {
		opts = append(opts, chromedp.ExecPath(s.cfg.ChromePath))
	}

	s.allocCtx, s.allocCancel = chromedp.NewExecAllocator(context.Background(), opts...)
	s.browserCtx, s.browserCancel = chromedp.NewContext(s.allocCtx)
	log.Println("[session] browser restarted")
}

func (s *Session) NavigateHome(ctx context.Context) error {
	return chromedp.Run(s.browserCtx,
		chromedp.ActionFunc(func(ctx context.Context) error {
			return chromedp.Evaluate(`window.location.href = "https://chat.deepseek.com/"`, nil).Do(ctx)
		}),
		chromedp.Sleep(3*time.Second),
	)
}

func (s *Session) Close() {
	if s.browserCancel != nil {
		s.browserCancel()
	}
	if s.allocCancel != nil {
		s.allocCancel()
	}
}
