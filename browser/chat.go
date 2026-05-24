package browser

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chromedp/chromedp"
)

var observeCaptureScript = `
(() => {
	window.__dsBrowserDOMDone = false;
	window.__dsObserveActive = true;
	let lastText = '';
	let unchangedCount = 0;

	function scan() {
		const articles = document.querySelectorAll('[class*="ds-markdown"]');
		if (articles.length === 0) return;
		const last = articles[articles.length - 1];
		const text = (last.textContent || '').trim();
		if (text && text !== lastText) {
			lastText = text;
			unchangedCount = 0;
		} else if (text && text === lastText) {
			unchangedCount++;
		}
		if (text && unchangedCount >= 5) {
			window.__dsBrowserDOMDone = true;
		}
	}
	setInterval(scan, 1000);
	scan();
})();
`

type ChatHandler struct {
	session *Session
}

type ChatRequest struct {
	Text   string   `json:"text"`
	Images []string `json:"images"`
	Model  string   `json:"model,omitempty"`
}

type ChatResponse struct {
	Content  string `json:"content"`
	Thinking string `json:"reasoning_content,omitempty"`
	Error    string `json:"error,omitempty"`
}

func NewChatHandler(session *Session) *ChatHandler {
	return &ChatHandler{session: session}
}

func (h *ChatHandler) SendImageChat(ctx context.Context, req *ChatRequest) (*ChatResponse, error) {
	if len(req.Images) == 0 {
		return nil, fmt.Errorf("no images provided for image chat")
	}

	if err := h.ensureReady(ctx); err != nil {
		return nil, fmt.Errorf("ensure ready: %w", err)
	}

	if err := h.switchToImageMode(ctx); err != nil {
		return nil, fmt.Errorf("switch to image mode: %w", err)
	}

	filePath, err := h.saveBase64Image(req.Images[0])
	if err != nil {
		return nil, fmt.Errorf("save image: %w", err)
	}
	defer os.Remove(filePath)

	log.Println("[chat] uploading image...")
	if err := h.uploadImage(ctx, filePath); err != nil {
		return nil, fmt.Errorf("upload image: %w", err)
	}
	log.Println("[chat] image uploaded")

	log.Println("[chat] injecting interceptor...")
	if err := h.injectInterceptor(ctx); err != nil {
		return nil, fmt.Errorf("inject interceptor: %w", err)
	}
	log.Println("[chat] interceptor injected")

	log.Println("[chat] sending message...")
	if err := h.sendMessage(ctx, req.Text); err != nil {
		return nil, fmt.Errorf("send message: %w", err)
	}
	log.Println("[chat] message sent, waiting for response...")

	content, thinking, err := h.waitForResponse(ctx, 60*time.Second)
	if err != nil {
		return nil, fmt.Errorf("wait response: %w", err)
	}
	log.Printf("[chat] got response: %d chars, thinking: %d chars", len([]rune(content)), len([]rune(thinking)))

	_ = h.session.NavigateHome(ctx)

	return &ChatResponse{Content: content, Thinking: thinking}, nil
}

func (h *ChatHandler) ensureReady(ctx context.Context) error {
	if !h.session.loggedIn {
		return fmt.Errorf("not logged in")
	}

	var info string
	err := chromedp.Run(h.session.Context(),
		chromedp.Evaluate(`(()=>{
			var ta = document.querySelector('textarea');
			return 'ta:' + !!ta + ' url:' + window.location.href;
		})()`, &info),
	)
	log.Printf("[chat] page state: %s (err=%v)", info, err)

	if err != nil {
		log.Println("[chat] evaluate failed, navigating home")
		h.session.NavigateHome(ctx)
		time.Sleep(3 * time.Second)
	}

	var exists bool
	err = chromedp.Run(h.session.Context(),
		chromedp.Evaluate(`!!document.querySelector('textarea')`, &exists),
	)
	log.Printf("[chat] textarea exists: %v (err=%v)", exists, err)

	if !exists || err != nil {
		log.Println("[chat] textarea not found, navigating home")
		h.session.NavigateHome(ctx)
		time.Sleep(3 * time.Second)

		err = chromedp.Run(h.session.Context(),
			chromedp.Evaluate(`!!document.querySelector('textarea')`, &exists),
		)
		log.Printf("[chat] after navigate: textarea=%v (err=%v)", exists, err)

		if !exists || err != nil {
			return fmt.Errorf("textarea still not found after navigation")
		}
	}
	return nil
}

