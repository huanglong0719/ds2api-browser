package browser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chromedp/chromedp"
)

const (
	clickDelay          = 300 * time.Millisecond
	typeDelay           = 50 * time.Millisecond
	enterDelay          = 200 * time.Millisecond
	uploadDelay         = 500 * time.Millisecond
	previewCheck        = 300 * time.Millisecond
	modeSwitchDelay     = 800 * time.Millisecond
	newConvDelay        = 1500 * time.Millisecond
	pollInterval        = 300 * time.Millisecond
	maxTextChunk        = 3000
	fileUploadThreshold = 3000
	requestBodyLimit    = 10 << 20
)

// errorDetectJS 统一的 DOM 错误检测 JS，被 waitForResponse 和 detectImmediateError 共用
const errorDetectJS = `
(function() {
	var ta = document.querySelector('textarea');
	var taDisabled = ta && ta.disabled;
	var busyKeywords = ['消息发送过于频繁', '发送过于频繁', '服务器繁忙', '服务繁忙', '请稍后重试', '请稍后再试'];
	var limitKeywords = ['达到对话长度上限', '请开启新对话', '对话长度上限'];

	// 1. 精准检测 ds-message 中的短文本错误提示
	var msgSelectors = ['.ds-message', '[class*="ds-message"]', '[class*="message-bubble"]', '[class*="error-message"]', '[class*="warning"]', '[class*="system-message"]'];
	for (var s = 0; s < msgSelectors.length; s++) {
		var msgs = document.querySelectorAll(msgSelectors[s]);
		for (var i = 0; i < msgs.length; i++) {
			var txt = (msgs[i].textContent || '').trim();
			if (txt.length > 200) continue;
			for (var k = 0; k < busyKeywords.length; k++) {
				if (txt.indexOf(busyKeywords[k]) !== -1) return 'serverBusy:' + busyKeywords[k];
			}
			for (var k = 0; k < limitKeywords.length; k++) {
				if (txt.indexOf(limitKeywords[k]) !== -1) return 'convLimit:' + limitKeywords[k];
			}
		}
	}

	// 2. textarea 被禁用时，扫描页面全文
	if (taDisabled) {
		var all = (document.body && document.body.textContent) || '';
		for (var i = 0; i < busyKeywords.length; i++) {
			if (all.indexOf(busyKeywords[i]) !== -1) return 'serverBusy:' + busyKeywords[i];
		}
		for (var i = 0; i < limitKeywords.length; i++) {
			if (all.indexOf(limitKeywords[i]) !== -1) return 'convLimit:' + limitKeywords[i];
		}
		return 'serverBusy:inputDisabled';
	}

	// 3. 页面全文扫描
	var all = (document.body && document.body.textContent) || '';
	for (var i = 0; i < busyKeywords.length; i++) {
		if (all.indexOf(busyKeywords[i]) !== -1) return 'serverBusy:' + busyKeywords[i];
	}
	for (var i = 0; i < limitKeywords.length; i++) {
		if (all.indexOf(limitKeywords[i]) !== -1) return 'convLimit:' + limitKeywords[i];
	}

	// 4. toast/notification 元素检测
	var selectors = '[class*="toast"], [class*="notification"], [class*="error"], [class*="notice"], [role="alert"], [class*="snackbar"], [class*="message"]';
	var toasts = document.querySelectorAll(selectors);
	for (var j = 0; j < toasts.length; j++) {
		var ttxt = (toasts[j].textContent || toasts[j].innerText || '').trim();
		if (ttxt.length > 200) continue;
		for (var k = 0; k < busyKeywords.length; k++) {
			if (ttxt.indexOf(busyKeywords[k]) !== -1) return 'serverBusy:' + busyKeywords[k];
		}
		for (var k = 0; k < limitKeywords.length; k++) {
			if (ttxt.indexOf(limitKeywords[k]) !== -1) return 'convLimit:' + limitKeywords[k];
		}
	}

	// 5. innerHTML 深度扫描（捕获 textContent 遗漏的内容）
	var html = (document.body && document.body.innerHTML || '').substring(0, 5000);
	for (var i = 0; i < busyKeywords.length; i++) {
		if (html.indexOf(busyKeywords[i]) !== -1) return 'serverBusy:' + busyKeywords[i];
	}
	for (var i = 0; i < limitKeywords.length; i++) {
		if (html.indexOf(limitKeywords[i]) !== -1) return 'convLimit:' + limitKeywords[i];
	}

	return '';
})()
`

var observeCaptureScript = `
(() => {
	window.__dsBrowserDOMDone = false;
	window.__dsObserveActive = true;
	if (window.__dsObserveInterval) {
		clearInterval(window.__dsObserveInterval);
	}
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
		if (text && unchangedCount >= 3) {
			window.__dsBrowserDOMDone = true;
		}
	}
	window.__dsObserveInterval = setInterval(scan, 500);
	scan();
})();
`

type ChatHandler struct {
	session         *Session
	mu              sync.Mutex
	responseTimeout time.Duration
	lastActivity    atomic.Int64
}

type ChatRequest struct {
	Text   string   `json:"text"`
	Images []string `json:"images"`
	Files  []string `json:"files"`
	Model  string   `json:"model,omitempty"`
}

type ChatResponse struct {
	Content  string `json:"content"`
	Thinking string `json:"reasoning_content,omitempty"`
	Error    string `json:"error,omitempty"`
}

func NewChatHandler(session *Session, timeoutSec int) *ChatHandler {
	if timeoutSec <= 0 {
		timeoutSec = 120
	}
	return &ChatHandler{session: session, responseTimeout: time.Duration(timeoutSec) * time.Second}
}

// Session 返回底层 Session 引用
func (h *ChatHandler) Session() *Session {
	return h.session
}

func (h *ChatHandler) SendTextChat(ctx context.Context, text string, shouldNewConv bool) (*ChatResponse, error) {
	if text == "" {
		return nil, fmt.Errorf("empty text message")
	}
	return h.sendChat(ctx, "text", text, nil, nil, shouldNewConv)
}

func (h *ChatHandler) SendImageChat(ctx context.Context, req *ChatRequest, shouldNewConv bool) (*ChatResponse, error) {
	if len(req.Images) == 0 && len(req.Files) == 0 {
		return nil, fmt.Errorf("no images or files provided")
	}
	return h.sendChat(ctx, "image", req.Text, req.Images, req.Files, shouldNewConv)
}

func (h *ChatHandler) sendChat(ctx context.Context, mode string, text string, images []string, files []string, shouldNewConv bool) (*ChatResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	t0 := time.Now()
	startTime := t0
	step := func(name string) {
		log.Printf("[chat⏱] %s: +%dms (total %dms)", name, time.Since(t0)/time.Millisecond, time.Since(startTime)/time.Millisecond)
		t0 = time.Now()
	}

	if shouldNewConv {
		log.Println("[chat] starting new conversation before send")
		if err := h.NewConversation(ctx); err != nil {
			log.Printf("[chat] new conversation failed: %v", err)
		}
		step("newConversation")
	}

	if err := h.ensureReady(); err != nil {
		return nil, fmt.Errorf("ensure ready: %w", err)
	}
	step("ensureReady")

	if err := h.switchMode(ctx, mode); err != nil {
		return nil, fmt.Errorf("switch to %s mode: %w", mode, err)
	}
	step("switchMode")

	if err := h.injectInterceptor(); err != nil {
		return nil, fmt.Errorf("inject interceptor: %w", err)
	}
	step("injectInterceptor")

	if mode == "image" && len(images) > 0 {
		if err := h.uploadImageFromData(images[0]); err != nil {
			return nil, fmt.Errorf("upload image: %w", err)
		}
		step("uploadImage")
	}

	// 统一文件上传：收集客户端文件 + 长文本（转为文件）
	type fileItem struct {
		path string
		tmp  bool // 是否临时文件，需在流程结束时清理
	}
	var allFiles []fileItem

	// 客户端传的文件（路径已在 handler 中生成）
	for _, f := range files {
		allFiles = append(allFiles, fileItem{path: f})
	}

	// 长文本（超过阈值）转为文件，避免 textarea 撑大导致按钮不可见
	textRunes := len([]rune(text))
	needTextFile := textRunes > fileUploadThreshold && mode != "image"
	if needTextFile {
		log.Printf("[chat] text too long (%d chars), saving as file", textRunes)
		tmpPath := filepath.Join(os.TempDir(), fmt.Sprintf("ds_message_%d.txt", time.Now().UnixNano()))
		if err := os.WriteFile(tmpPath, []byte(text), 0644); err != nil {
			return nil, fmt.Errorf("write text file: %w", err)
		}
		defer os.Remove(tmpPath)
		allFiles = append(allFiles, fileItem{path: tmpPath, tmp: true})
	}

	// 上传所有收集到的文件（一次 SetUploadFiles 调用，不互相覆盖）
	if len(allFiles) > 0 {
		paths := make([]string, len(allFiles))
		for i, f := range allFiles {
			paths[i] = f.path
		}
		log.Printf("[chat] uploading %d file(s) to DeepSeek", len(paths))
		if err := chromedp.Run(h.session.Context(),
			chromedp.SetUploadFiles(`input[type="file"]`, paths, chromedp.ByQuery),
		); err != nil {
			return nil, fmt.Errorf("upload files: %w", err)
		}
		log.Println("[chat] files uploaded, waiting for processing...")
		time.Sleep(1 * time.Second)
		step("uploadFiles")

		// 文件上传后立即检查页面是否有服务器繁忙等错误提示
		var uploadErr string
		chromedp.Run(h.session.Context(),
			chromedp.Evaluate(errorDetectJS, &uploadErr),
		)
		if strings.HasPrefix(uploadErr, "serverBusy:") {
			log.Printf("[chat] server busy during file upload: %s", uploadErr)
			return h.retryWithAccountSwitch(ctx, mode, text, images, files)
		}
		if strings.HasPrefix(uploadErr, "convLimit:") {
			log.Printf("[chat] conv limit during file upload: %s", uploadErr)
			return h.retryWithNewConversation(ctx, mode, text, images, files)
		}

		// 输入提示文字并发送（保持 textarea 短小让按钮可见）
		prompt := text
		if needTextFile {
			prompt = "请处理附件中的内容"
		}
		if prompt == "" {
			prompt = "请处理附件中的内容"
		}
		if err := h.clearTextarea(); err != nil {
			return nil, fmt.Errorf("clear textarea: %w", err)
		}
		if err := h.typeText(prompt); err != nil {
			return nil, fmt.Errorf("type prompt: %w", err)
		}
		stillInBox, err := h.pressEnter()
		if err != nil {
			return nil, fmt.Errorf("press enter: %w", err)
		}
		if stillInBox {
			// 发送可能被页面错误遮挡，先检查错误再做兜底点击
			errType, errMsg := h.detectImmediateError()
			if errType == "serverBusy" {
				log.Printf("[chat] detected %s after file upload: %s", errType, errMsg)
				return h.retryWithAccountSwitch(ctx, mode, text, images, files)
			}
			if errType == "convLimit" {
				log.Printf("[chat] detected %s after file upload: %s", errType, errMsg)
				return h.retryWithNewConversation(ctx, mode, text, images, files)
			}
			// 兜底点击发送（可能页面只是卡顿，没有错误提示）
			if err := h.ensureMessageSent(); err != nil {
				return nil, err
			}
			// 兜底成功，继续走正常等待响应流程
		}
	} else {
		// 无文件，直接输入文本
		if err := h.sendMessage(text); err != nil {
			log.Printf("[chat] sendMessage failed: %v, navigating home and retrying once", err)
			h.session.NavigateHome(ctx)
			time.Sleep(2 * time.Second)
			if err2 := h.switchMode(ctx, mode); err2 != nil {
				return nil, fmt.Errorf("send message: %w", err)
			}
			if err2 := h.sendMessage(text); err2 != nil {
				return nil, fmt.Errorf("send message: %w", err2)
			}
		}
	}
	step("sendMessage")

	errType, errMsg := h.detectImmediateError()
	if errType != "" {
		log.Printf("[chat] detected %s immediately after send: %s", errType, errMsg)
		if errType == "serverBusy" {
			return h.retryWithAccountSwitch(ctx, mode, text, images, files)
		}
		return h.retryWithNewConversation(ctx, mode, text, images, files)
	}
	step("immediateErrorDetection")

	content, thinking, convLimit, serverBusy, err := h.waitForResponse(ctx, h.responseTimeout)
	if err != nil {
		return &ChatResponse{Error: err.Error()}, fmt.Errorf("wait response: %w", err)
	}
	step("waitForResponse")

	// 检测服务器繁忙/消息过于频繁，切换账号并重试（优先级高于对话长度上限）
	if serverBusy || hasServerBusy(content) {
		log.Println("[chat] server busy detected, switching account and retrying...")
		return h.retryWithAccountSwitch(ctx, mode, text, images, files)
	}

	// 检测对话长度上限，自动开启新对话并重试
	if convLimit || hasConvLimit(content) {
		log.Println("[chat] conversation limit detected, starting new conversation and retrying...")
		return h.retryWithNewConversation(ctx, mode, text, images, files)
	}

	log.Printf("[chat] got response: %d chars, thinking: %d chars", len([]rune(content)), len([]rune(thinking)))
	h.lastActivity.Store(time.Now().UnixNano())
	return &ChatResponse{Content: content, Thinking: thinking}, nil
}

func (h *ChatHandler) ShouldNewConversation() bool {
	ns := h.lastActivity.Load()
	if ns == 0 {
		return true
	}
	return time.Since(time.Unix(0, ns)) > 10*time.Minute
}

func (h *ChatHandler) ensureReady() error {
	if !h.session.loggedIn {
		return fmt.Errorf("not logged in")
	}
	// 每次发送前清理多余标签页，确保只有一个 DeepSeek 页
	h.session.closeExtraTargets()
	if err := h.session.ensureOnDeepSeek(h.session.Context()); err != nil {
		return fmt.Errorf("navigate to DeepSeek: %w", err)
	}
	var exists bool
	err := chromedp.Run(h.session.Context(),
		chromedp.Evaluate(`!!document.querySelector('textarea')`, &exists),
	)
	log.Printf("[chat] textarea exists: %v (err=%v)", exists, err)
	if !exists || err != nil {
		return fmt.Errorf("textarea not found on page")
	}
	return nil
}

// getDirectText 获取元素直接文本节点内容（排除子元素嵌套文本），避免 radio button 文本重复问题
const getDirectText = `(el) => {
	let txt = '';
	for (const n of el.childNodes) {
		if (n.nodeType === 3) txt += n.textContent;
	}
	return txt.trim();
}`