func (h *ChatHandler) switchToImageMode(ctx context.Context) error {
	var currentMode string
	_ = chromedp.Run(h.session.Context(),
		chromedp.Evaluate(`(()=>{
			const radios = document.querySelectorAll('[role="radio"]');
			for (const r of radios) {
				if (r.getAttribute('aria-checked') === 'true' || r.classList.contains('_37fb93d')) {
					return (r.textContent || '').trim();
				}
			}
			return '';
		})()`, &currentMode),
	)

	log.Printf("[chat] current mode: %q", currentMode)

	if strings.Contains(currentMode, "识图") {
		log.Println("[chat] already in image mode")
		return nil
	}

	log.Printf("[chat] switching from %q to image mode", currentMode)

	var clickResult string
	err := chromedp.Run(h.session.Context(),
		chromedp.Evaluate(`(()=>{
			const radios = document.querySelectorAll('[role="radio"]');
			for (const r of radios) {
				const txt = (r.textContent || '').trim();
				if (txt === '识图模式') {
					r.click();
					return 'clicked';
				}
			}
			for (const r of radios) {
				const txt = (r.textContent || '').trim();
				if (txt.includes('识图')) {
					r.click();
					return 'clicked_partial:' + txt;
				}
			}
			return 'not_found:' + radios.length + ' radios';
		})()`, &clickResult),
		chromedp.Sleep(800*time.Millisecond),
	)

	log.Printf("[chat] click result: %s (err=%v)", clickResult, err)

	if err != nil {
		return fmt.Errorf("click image mode radio: %w", err)
	}

	if strings.Contains(clickResult, "not_found") {
		log.Println("[chat] mode radios not found, navigating home first")
		h.session.NavigateHome(ctx)
		time.Sleep(3 * time.Second)
		return h.switchToImageMode(ctx)
	}

	_ = chromedp.Run(h.session.Context(),
		chromedp.Evaluate(`(()=>{
			const radios = document.querySelectorAll('[role="radio"]');
			for (const r of radios) {
				if (r.getAttribute('aria-checked') === 'true' || r.classList.contains('_37fb93d')) {
					return (r.textContent || '').trim();
				}
			}
			return '';
		})()`, &currentMode),
	)

	log.Printf("[chat] after click, mode: %q", currentMode)

	if !strings.Contains(currentMode, "识图") {
		return fmt.Errorf("failed to switch to image mode, current=%q", currentMode)
	}

	log.Println("[chat] switched to image mode")
	return nil
}

func (h *ChatHandler) saveBase64Image(dataURL string) (string, error) {
	parts := strings.SplitN(dataURL, ",", 2)
	if len(parts) != 2 {
		parts = []string{"", dataURL}
	}

	data, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("decode base64: %w", err)
	}

	tmpDir := os.TempDir()
	ext := ".png"
	if strings.Contains(parts[0], "jpeg") || strings.Contains(parts[0], "jpg") {
		ext = ".jpg"
	} else if strings.Contains(parts[0], "gif") {
		ext = ".gif"
	} else if strings.Contains(parts[0], "webp") {
		ext = ".webp"
	}

	filePath := filepath.Join(tmpDir, fmt.Sprintf("ds_browser_%d%s", time.Now().UnixNano(), ext))
	if err := os.WriteFile(filePath, data, 0644); err != nil {
		return "", fmt.Errorf("write file: %w", err)
	}
	log.Printf("[chat] saved image to %s (%d bytes)", filePath, len(data))
	return filePath, nil
}

func (h *ChatHandler) uploadImage(ctx context.Context, filePath string) error {
	return chromedp.Run(h.session.Context(),
		chromedp.SetUploadFiles(`input[type="file"]`, []string{filePath}, chromedp.ByQuery),
		chromedp.Sleep(2*time.Second),
	)
}

func (h *ChatHandler) injectInterceptor(ctx context.Context) error {
	err := chromedp.Run(h.session.Context(),
		chromedp.Evaluate(injectScript, nil),
		chromedp.Evaluate(observeCaptureScript, nil),
	)
	if err != nil {
		return err
	}
	_ = chromedp.Run(h.session.Context(),
		chromedp.Evaluate(`setTimeout(()=>{ window.__dsObserveActive=true; }, 1000)`, nil),
	)
	log.Printf("[chat] injected both network interceptor and DOM observer")
	return nil
}