// getCurrentModeFromRadio 获取当前选中 radio 的文本，先尝试直接文本节点，回退到 textContent 并去重
const getCurrentModeJS = `(()=>{
	const radios = document.querySelectorAll('[role="radiogroup"] [role="radio"]');
	if (radios.length === 0) {
		// 回退方案：查找所有 role=radio
		const allRadios = document.querySelectorAll('[role="radio"]');
		for (const r of allRadios) {
			if (r.getAttribute('aria-checked') === 'true') {
				let txt = '';
				for (const n of r.childNodes) {
					if (n.nodeType === 3) txt += n.textContent;
				}
				return txt.trim() || (r.textContent || '').trim().replace(/(识图模式)+/g, '识图模式').replace(/(默认模式)+/g, '默认模式').replace(/(快速模式)+/g, '快速模式');
			}
		}
		// 回退：没找到 radio，检查是否有实际已上传的图片预览（对话进行中 radio 可能被隐藏）
		// 注意：input[type="file"] 在所有模式中都存在（附件按钮），不能作为识图模式的依据
		const previewImgs = document.querySelectorAll('img[src*="blob:"], img[src*="data:"]');
		if (previewImgs.length > 0) {
			return '识图模式'; // 有图片预览，说明处于识图模式且有已上传图片
		}
		return '';
	}
	for (const r of radios) {
		if (r.getAttribute('aria-checked') === 'true') {
			let txt = '';
			for (const n of r.childNodes) {
				if (n.nodeType === 3) txt += n.textContent;
			}
			return txt.trim() || (r.textContent || '').trim().replace(/(识图模式)+/g, '识图模式').replace(/(默认模式)+/g, '默认模式').replace(/(快速模式)+/g, '快速模式');
		}
	}
	return '';
})()`

func (h *ChatHandler) switchToTextMode(ctx context.Context) error {
	var currentMode string
	if err := chromedp.Run(h.session.Context(),
		chromedp.Evaluate(getCurrentModeJS, &currentMode),
	); err != nil {
		log.Printf("[chat] detect text mode error: %v", err)
	}
	log.Printf("[chat] current mode: %q", currentMode)
	if currentMode == "" || !strings.Contains(currentMode, "识图") {
		log.Println("[chat] already in text mode")
		return nil
	}
	log.Printf("[chat] switching from %q to text mode", currentMode)
	var clickResult string
	err := chromedp.Run(h.session.Context(),
		chromedp.Evaluate(`(()=>{
			// 先找 radiogroup 内的 radio
			let radios = document.querySelectorAll('[role="radiogroup"] [role="radio"]');
			if (radios.length === 0) {
				radios = document.querySelectorAll('[role="radio"]');
			}
			for (const r of radios) {
				let txt = '';
				for (const n of r.childNodes) {
					if (n.nodeType === 3) txt += n.textContent;
				}
				txt = txt.trim();
				if (!txt) txt = (r.textContent || '').trim();
				// 跳过任何包含 识图 的选项
				if (!txt.includes('识图')) {
					r.click();
					return 'clicked:' + txt;
				}
			}
			// 没找到非识图选项，尝试查找包含 '识图' 的并取消选中
			for (const r of radios) {
				let txt = '';
				for (const n of r.childNodes) {
					if (n.nodeType === 3) txt += n.textContent;
				}
				txt = txt.trim();
				if (!txt) txt = (r.textContent || '').trim();
				if (txt.includes('识图') && r.getAttribute('aria-checked') === 'true') {
					// 试图取消选中 - 点击当前选中的识图模式
					// 某些 UI 点击已选中的 radio 会取消
					r.click();
					return 'attempt_uncheck:' + txt;
				}
			}
			return 'not_found:' + radios.length + ' radios';
		})()`, &clickResult),
		chromedp.Sleep(clickDelay),
	)
	log.Printf("[chat] switch to text mode result: %s (err=%v)", clickResult, err)
	if err != nil {
		return fmt.Errorf("click text mode radio: %w", err)
	}
	if strings.Contains(clickResult, "not_found") {
		h.session.NavigateHome(ctx)
		time.Sleep(1 * time.Second)
	}
	log.Println("[chat] switched to text mode")
	return nil
}

func (h *ChatHandler) switchToImageMode(ctx context.Context) error {
	return h.switchToImageModeDepth(ctx, 0)
}

func (h *ChatHandler) switchToImageModeDepth(ctx context.Context, depth int) error {
	var currentMode string
	if err := chromedp.Run(h.session.Context(),
		chromedp.Evaluate(getCurrentModeJS, &currentMode),
	); err != nil {
		log.Printf("[chat] detect image mode error: %v", err)
	}

	log.Printf("[chat] current mode: %q", currentMode)

	if strings.Contains(currentMode, "识图") {
		log.Println("[chat] already in image mode")
		return nil
	}

	log.Printf("[chat] switching from %q to image mode", currentMode)

	var clickResult string
	err := chromedp.Run(h.session.Context(),
		chromedp.Evaluate(`(()=>{
			// 先找 radiogroup 内的 radio
			let radios = document.querySelectorAll('[role="radiogroup"] [role="radio"]');
			if (radios.length === 0) {
				radios = document.querySelectorAll('[role="radio"]');
			}
			// 获取元素的直接文本（排除子元素嵌套文本）
			function getDirectText(el) {
				let txt = '';
				for (const n of el.childNodes) {
					if (n.nodeType === 3) txt += n.textContent;
				}
				return txt.trim();
			}
			for (const r of radios) {
				let txt = getDirectText(r);
				if (!txt) txt = (r.textContent || '').trim().replace(/(识图模式)+/g, '识图模式').replace(/(默认模式)+/g, '默认模式');
				if (txt === '识图模式') {
					r.click();
					return 'clicked';
				}
			}
			for (const r of radios) {
				let txt = getDirectText(r);
				if (!txt) txt = (r.textContent || '').trim().replace(/(识图模式)+/g, '识图模式').replace(/(默认模式)+/g, '默认模式');
				if (txt.includes('识图')) {
					r.click();
					return 'clicked_partial:' + txt;
				}
			}
			return 'not_found:' + radios.length + ' radios';
		})()`, &clickResult),
		chromedp.Sleep(modeSwitchDelay),
	)

	log.Printf("[chat] click result: %s (err=%v)", clickResult, err)

	if err != nil {
		return fmt.Errorf("click image mode radio: %w", err)
	}

	if strings.Contains(clickResult, "not_found") {
		if depth >= 2 {
			return fmt.Errorf("image mode radios not found after %d navigation attempts", depth)
		}
		log.Println("[chat] mode radios not found, navigating home first")
		h.session.NavigateHome(ctx)
		time.Sleep(3 * time.Second)
		return h.switchToImageModeDepth(ctx, depth+1)
	}

	if err := chromedp.Run(h.session.Context(),
		chromedp.Evaluate(getCurrentModeJS, &currentMode),
	); err != nil {
		log.Printf("[chat] detect image mode error: %v", err)
	}

	log.Printf("[chat] after click, mode: %q", currentMode)

	if !strings.Contains(currentMode, "识图") {
		// 尝试额外等一秒再检查
		time.Sleep(1 * time.Second)
		chromedp.Run(h.session.Context(),
			chromedp.Evaluate(getCurrentModeJS, &currentMode),
		)
		log.Printf("[chat] after extra wait, mode: %q", currentMode)
		if !strings.Contains(currentMode, "识图") {
			return fmt.Errorf("failed to switch to image mode, current=%q", currentMode)
		}
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

func (h *ChatHandler) uploadImage(filePath string) error {
	err := chromedp.Run(h.session.Context(),
		chromedp.SetUploadFiles(`input[type="file"]`, []string{filePath}, chromedp.ByQuery),
	)
	if err != nil {
		return err
	}
	var uploaded bool
	for i := range 10 {
		chromedp.Run(h.session.Context(),
			chromedp.Evaluate(`(()=>{
				const imgs = document.querySelectorAll('img[src*="blob:"], img[src*="data:"], [class*="preview"], [class*="thumbnail"], [class*="upload"]');
				return imgs.length > 0;
			})()`, &uploaded),
		)
		if uploaded {
			log.Printf("[chat] image preview detected after %d checks", i+1)
			break
		}
		time.Sleep(previewCheck)
	}
	if !uploaded {
		log.Println("[chat] no preview detected, waiting extra time")
		time.Sleep(1 * time.Second)
	}
	time.Sleep(uploadDelay)
	return nil
}

// uploadTextFile 将长文本保存为临时文件并上传，避免 textarea 撑大导致发送按钮不可见
func (h *ChatHandler) uploadTextFile(text string) error {
	tmpDir := os.TempDir()
	filePath := filepath.Join(tmpDir, fmt.Sprintf("ds_message_%d.txt", time.Now().UnixNano()))
	if err := os.WriteFile(filePath, []byte(text), 0644); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}
	defer os.Remove(filePath)

	log.Printf("[chat] uploading text file (%d chars)", len([]rune(text)))

	if err := chromedp.Run(h.session.Context(),
		chromedp.SetUploadFiles(`input[type="file"]`, []string{filePath}, chromedp.ByQuery),
	); err != nil {
		return fmt.Errorf("set upload file: %w", err)
	}

	log.Println("[chat] waiting for file upload...")
	time.Sleep(1 * time.Second)

	// 输入简短提示并发送，保持 textarea 短小让按钮可见
	prompt := "请处理附件中的内容"
	if err := h.clearTextarea(); err != nil {
		return fmt.Errorf("clear textarea for prompt: %w", err)
	}
	if err := h.typeText(prompt); err != nil {
		return fmt.Errorf("type prompt: %w", err)
	}

	stillInBox, err := h.pressEnter()
	if err != nil {
		return fmt.Errorf("press enter after file upload: %w", err)
	}
	if stillInBox {
		return h.ensureMessageSent()
	}
	return nil
}

func (h *ChatHandler) injectInterceptor() error {
	// 检查页面中是否实际存在拦截器（页面重载后会丢失）
	var injected bool
	if err := chromedp.Run(h.session.Context(),
		chromedp.Evaluate(`window.__dsInjectDone === true`, &injected),
	); err != nil {
		log.Printf("[chat] check interceptor error: %v, will re-inject", err)
	}

	if !injected {
		err := chromedp.Run(h.session.Context(),
			chromedp.Evaluate(InjectScript, nil),
			chromedp.Evaluate(observeCaptureScript, nil),
		)
		if err != nil {
			return err
		}
		log.Println("[chat] interceptor injected (page reloaded or first time)")
	}
	return chromedp.Run(h.session.Context(),
		chromedp.Evaluate(`(()=>{
			window.__dsBrowserCapture = '';
			window.__dsBrowserThinking = '';
			window.__dsBrowserDone = false;
			window.__dsBrowserDOMDone = false;
			window.__dsCurrentFragmentType = '';
			window.__dsConvLimitHit = false;
			window.__dsServerBusy = false;
		})()`, nil),
	)
}

func (h *ChatHandler) sendMessage(text string) error {
	log.Printf("[chat] preparing to type %d chars", len([]rune(text)))
	if err := h.clearTextarea(); err != nil {
		return fmt.Errorf("clear textarea: %w", err)
	}
	if err := h.typeText(text); err != nil {
		return fmt.Errorf("type text: %w", err)
	}
	stillInBox, err := h.pressEnter()
	if err != nil {
		return fmt.Errorf("press enter: %w", err)
	}
	if stillInBox {
		return h.ensureMessageSent()
	}
	return nil
}

// sendMessageOrUpload 根据文本长度自动选择 typing 或文件上传（供重试路径使用，支持已有附件）
func (h *ChatHandler) sendMessageOrUpload(text string, files []string) error {
	// 合并可能的长文本文件与已有附件
	var paths []string
	if len(files) > 0 {
		paths = make([]string, len(files))
		copy(paths, files)
	}
	if len([]rune(text)) > fileUploadThreshold {
		tmpPath := filepath.Join(os.TempDir(), fmt.Sprintf("ds_message_%d.txt", time.Now().UnixNano()))
		if err := os.WriteFile(tmpPath, []byte(text), 0644); err != nil {
			return fmt.Errorf("write temp text: %w", err)
		}
		defer os.Remove(tmpPath)
		paths = append(paths, tmpPath)
	}

	// 有附件需要上传时，统一 SetUploadFiles 后再发送提示文字
	if len(paths) > 0 {
		log.Printf("[chat] retry uploading %d file(s)", len(paths))
		if err := chromedp.Run(h.session.Context(),
			chromedp.SetUploadFiles(`input[type="file"]`, paths, chromedp.ByQuery),
		); err != nil {
			return fmt.Errorf("upload files: %w", err)
		}
		time.Sleep(1 * time.Second)

		prompt := text
		if len([]rune(text)) > fileUploadThreshold {
			prompt = "请处理附件中的内容"
		}
		if prompt == "" {
			prompt = "请处理附件中的内容"
		}
		if err := h.clearTextarea(); err != nil {
			return fmt.Errorf("clear textarea: %w", err)
		}
		if err := h.typeText(prompt); err != nil {
			return fmt.Errorf("type prompt: %w", err)
		}
		stillInBox, err := h.pressEnter()
		if err != nil {
			return fmt.Errorf("press enter: %w", err)
		}
		if stillInBox {
			return h.ensureMessageSent()
		}
		return nil
	}

	// 无任何附件，直接输入文本
	return h.sendMessage(text)
}

func (h *ChatHandler) clearTextarea() error {
	err := chromedp.Run(h.session.Context(),
		chromedp.Click("textarea", chromedp.ByQuery),
		chromedp.Sleep(100*time.Millisecond),
	)
	if err != nil {
		log.Printf("[chat] click textarea: %v", err)
	}
	var cleared bool
	chromedp.Run(h.session.Context(),
		chromedp.Evaluate(`(()=>{
			const ta = document.querySelector('textarea');
			if (!ta) return false;
			ta.focus();
			const setter = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value').set;
			setter.call(ta, '');
			ta.dispatchEvent(new Event('input', { bubbles: true }));
			return true;
		})()`, &cleared),
	)
	log.Printf("[chat] textarea cleared: %v", cleared)
	time.Sleep(50 * time.Millisecond)
	return nil
}

func (h *ChatHandler) typeText(text string) error {
	runes := []rune(text)
	totalRunes := len(runes)
	for i := 0; i < totalRunes; i += maxTextChunk {
		end := min(i+maxTextChunk, totalRunes)
		chunk := string(runes[i:end])
		// 使用 json.Marshal 安全编码，避免 XSS/注入风险
		encodedChunk, err := json.Marshal(chunk)
		if err != nil {
			return fmt.Errorf("marshal text chunk: %w", err)
		}
		var chunkLen int
		err = chromedp.Run(h.session.Context(),
			chromedp.Evaluate(fmt.Sprintf(`(()=>{
				const ta = document.querySelector('textarea');
				if (!ta) return 0;
				ta.focus();
				const setter = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value').set;
				const current = ta.value;
				setter.call(ta, current + %s);
				ta.dispatchEvent(new Event('input', { bubbles: true }));
				return ta.value.length;
			})()`, string(encodedChunk)), &chunkLen),
		)
		if err != nil {
			log.Printf("[chat] type chunk error: %v", err)
		}
		time.Sleep(typeDelay)
	}
	time.Sleep(50 * time.Millisecond)
	return nil
}

func (h *ChatHandler) pressEnter() (bool, error) {
	err := chromedp.Run(h.session.Context(),
		chromedp.SendKeys("textarea", "\r", chromedp.ByQuery),
	)
	if err != nil {
		log.Printf("[chat] Enter key error: %v", err)
	}
	for i := range 10 {
		time.Sleep(100 * time.Millisecond)
		var stillInBox bool
		chromedp.Run(h.session.Context(),
			chromedp.Evaluate(`(()=>{
				const ta = document.querySelector('textarea');
				return ta && ta.value && ta.value.trim().length > 0;
			})()`, &stillInBox),
		)
		if !stillInBox {
			log.Printf("[chat] pressEnter cleared after %d checks", i+1)
			return false, nil
		}
	}
	// Enter 无效，尝试 Ctrl+Enter（DeepSeek 某些模式使用组合键发送）
	log.Println("[chat] pressEnter: Enter failed, trying Ctrl+Enter...")
	chromedp.Run(h.session.Context(),
		chromedp.Evaluate(`(()=>{
			const ta = document.querySelector('textarea');
			if (!ta) return;
			ta.focus();
			ta.dispatchEvent(new KeyboardEvent('keydown', {key:'Enter', code:'Enter', ctrlKey:true, bubbles:true}));
			ta.dispatchEvent(new KeyboardEvent('keyup', {key:'Enter', code:'Enter', ctrlKey:true, bubbles:true}));
		})()`, nil),
	)
	time.Sleep(300 * time.Millisecond)

	var stillInBox bool
	chromedp.Run(h.session.Context(),
		chromedp.Evaluate(`(()=>{
			const ta = document.querySelector('textarea');
			return ta && ta.value && ta.value.trim().length > 0;
		})()`, &stillInBox),
	)
	if !stillInBox {
		log.Println("[chat] pressEnter: Ctrl+Enter succeeded")
		return false, nil
	}

	var taValue string
	chromedp.Run(h.session.Context(),
		chromedp.Evaluate(`(document.querySelector('textarea')||{}).value||''`, &taValue),
	)

	// 页面状态诊断
	var pageDiag string
	chromedp.Run(h.session.Context(),
		chromedp.Evaluate(`(()=>{
			const ta = document.querySelector('textarea');
			const allBtns = document.querySelectorAll('button, [role="button"]');
			let btnInfo = [];
			for (const b of allBtns) {
				const r = b.getBoundingClientRect();
				if (r.width > 0 && r.height > 0 && Math.abs(r.y + r.height/2 - (ta?ta.getBoundingClientRect().bottom:0)) < 100) {
					btnInfo.push({
						cls: (b.className||'').substring(0,40),
						x: Math.round(r.x + r.width/2),
						y: Math.round(r.y + r.height/2),
						text: (b.textContent||'').trim().substring(0,15)
					});
				}
			}
			return JSON.stringify({
				taValue: ta ? (ta.value||'').substring(0,30) : 'no_ta',
				taDisabled: ta ? ta.disabled : true,
				nearbyBtns: btnInfo
			});
		})()`, &pageDiag),
	)
	log.Printf("[chat] pressEnter: all methods failed, value=%q, diag=%s", taValue[:min(len(taValue), 50)], pageDiag)
	return true, nil
}

func (h *ChatHandler) ensureMessageSent() error {
	// 优先用 JS 直接查找 primary 按钮并点击（更精确）
	var jsClickedStr string
	chromedp.Run(h.session.Context(),
		chromedp.Evaluate(`(()=>{
			// 先找 ds-button--primary（DeepSeek 的发送按钮）
			let btns = document.querySelectorAll('.ds-button--primary, [class*="primary"][class*="send"], button[aria-label*="send"], button[aria-label*="发送"]');
			if (btns.length === 0) {
				// 回退：找 input 区附近最右按钮
				const ta = document.querySelector('textarea');
				if (!ta) return 'no_textarea';
				const taBottom = ta.getBoundingClientRect().bottom;
				btns = document.querySelectorAll('button, [role="button"]');
				let rightmostBtn = null, rightmostX = -1;
				for (const b of btns) {
					const r = b.getBoundingClientRect();
					if (r.width === 0 || r.height === 0) continue;
					if (Math.abs(r.y + r.height/2 - taBottom) >= 80) continue;
					if (r.x + r.width/2 > rightmostX) {
						rightmostX = r.x + r.width/2;
						rightmostBtn = b;
					}
				}
				if (rightmostBtn) {
					btns = [rightmostBtn];
				} else {
					return JSON.stringify({found:false, reason:'no_nearby_btn'});
				}
			}
			// 尝试点击找到的按钮
			for (const b of btns) {
				const r = b.getBoundingClientRect();
				if (r.width === 0 || r.height === 0) continue;
				if ((b.disabled === true || b.getAttribute('aria-disabled') === 'true')) {
					continue; // 跳过禁用按钮
				}
				b.focus();
				b.click();
				b.dispatchEvent(new MouseEvent('mousedown', {bubbles: true, cancelable: true}));
				b.dispatchEvent(new MouseEvent('mouseup', {bubbles: true, cancelable: true}));
				b.dispatchEvent(new MouseEvent('click', {bubbles: true, cancelable: true}));
				return JSON.stringify({
					found: true,
					x: Math.round(r.x + r.width/2),
					y: Math.round(r.y + r.height/2),
					tag: b.tagName,
					cls: (b.getAttribute('class')||'').substring(0,50),
					disabled: b.disabled === true || b.getAttribute('aria-disabled') === 'true'
				});
			}
			return JSON.stringify({found:false, reason:'all_btns_disabled'});
		})()`, &jsClickedStr),
	)
	if strings.Contains(jsClickedStr, `"found":true`) {
		log.Printf("[chat] ensureMessageSent: JS click result: %s", jsClickedStr)
		time.Sleep(clickDelay)
		if !h.isTextareaStillFilled() {
			log.Println("[chat] ensureMessageSent: JS direct click succeeded")
			return nil
		}
		log.Println("[chat] ensureMessageSent: JS direct click failed (textarea still filled)")
	} else {
		log.Printf("[chat] ensureMessageSent: no primary button found (%s), trying coordinate-based", jsClickedStr)
	}

	// 回退方案：坐标定位 + chromedp mouse click
	var btnPos string
	err := chromedp.Run(h.session.Context(),
		chromedp.Evaluate(`(()=>{
			const ta = document.querySelector('textarea');
			if (!ta) return 'no_textarea';
			const taBottom = ta.getBoundingClientRect().bottom;
			const allBtns = document.querySelectorAll('[role="button"], button');
			let rightmostBtn = null;
			let rightmostX = -1;
			for (const b of allBtns) {
				const r = b.getBoundingClientRect();
				if (r.width === 0 || r.height === 0) continue;
				if (Math.abs(r.y + r.height/2 - taBottom) >= 80) continue;
				const centerX = r.x + r.width / 2;
				if (centerX > rightmostX) {
					rightmostX = centerX;
					rightmostBtn = b;
				}
			}
			if (rightmostBtn) {
				const r = rightmostBtn.getBoundingClientRect();
				return JSON.stringify({
					found: true,
					x: Math.round(r.x + r.width/2),
					y: Math.round(r.y + r.height/2),
					tag: rightmostBtn.tagName,
					cls: (rightmostBtn.getAttribute('class')||'').toString().substring(0,50),
					disabled: rightmostBtn.disabled === true || rightmostBtn.getAttribute('aria-disabled') === 'true'
				});
			}
			return JSON.stringify({found: false});
		})()`, &btnPos),
	)
	if strings.Contains(btnPos, `"found":true`) && !strings.Contains(btnPos, `"disabled":true`) && err == nil {
		var pos struct {
			Found    bool `json:"found"`
			X        int  `json:"x"`
			Y        int  `json:"y"`
			Disabled bool `json:"disabled"`
		}
		json.Unmarshal([]byte(btnPos), &pos)
		log.Printf("[chat] ensureMessageSent: found button at (%d,%d), trying mouse click", pos.X, pos.Y)
		if h.tryMouseClickXY(pos.X, pos.Y) {
			log.Println("[chat] ensureMessageSent: mouse click succeeded")
			return nil
		}
		log.Println("[chat] ensureMessageSent: mouse click failed")
	} else {
		log.Printf("[chat] ensureMessageSent: no usable button (btnPos=%q, err=%v)", btnPos, err)
	}

	// 最终回退：键盘 Enter 重试
	log.Println("[chat] ensureMessageSent: trying keyboard enter fallback")
	return h.keyboardEnterFallback()
}

func (h *ChatHandler) tryJSEventClick(x, y int) bool {
	chromedp.Run(h.session.Context(),
		chromedp.Evaluate(fmt.Sprintf(`(()=>{
			const el = document.elementFromPoint(%d, %d);
			if (!el) return;
			el.focus();
			el.click();
			el.dispatchEvent(new MouseEvent('mousedown', {bubbles: true, cancelable: true}));
			el.dispatchEvent(new MouseEvent('mouseup', {bubbles: true, cancelable: true}));
			el.dispatchEvent(new MouseEvent('click', {bubbles: true, cancelable: true}));
			el.dispatchEvent(new PointerEvent('pointerdown', {bubbles: true}));
			el.dispatchEvent(new PointerEvent('pointerup', {bubbles: true}));
		})()`, x, y), nil),
	)
	time.Sleep(clickDelay)
	return !h.isTextareaStillFilled()
}

func (h *ChatHandler) tryMouseClickXY(x, y int) bool {
	chromedp.Run(h.session.Context(),
		chromedp.MouseClickXY(float64(x), float64(y)),
	)
	time.Sleep(clickDelay)
	return !h.isTextareaStillFilled()
}

func (h *ChatHandler) tryKeyboardEnterRetry() bool {
	for range 3 {
		chromedp.Run(h.session.Context(),
			chromedp.Click("textarea", chromedp.ByQuery),
			chromedp.Sleep(200*time.Millisecond),
			chromedp.SendKeys("textarea", "\n", chromedp.ByQuery),
		)
		time.Sleep(clickDelay)
		if !h.isTextareaStillFilled() {
			return true
		}
	}
	return false
}

func (h *ChatHandler) keyboardEnterFallback() error {
	// 先尝试 Enter 键发送
	chromedp.Run(h.session.Context(),
		chromedp.Click("textarea", chromedp.ByQuery),
		chromedp.Sleep(100*time.Millisecond),
		chromedp.SendKeys("textarea", "\n", chromedp.ByQuery),
	)
	time.Sleep(500 * time.Millisecond)
	if !h.isTextareaStillFilled() {
		log.Println("[chat] keyboardEnterFallback: Enter succeeded")
		return nil
	}

	// Enter 无效，尝试 Submit（触发表单提交）
	log.Println("[chat] keyboardEnterFallback: Enter failed, trying Submit...")
	chromedp.Run(h.session.Context(),
		chromedp.Submit("textarea", chromedp.ByQuery),
	)
	time.Sleep(500 * time.Millisecond)
	if !h.isTextareaStillFilled() {
		log.Println("[chat] keyboardEnterFallback: Submit succeeded")
		return nil
	}

	// 最终手段：强点点击 primary 按钮
	log.Println("[chat] keyboardEnterFallback: Submit failed, trying direct click on primary button")
	chromedp.Run(h.session.Context(),
		chromedp.Evaluate(`(()=>{
			const btn = document.querySelector('.ds-button--primary, button[class*="primary"]');
			if (btn) {
				btn.focus();
				btn.click();
				btn.dispatchEvent(new MouseEvent('mousedown', {bubbles: true, cancelable: true}));
				btn.dispatchEvent(new MouseEvent('mouseup', {bubbles: true, cancelable: true}));
				btn.dispatchEvent(new MouseEvent('click', {bubbles: true, cancelable: true}));
				return 'clicked';
			}
			return 'no_btn';
		})()`, nil),
	)
	time.Sleep(500 * time.Millisecond)
	if !h.isTextareaStillFilled() {
		log.Println("[chat] keyboardEnterFallback: direct click succeeded")
		return nil
	}

	// 最终：全部失败，返回错误
	return fmt.Errorf("all keyboard fallback methods failed")
}

func (h *ChatHandler) isTextareaStillFilled() bool {
	var stillFilled bool
	err := chromedp.Run(h.session.Context(),
		chromedp.Evaluate(`(()=>{
			const ta = document.querySelector('textarea');
			return ta && ta.value && ta.value.trim().length > 0;
		})()`, &stillFilled),
	)
	if err != nil {
		log.Printf("[chat] isTextareaStillFilled eval error: %v (assuming filled)", err)
		return true
	}
	return stillFilled
}

func (h *ChatHandler) waitForResponse(ctx context.Context, timeout time.Duration) (content string, thinking string, convLimit bool, serverBusy bool, err error) {
	deadline := time.Now().Add(timeout)
	zeroCharCount := 0
	totalPolls := 0 // 总轮询次数，不被重置，用于60秒兜底
	recoveryTriggered := false
	pollCount := 0
	for time.Now().Before(deadline) {
		pollCount++
		select {
		case <-ctx.Done():
			return "", "", false, false, ctx.Err()
		default:
		}
		var result string
		err := chromedp.Run(h.session.Context(),
			chromedp.Evaluate(`JSON.stringify({
				d: window.__dsBrowserDone || false,
				dd: window.__dsBrowserDOMDone || false,
				c: window.__dsBrowserCapture || '',
				t: window.__dsBrowserThinking || '',
				lim: window.__dsConvLimitHit || false,
				busy: window.__dsServerBusy || false,
				ptypes: window.__dsBrowserPTypes || {}
			})`, &result),
		)
		if err == nil && result != "" {
			var r struct {
				D      bool           `json:"d"`
				DD     bool           `json:"dd"`
				C      string         `json:"c"`
				T      string         `json:"t"`
				Lim    bool           `json:"lim"`
				Busy   bool           `json:"busy"`
				PTypes map[string]int `json:"ptypes"`
			}
			if json.Unmarshal([]byte(result), &r) == nil && r.C != "" && (r.D || r.DD) {
				log.Printf("[chat] captured: %d chars, thinking: %d chars (netDone=%v domDone=%v, convLimit=%v, serverBusy=%v, ptypes=%v)",
					len([]rune(r.C)), len([]rune(r.T)), r.D, r.DD, r.Lim, r.Busy, r.PTypes)
				// 保存原始 SSE 用于调试
				var rawSSE string
				chromedp.Run(h.session.Context(),
					chromedp.Evaluate(`window.__dsBrowserRawSSE||''`, &rawSSE),
				)
				if len(rawSSE) > 0 {
					os.WriteFile(filepath.Join(os.TempDir(), "ds_raw_sse.txt"), []byte(rawSSE), 0644)
				}
				c := deduplicateContent(r.C)
				t := deduplicateContent(r.T)
				// 兜底：当 content 极短而 thinking 包含大量实质内容时，
				// 说明 DeepSeek 可能把完整回复放在了 THINK fragment（结构化 JSON 等场景），
				// 此时将 thinking 合并到 content 作为最终输出
				if len([]rune(c)) < 50 && len([]rune(t)) > 200 {
					log.Printf("[chat] thinking>capture: content=%d, thinking=%d, using thinking as content", len([]rune(c)), len([]rune(t)))
					c = t
					t = ""
				}
				return c, t, r.Lim, r.Busy, nil
			}
			if !r.D && !r.DD {
				log.Printf("[chat] waiting... %d chars so far", len([]rune(r.C)))
				zeroCharCount++
				totalPolls++
				// 每 30 秒（约 100 次）输出一次页面文本用于调试，并尝试恢复
				if zeroCharCount == 100 {
					zeroCharCount = 0
					log.Printf("[debug] 30s with 0 chars, checking page state...")
					var recoveryInfo string
					chromedp.Run(h.session.Context(),
						chromedp.Evaluate(`
							(function() {
								// 用 innerHTML 搜索（比 textContent 更完整）
								var html = (document.body && document.body.innerHTML || '').substring(0, 5000);
								var busyKeywords = ['消息发送过于频繁', '发送过于频繁', '服务器繁忙', '服务繁忙', '请稍后重试'];
								var limitKeywords = ['达到对话长度上限', '请开启新对话', '对话长度上限'];
								for (var i = 0; i < busyKeywords.length; i++) {
									if (html.indexOf(busyKeywords[i]) !== -1) {
										return 'serverBusy:' + busyKeywords[i];
									}
								}
								for (var i = 0; i < limitKeywords.length; i++) {
									if (html.indexOf(limitKeywords[i]) !== -1) {
										return 'convLimit:' + limitKeywords[i];
									}
								}
								var ta = document.querySelector('textarea');
								var taEmpty = !ta || ta.value.trim() === '';
								return 'noError:taEmpty=' + taEmpty;
							})()
						`, &recoveryInfo),
					)
					log.Printf("[debug] recovery check: %s", recoveryInfo)
					if strings.HasPrefix(recoveryInfo, "serverBusy:") {
						log.Printf("[debug] found serverBusy in HTML, triggering recovery")
						return strings.TrimPrefix(recoveryInfo, "serverBusy:"), "", false, true, nil
					}
					if strings.HasPrefix(recoveryInfo, "convLimit:") {
						log.Printf("[debug] found convLimit in HTML, triggering recovery")
						return strings.TrimPrefix(recoveryInfo, "convLimit:"), "", true, false, nil
					}
					recoveryTriggered = true
				}
				// 60 秒后仍未恢复（totalPolls 约200次），自动切换账号作为最后手段
				if recoveryTriggered && totalPolls >= 200 {
					log.Printf("[debug] 60s with 0 chars, auto-switching account as last resort")
					return "", "", false, true, nil
				}
			}
		}
		// DOM 检测：每5轮（1.5秒）执行一次，减少性能开销
		if !serverBusy && !convLimit && pollCount%5 == 0 {
			var domText string
			domErr := chromedp.Run(h.session.Context(),
				chromedp.Evaluate(errorDetectJS, &domText),
			)
			if domErr == nil && domText != "" {
				parts := strings.SplitN(domText, ":", 2)
				if len(parts) == 2 {
					log.Printf("[chat] detected UI toast: %s", domText)
					if parts[0] == "convLimit" {
						return parts[1], "", true, false, nil
					}
					return parts[1], "", false, true, nil
				}
			}
		}
		time.Sleep(pollInterval)
	}
	return "", "", false, false, fmt.Errorf("timeout waiting for response after %v", timeout)
}

func deduplicateContent(content string) string {
	if content == "" {
		return content
	}
	lines := strings.Split(content, "\n")
	if len(lines) <= 1 {
		return content
	}
	deduped := lineLevelDedup(lines)
	return strings.Join(deduped, "\n")
}

func lineLevelDedup(lines []string) []string {
	if len(lines) == 0 {
		return lines
	}
	result := []string{lines[0]}
	for i := 1; i < len(lines); i++ {
		prev := result[len(result)-1]
		curr := lines[i]
		if prev == curr {
			continue
		}
		// Only replace if curr is a strict superset of prev (curr starts with prev and is longer)
		// This handles incremental SSE updates where a line grows over time
		// Also require curr to be at most 2x the length of prev to avoid merging
		// unrelated lines that happen to share a long prefix
		if len(curr) > len(prev) && len(prev) > 20 && strings.HasPrefix(curr, prev) && len(curr) <= len(prev)*2 {
			result[len(result)-1] = curr
			continue
		}
		if isMarkdownSeparator(prev) && isMarkdownSeparator(curr) {
			continue
		}
		result = append(result, curr)
	}
	return result
}

func isMarkdownSeparator(line string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return true
	}
	// 检查整行是否完全由同一分隔符字符构成（至少3个），允许前后空格
	// 例如 "---", "***", "___" 是分隔符，但 "---text" 不是
	if len(trimmed) >= 3 {
		ch := trimmed[0]
		if ch == '-' || ch == '*' || ch == '_' {
			for i := 1; i < len(trimmed); i++ {
				if trimmed[i] != ch {
					return false
				}
			}
			return true
		}
	}
	return false
}