func (h *ChatHandler) sendMessage(ctx context.Context, text string) error {
	// Clear textarea using Evaluate + focus
	err := chromedp.Run(h.session.Context(),
		chromedp.Click("textarea", chromedp.ByQuery),
		chromedp.Sleep(200*time.Millisecond),
		chromedp.Evaluate(`(()=>{
			const ta = document.querySelector('textarea');
			if (!ta) return;
			ta.focus();
			ta.select();
			// Use execCommand to ensure React sees the change
			document.execCommand('delete', false, null);
		})()`, nil),
		chromedp.Sleep(300*time.Millisecond),
	)
	if err != nil {
		log.Printf("[chat] clear: %v", err)
	}
	log.Printf("[chat] cleared textarea")

	// Retry text injection up to 3 times until button enables
	escapedText := strings.ReplaceAll(text, "\\", "\\\\")
	escapedText = strings.ReplaceAll(escapedText, "`", "\\`")
	escapedText = strings.ReplaceAll(escapedText, "$", "\\$")
	var buttonEnabled bool
	for attempt := 0; attempt < 3 && !buttonEnabled; attempt++ {
		if attempt > 0 {
			log.Printf("[chat] retrying text injection (attempt %d)", attempt+1)
			chromedp.Run(h.session.Context(),
				chromedp.Evaluate(`(()=>{
					const ta = document.querySelector('textarea');
					if (ta) { ta.value = ''; ta.select(); ta.focus(); }
				})()`, nil),
				chromedp.Sleep(500*time.Millisecond),
			)
		}

		var typedLen int
		err = chromedp.Run(h.session.Context(),
			chromedp.Evaluate(fmt.Sprintf(`(()=>{
				const ta = document.querySelector('textarea');
				if (!ta) return 0;
				ta.select();
				ta.focus();
				ta.dispatchEvent(new CompositionEvent('compositionstart', { bubbles: true }));
				ta.dispatchEvent(new CompositionEvent('compositionupdate', { bubbles: true }));
				document.execCommand('insertText', false, `+"`%s`"+`);
				ta.dispatchEvent(new CompositionEvent('compositionend', { bubbles: true, data: `+"`%s`"+` }));
				ta.dispatchEvent(new Event('input', { bubbles: true }));
				ta.dispatchEvent(new Event('change', { bubbles: true }));
				return ta.value.length;
			})()`, escapedText, escapedText), &typedLen),
			chromedp.Sleep(2000*time.Millisecond),
		)
		if err != nil {
			log.Printf("[chat] insertValue (attempt %d): %v", attempt+1, err)
		}
		log.Printf("[chat] typed %d chars (insert=%d), attempt %d", len([]rune(text)), typedLen, attempt+1)

		// Check if the rightmost button is enabled
		for i := 0; i < 10; i++ {
			_ = chromedp.Run(h.session.Context(),
				chromedp.Evaluate(`(()=>{
					var ta = document.querySelector('textarea');
					if (!ta) return false;
					var taRect = ta.getBoundingClientRect();
					var all = document.querySelectorAll('div[role="button"][class*="ds-icon-button"]');
					var best = null, bestX = -1;
					for (var j = 0; j < all.length; j++) {
						var r = all[j].getBoundingClientRect();
						if (r.width > 0 && r.height > 0 && r.x > taRect.x + taRect.width * 0.5) {
							if (r.x > bestX) { bestX = r.x; best = all[j]; }
						}
					}
					return best !== null && best.getAttribute('aria-disabled') !== 'true';
				})()`, &buttonEnabled),
			)
			if buttonEnabled {
				log.Printf("[chat] send button enabled after %d checks (attempt %d)", i+1, attempt+1)
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	if !buttonEnabled {
		log.Println("[chat] WARNING: send button still disabled, attempting to send anyway")
	}

	// State check
	var stateInfo string
	_ = chromedp.Run(h.session.Context(),
		chromedp.Evaluate(`(()=>{
			var ta = document.querySelector('textarea');
			var info = [];
			info.push('len=' + (ta ? ta.value.length : -1));
			var ib = document.querySelectorAll('div[role="button"][class*="ds-icon-button"]');
			for (var i = 0; i < ib.length; i++) {
				var r = ib[i].getBoundingClientRect();
				if (r.width === 0) continue;
				info.push('#' + i + ' x=' + Math.round(r.x) + ' dis=' + (ib[i].getAttribute('aria-disabled')==='true') + ' ' + (ib[i].className||'').toString().substring(0,30));
			}
			return info.join(' | ');
		})()`, &stateInfo),
	)
	log.Printf("[chat] state: %s", stateInfo)

	// Click the rightmost enabled button
	var clickResult string
	_ = chromedp.Run(h.session.Context(),
		chromedp.Evaluate(`(()=>{
			var ta = document.querySelector('textarea');
			if (!ta) return 'no_ta';
			var taRect = ta.getBoundingClientRect();
			var best = null, bestX = -1;
			var all = document.querySelectorAll('div[role="button"][class*="ds-icon-button"]');
			for (var i = 0; i < all.length; i++) {
				var r = all[i].getBoundingClientRect();
				if (r.width > 0 && r.height > 0 && r.x > taRect.x + taRect.width * 0.5) {
					if (all[i].getAttribute('aria-disabled') === 'true') continue;
					if (r.x > bestX) { bestX = r.x; best = all[i]; }
				}
			}
			if (!best) return 'no_right_btn';
			var cls = (best.className || '').toString().substring(0,30);
			var bx = Math.round(best.getBoundingClientRect().x);
			best.click();
			return 'clicked:' + cls + ' x=' + bx;
		})()`, &clickResult),
	)
	log.Printf("[chat] click: %s", clickResult)

	// Also try Enter as fallback
	time.Sleep(500 * time.Millisecond)
	err = chromedp.Run(h.session.Context(),
		chromedp.SendKeys("textarea", "\r", chromedp.ByQuery),
	)
	if err != nil {
		log.Printf("[chat] Enter key: %v", err)
	} else {
		log.Println("[chat] sent Enter key")
	}

	return nil
}

func (h *ChatHandler) waitForResponse(ctx context.Context, timeout time.Duration) (string, string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var netDone, domDone bool
		var content, thinking string
		err := chromedp.Run(h.session.Context(),
			chromedp.Evaluate(`window.__dsBrowserDone === true`, &netDone),
			chromedp.Evaluate(`window.__dsBrowserDOMDone === true`, &domDone),
			chromedp.Evaluate(`window.__dsBrowserCapture || ''`, &content),
			chromedp.Evaluate(`window.__dsBrowserThinking || ''`, &thinking),
		)
		if err == nil && content != "" && (netDone || domDone) {
			var browserLog string
			chromedp.Run(h.session.Context(),
				chromedp.Evaluate(`JSON.stringify((window.__dsBrowserLog||[]).slice(-30))`, &browserLog),
			)
			var ptypeJSON string
			chromedp.Run(h.session.Context(),
				chromedp.Evaluate(`JSON.stringify(window.__dsBrowserPTypes||{})`, &ptypeJSON),
			)
			var samplesJSON string
			chromedp.Run(h.session.Context(),
				chromedp.Evaluate(`JSON.stringify(window.__dsBrowserSamples||{})`, &samplesJSON),
			)
			var rawSSE string
			chromedp.Run(h.session.Context(),
				chromedp.Evaluate(`window.__dsBrowserRawSSE||''`, &rawSSE),
			)
			log.Printf("[chat] captured: %d chars, thinking: %d chars (netDone=%v domDone=%v) ptypes=%s samples=%s rawSSE(->%d) urls=%s",
				len([]rune(content)), len([]rune(thinking)), netDone, domDone, ptypeJSON, samplesJSON, len(rawSSE), browserLog)
			if len(rawSSE) > 0 {
				os.WriteFile(filepath.Join(os.TempDir(), "ds_raw_sse.txt"), []byte(rawSSE), 0644)
			}
			return content, thinking, nil
		}
		if err == nil && !netDone && !domDone {
			log.Printf("[chat] waiting... %d chars so far", len([]rune(content)))
		}
		time.Sleep(1500 * time.Millisecond)
	}
	return "", "", fmt.Errorf("timeout waiting for response after %v", timeout)
}