func (h *ChatHandler) NewConversation(ctx context.Context) error {
	log.Println("[chat] starting new conversation")
	var found bool
	err := chromedp.Run(h.session.Context(),
		chromedp.Evaluate(`(()=>{
			const links = document.querySelectorAll('a[href="/"], a[href*="/chat"]');
			for (const link of links) {
				const text = (link.textContent || '').trim();
				if (text === 'New Chat' || text === '新对话' || text === '新话题') {
					link.click();
					return true;
				}
			}
			const btns = document.querySelectorAll('[class*="new"], [class*="New"], [class*="sidebar"]');
			for (const btn of btns) {
				const text = (btn.textContent || '').trim();
				if (text === 'New Chat' || text === '新对话' || text === '新话题') {
					btn.click();
					return true;
				}
			}
			return false;
		})()`, &found),
	)
	if err != nil {
		log.Printf("[chat] UI click attempt: %v", err)
	}
	if found {
		log.Println("[chat] new conversation started via UI click")
		h.waitForEmptyTextarea(3 * time.Second)
		return nil
	}
	log.Println("[chat] UI element not found, trying Ctrl+J")
	err = chromedp.Run(h.session.Context(),
		chromedp.Evaluate(`(()=>{
			document.body.dispatchEvent(new KeyboardEvent('keydown', { key: 'j', code: 'KeyJ', ctrlKey: true, metaKey: false, bubbles: true }));
			document.body.dispatchEvent(new KeyboardEvent('keyup', { key: 'j', code: 'KeyJ', ctrlKey: true, metaKey: false, bubbles: true }));
		})()`, nil),
	)
	if err != nil {
		log.Printf("[chat] Ctrl+J attempt: %v", err)
	}
	if h.waitForEmptyTextarea(3 * time.Second) {
		log.Println("[chat] new conversation confirmed (empty chat)")
		return nil
	}
	log.Println("[chat] Ctrl+J may not have worked, navigating home")
	return h.session.NavigateHome(ctx)
}

func (h *ChatHandler) waitForEmptyTextarea(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var emptyChat bool
		chromedp.Run(h.session.Context(),
			chromedp.Evaluate(`(()=>{
				const ta = document.querySelector('textarea');
				if (!ta) return false;
				const chatList = document.querySelectorAll('[class*="ds-markdown"]');
				return chatList.length === 0;
			})()`, &emptyChat),
		)
		if emptyChat {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

// ---- 辅助方法 ----

// switchMode 切换到指定模式
func (h *ChatHandler) switchMode(ctx context.Context, mode string) error {
	if mode == "text" {
		return h.switchToTextMode(ctx)
	}
	return h.switchToImageMode(ctx)
}

// uploadImageFromData 保存base64图片并上传，上传后立即清理临时文件
func (h *ChatHandler) uploadImageFromData(imageData string) error {
	filePath, err := h.saveBase64Image(imageData)
	if err != nil {
		return err
	}
	defer os.Remove(filePath)
	log.Println("[chat] uploading image...")
	if err := h.uploadImage(filePath); err != nil {
		return err
	}
	log.Println("[chat] image uploaded")
	return nil
}

// prepareForRetry 重试前的准备工作：注入拦截器、切换模式、上传图片
func (h *ChatHandler) prepareForRetry(ctx context.Context, mode string, images []string) error {
	if err := h.injectInterceptor(); err != nil {
		return fmt.Errorf("inject interceptor: %w", err)
	}
	if err := h.switchMode(ctx, mode); err != nil {
		return fmt.Errorf("switch mode: %w", err)
	}
	if mode == "image" && len(images) > 0 {
		if err := h.uploadImageFromData(images[0]); err != nil {
			return fmt.Errorf("upload image: %w", err)
		}
	}
	return nil
}

// detectImmediateError 消息发送后立即检测页面错误提示
func (h *ChatHandler) detectImmediateError() (string, string) {
	for range 15 {
		var domText string
		domErr := chromedp.Run(h.session.Context(),
			chromedp.Evaluate(errorDetectJS, &domText),
		)
		if domErr == nil && domText != "" {
			parts := strings.SplitN(domText, ":", 2)
			if len(parts) == 2 {
				return parts[0], parts[1]
			}
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	return "", ""
}

// retryWithAccountSwitch 切换账号后重新发送消息（支持多账号轮询）
func (h *ChatHandler) retryWithAccountSwitch(ctx context.Context, mode string, text string, images []string, files []string) (*ChatResponse, error) {
	accountCount := h.session.AccountCount()
	log.Printf("[chat] starting account switch retry, total accounts: %d, mode=%q, text_len=%d, images=%d, files=%d",
		accountCount, mode, len([]rune(text)), len(images), len(files))

	// 只尝试其他账号，不重复登录当前账号（accountCount-1 次）
	// 如果只有一个账号，则直接返回错误
	if accountCount <= 1 {
		log.Printf("[chat] retryWithAccountSwitch: only %d account(s), giving up", accountCount)
		return nil, fmt.Errorf("服务器繁忙，请稍后重试")
	}

	// 记录当前页面状态以便诊断
	var pageState string
	chromedp.Run(h.session.Context(),
		chromedp.Evaluate(`(()=>{
			const ta = document.querySelector('textarea');
			const articles = document.querySelectorAll('[class*="ds-markdown"]');
			const fileInput = document.querySelector('input[type="file"]');
			return JSON.stringify({
				taExists: !!ta,
				taDisabled: ta ? ta.disabled : false,
				taValue: ta ? (ta.value||'').substring(0,50) : '',
				articleCount: articles.length,
				url: window.location.href.substring(0,100),
				hasFileInput: !!fileInput
			});
		})()`, &pageState),
	)
	log.Printf("[chat] retryWithAccountSwitch: page state before retry: %s", pageState)

	for attempt := 0; attempt < accountCount-1; attempt++ {
		newEmail, switchErr := h.session.SwitchAccount()
		if switchErr != nil {
			log.Printf("[chat] switch account attempt %d failed: %v, trying next account", attempt+1, switchErr)
			// 前进索引，下次 SwitchAccount 尝试下一个账号
			h.session.advanceAccountIdx()
			continue
		}
		log.Printf("[chat] attempt %d/%d: switched to account: %s", attempt+1, accountCount-1, newEmail)

		log.Printf("[chat] attempt %d: preparing for retry (mode=%q, images=%d)", attempt+1, mode, len(images))
		if err := h.prepareForRetry(ctx, mode, images); err != nil {
			log.Printf("[chat] attempt %d prepareForRetry failed: %v", attempt+1, err)
			return &ChatResponse{Content: "切换账号后准备失败"}, err
		}
		log.Printf("[chat] attempt %d: sending messageOrUpload (text_len=%d, files=%d)", attempt+1, len([]rune(text)), len(files))
		if err := h.sendMessageOrUpload(text, files); err != nil {
			log.Printf("[chat] attempt %d sendMessageOrUpload failed: %v", attempt+1, err)
			return &ChatResponse{Content: "切换账号后重新发送失败"}, err
		}

		// 立即检测（消息发送后 1.5 秒内）
		errType, errMsg := h.detectImmediateError()
		if errType == "serverBusy" {
			log.Printf("[chat] attempt %d failed with serverBusy: %s, will try next account", attempt+1, errMsg)
			continue
		}
		if errType == "convLimit" {
			log.Printf("[chat] attempt %d hit convLimit, will retry with new conversation", attempt+1)
			return h.retryWithNewConversation(ctx, mode, text, images, files)
		}
		if errType != "" {
			log.Printf("[chat] attempt %d detected other error: %s:%s", attempt+1, errType, errMsg)
		}

		// 等待响应
		content, thinking, convLimit, serverBusy, err := h.waitForResponse(ctx, h.responseTimeout)
		if err != nil {
			log.Printf("[chat] attempt %d waitForResponse error: %v (content=%d chars)", attempt+1, err, len([]rune(content)))
			return &ChatResponse{Content: content, Thinking: thinking}, err
		}
		log.Printf("[chat] attempt %d waitForResponse: content=%d chars, thinking=%d chars, convLimit=%v, serverBusy=%v",
			attempt+1, len([]rune(content)), len([]rune(thinking)), convLimit, serverBusy)
		if serverBusy {
			log.Printf("[chat] attempt %d serverBusy from waitForResponse, will try next account", attempt+1)
			continue
		}
		if convLimit {
			log.Printf("[chat] attempt %d convLimit from waitForResponse, will retry with new conversation", attempt+1)
			return h.retryWithNewConversation(ctx, mode, text, images, files)
		}
		if hasServerBusy(content) {
			log.Printf("[chat] attempt %d hasServerBusy in content, will try next account", attempt+1)
			continue
		}
		// 成功
		log.Printf("[chat] attempt %d succeeded: content=%d chars", attempt+1, len([]rune(content)))
		return &ChatResponse{Content: content, Thinking: thinking}, nil
	}

	log.Printf("[chat] all %d accounts exhausted, giving up", accountCount)
	return nil, fmt.Errorf("所有账号都繁忙，请稍后重试")
}

// retryWithNewConversation 开启新对话后重新发送消息
func (h *ChatHandler) retryWithNewConversation(ctx context.Context, mode string, text string, images []string, files []string) (*ChatResponse, error) {
	if err := h.NewConversation(ctx); err != nil {
		log.Printf("[chat] new conversation failed: %v", err)
		return &ChatResponse{Content: "达到对话长度上限"}, nil
	}
	if err := h.prepareForRetry(ctx, mode, images); err != nil {
		return &ChatResponse{Content: "新开对话后准备失败"}, err
	}
	if err := h.sendMessageOrUpload(text, files); err != nil {
		return &ChatResponse{Content: "新开对话后重新发送失败"}, err
	}
	content, thinking, convLimit, serverBusy, err := h.waitForResponse(ctx, h.responseTimeout)
	if err != nil {
		return &ChatResponse{Content: content, Thinking: thinking}, err
	}
	// 新对话后仍然繁忙，切换账号重试
	if serverBusy || hasServerBusy(content) {
		log.Printf("[chat] new conversation still serverBusy, switching account")
		return h.retryWithAccountSwitch(ctx, mode, text, images, files)
	}
	// 新对话后仍然命中上限（极端情况），返回错误
	if convLimit || hasConvLimit(content) {
		log.Printf("[chat] new conversation still hit convLimit, giving up")
		return &ChatResponse{Content: content, Thinking: thinking}, nil
	}
	return &ChatResponse{Content: content, Thinking: thinking}, nil
}

// hasServerBusy 检查内容是否包含服务器繁忙相关提示
func hasServerBusy(content string) bool {
	return strings.Contains(content, "服务器繁忙") ||
		strings.Contains(content, "请稍后重试") ||
		strings.Contains(content, "请稍后再试") ||
		strings.Contains(content, "消息发送过于频繁") ||
		strings.Contains(content, "发送过于频繁")
}

// hasConvLimit 检查内容是否包含对话长度上限相关提示
func hasConvLimit(content string) bool {
	return strings.Contains(content, "达到对话长度上限") ||
		strings.Contains(content, "请开启新对话") ||
		strings.Contains(content, "对话长度上限")
}
