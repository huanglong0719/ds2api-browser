package browser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

const (
	clickDelay       = 300 * time.Millisecond
	typeDelay        = 50 * time.Millisecond
	enterDelay       = 200 * time.Millisecond
	previewCheck     = 300 * time.Millisecond
	// 上传后等待发送按钮恢复可用的超时上限（CDP 实测：12.3MB 文件约 1.9s、6MB 图片约 1.1s）
	sendBtnReadyTimeout = 15 * time.Second
	// 页面重载后等待 React 完全就绪的超时上限（新对话/刷新后页面刚重建，直接输入会输到"半新页面"）
	pageStableTimeout = 10 * time.Second
	modeSwitchDelay  = 800 * time.Millisecond
	newConvDelay     = 1500 * time.Millisecond
	pollInterval     = 300 * time.Millisecond
	maxTextChunk     = 3000
	requestBodyLimit = 10 << 20

	// 空闲保活：距离上次请求超过该时长时轻量唤醒页面（不重载），
	// [Fix 2026-08-10] 30 分钟太久，Chrome Memory Saver 可能已在此之前卸载页面导致唤醒失败；
	// 改为 12 分钟，跑在 Chrome 卸载判定之前
	idleRefreshInterval = 12 * time.Minute
	// 后台保活唤醒检查周期（StartIdleRefresher）
	idleRefreshCheck = 3 * time.Minute
)

// errorDetectJS 统一的 DOM 错误检测 JS
// [Fix 2026-07-31] 只检查最后一个 ds-message（即本次请求的回复），不扫描全页面，不需要 baseline
// 返回格式：空字符串=无异常, "serverBusy:关键词"=服务器繁忙, "convLimit:关键词"=对话上限, "thinking"=AI正常工作中
const errorDetectJS = `
(function() {
	var busyKeywords = ['消息发送过于频繁', '发送过于频繁', '服务器繁忙', '服务繁忙', '请稍后重试', '请稍后再试', '有消息正在生成'];
	var limitKeywords = ['达到对话长度上限', '请开启新对话', '对话长度上限'];
	// [Fix 2026-08-09] 含"正在阅读"：大文件发送后页面先显示"正在阅读"再"正在思考"，
	// 识别"正在阅读"可让大文件请求立即进入等待回复，不用白等 3 秒检测窗口
	// 注：thinking 判定已改为显式关键词检查（"正在思考"/"正在阅读"进行中 + "已思考"完成态需本轮新回复），不再用数组

	if (window.__dsServerBusy) return 'serverBusy:interceptor';
	// [Fix 2026-08-10] 漏检根因：拦截器已检测到"达到对话长度上限"并置位 __dsConvLimitHit，
	// 但 errorDetectJS 只检查了 __dsServerBusy，漏掉 convLimit，导致系统提示出现了却检测不到，
	// 请求在 waitForResponse 阶段干等 120 秒超时（实测 2026-08-10 11:53）。
	if (window.__dsConvLimitHit) return 'convLimit:interceptor';

	// [Fix 2026-08-09] 扫描 Toast/notification 形式的系统提示（"服务器繁忙"等可能以右上角弹窗出现，
	// 不在 ds-message 消息链中，且服务器繁忙时页面可能连用户消息都不显示）。
	// 因此本扫描必须在 messages 检查之前执行，否则页面无消息时会提前返回、扫不到弹窗。
	// 2026-08-07 曾确认该场景，但 2026-07-31 重构为"只查 lastMsg"时该步骤被删除；
	// 2026-08-09 CDP 实测漏检（注入 Toast 后仍返回 thinking），重新加回。
	var toastEls = document.querySelectorAll('[class*="toast"], [class*="notification"], [role="alert"]');
	for (var j = 0; j < toastEls.length; j++) {
		var ttxt = (toastEls[j].textContent || '').trim();
		if (!ttxt || ttxt.length > 200) continue; // 跳过过长内容避免误扫
		for (var k = 0; k < busyKeywords.length; k++) {
			if (ttxt.indexOf(busyKeywords[k]) !== -1) {
				window.__dsDiagLog = 'toast class=' + toastEls[j].className + ' text=' + ttxt.substring(0, 200);
				return 'serverBusy:' + busyKeywords[k];
			}
		}
		for (var k = 0; k < limitKeywords.length; k++) {
			if (ttxt.indexOf(limitKeywords[k]) !== -1) {
				window.__dsDiagLog = 'toast class=' + toastEls[j].className + ' text=' + ttxt.substring(0, 200);
				return 'convLimit:' + limitKeywords[k];
			}
		}
	}

	// 只检查最后一个 ds-message（本次请求的回复）及其相邻元素
	// [Fix 2026-07-31] 系统提示（如"消息发送过于频繁"）可能出现在 lastMsg 的下一个兄弟元素中
	// 而不是在 ds-message 内部，必须同时检查两者
	var messages = document.querySelectorAll('[class*="ds-message"]');
	if (messages.length === 0) return '';
	var lastMsg = messages[messages.length - 1];
	var text = (lastMsg.textContent || '');
	var next = lastMsg.nextElementSibling;
	if (next) {
		text += ' ' + (next.textContent || '');
	}
	if (!text.trim()) return '';

	// 检测系统提示（优先）
	for (var k = 0; k < busyKeywords.length; k++) {
		if (text.indexOf(busyKeywords[k]) !== -1) {
			window.__dsDiagLog = 'class=' + lastMsg.className + ' text=' + text.substring(0, 200);
			return 'serverBusy:' + busyKeywords[k];
		}
	}
	for (var k = 0; k < limitKeywords.length; k++) {
		if (text.indexOf(limitKeywords[k]) !== -1) {
			window.__dsDiagLog = 'class=' + lastMsg.className + ' text=' + text.substring(0, 200);
			return 'convLimit:' + limitKeywords[k];
		}
	}

	// 检测 thinking（早停信号）
	// [Fix 2026-08-11] 区分"进行中"与"完成态"：页面销毁重建/请求失败时 lastMsg 可能是
	// 上一轮旧回复（含"已思考"），此前一律判定 thinking 导致误判、系统提示漏检
	// （实测 2026-08-11 14:06:38 TARGET DESTROYED 后 14:06:40 误判 thinking 提前退出，
	// 实际请求失败、系统提示出现但未检测到，waitForResponse 干等超时）。
	// - "正在思考"/"正在阅读"：进行中状态（思考时页面实时显示），可安全早停
	// - "已思考"：完成态，旧回复也含此文字，仅当本轮有新回复（ds-markdown 数 > baseline）时才判定
	if (text.indexOf('正在思考') !== -1 || text.indexOf('正在阅读') !== -1) {
		return 'thinking';
	}
	var hasNewReply = document.querySelectorAll('[class*="ds-markdown"]').length > (window.__dsArticleBaseline || 0);
	if (hasNewReply && text.indexOf('已思考') !== -1) {
		return 'thinking';
	}

	return '';
})()
`

// dumpSystemPromptLocation 诊断用：扫描全页面找到系统提示文本的实际 DOM 位置
// 不改变检测逻辑，只在系统提示出现时记录其 tag/class/parentChain/相对ds-message位置
// 用于确定 errorDetectJS 漏检的根因
const dumpSystemPromptLocation = `
(function() {
	var keywords = ['消息发送过于频繁', '发送过于频繁', '服务器繁忙', '服务繁忙',
		'请稍后重试', '请稍后再试', '有消息正在生成',
		'达到对话长度上限', '请开启新对话', '对话长度上限'];
	var body = document.body;
	if (!body) return '';

	// 递归查找包含关键词的文本节点
	function findKeywordNodes(el, results) {
		if (!el || el === document) return;
		// 跳过 script/style 标签
		if (el.tagName === 'SCRIPT' || el.tagName === 'STYLE') return;
		for (var i = 0; i < el.childNodes.length; i++) {
			var child = el.childNodes[i];
			if (child.nodeType === 3) { // 文本节点
				var txt = child.textContent || '';
				for (var k = 0; k < keywords.length; k++) {
					if (txt.indexOf(keywords[k]) !== -1) {
						// 记录包含该关键词的元素信息
						var info = {
							keyword: keywords[k],
							tag: el.tagName,
							className: (el.className || '').substring(0, 100),
							text: txt.substring(0, 200),
							parentTag: el.parentElement ? el.parentElement.tagName : 'null',
							parentClass: el.parentElement ? (el.parentElement.className || '').substring(0, 100) : 'null',
							isDsMessage: !!(el.className && el.className.indexOf('ds-message') !== -1),
							// 检查与 ds-message 的关系
							closestDsMsg: (function(){
								var p = el;
								while (p) {
									if (p.className && p.className.indexOf('ds-message') !== -1) break;
									p = p.parentElement;
								}
								return p ? p.className.substring(0, 80) : 'null';
							})(),
							// 检查是否是 ds-message 的兄弟元素
							prevSiblingTag: el.previousElementSibling ? el.previousElementSibling.tagName : 'null',
							prevSiblingClass: el.previousElementSibling ? (el.previousElementSibling.className || '').substring(0, 80) : 'null',
							nextSiblingTag: el.nextElementSibling ? el.nextElementSibling.tagName : 'null',
							nextSiblingClass: el.nextElementSibling ? (el.nextElementSibling.className || '').substring(0, 80) : 'null'
						};
						results.push(info);
						return; // 每个元素只记录一次
					}
				}
			} else if (child.nodeType === 1) {
				findKeywordNodes(child, results);
			}
		}
	}

	var results = [];
	findKeywordNodes(body, results);
	return JSON.stringify(results);
})()
`

var observeCaptureScript = `
(() => {
	window.__dsBrowserDOMDone = false;
	window.__dsObserveActive = true;
	if (window.__dsObserveInterval) {
		clearInterval(window.__dsObserveInterval);
	}

	function scan() {
		// [Fix 2026-07-31] 5个操作按钮不在 ds-message 内部，在其父级容器中
		// 依据：DOM_STRUCTURE.md 第三章——按钮在 ds-message 的兄弟元素中
		// 每个按钮由3个div组成（本体+背景+图标），5个按钮×3=15个 [class*="ds-button"]
		// 必须确认 lastMsg 是 AI 回复（含 ds-assistant-message-main-content），
		// 避免用户消息（d29f3d7d）的父级容器误判（上一轮回复的按钮还在）
		//
		// [Fix 2026-07-31] 增加 baseline 检查，避免上一轮回复的按钮被误判为本轮完成
		// 场景：新请求发送后，上一轮 AI 回复的 15 个按钮还在页面上，scan 会立即设 domDone=true
		// 但此时本轮 AI 还没开始回复（capture=""），会触发"DOM已完成但拦截器空"错误分支
		// 解决：只有当本轮有新的 ds-markdown 出现（数量 > baseline）时，才扫描按钮
		const articles = document.querySelectorAll('[class*="ds-markdown"]');
		if (articles.length <= (window.__dsArticleBaseline || 0)) return;
		const messages = document.querySelectorAll('[class*="ds-message"]');
		if (!messages.length) return;
		const lastMsg = messages[messages.length - 1];
		// 必须是 AI 回复，不是用户消息
		if (!lastMsg.querySelector('[class*="ds-assistant-message-main-content"]')) return;
		// 按钮在父级容器中（不在 ds-message 内部）
		const parent = lastMsg.parentElement;
		if (!parent) return;
		const btns = parent.querySelectorAll('[class*="ds-button"]');
		if (btns.length >= 15) {
			window.__dsBrowserDOMDone = true;
			return;
		}
	}
	window.__dsObserveInterval = setInterval(scan, 500);
	scan();
})();
`

type ChatHandler struct {
	session          *Session
	mu               sync.Mutex
	responseTimeout  time.Duration
	lastActivity     atomic.Int64
	lastRefresh      atomic.Int64 // 最近一次空闲保活刷新时间（纳秒时间戳，0=从未刷新）
	convMsgCount     atomic.Int64 // 当前对话中已发送的消息数
	convMsgThreshold atomic.Int64 // 触发新对话的随机阈值（30-60）
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
	h := &ChatHandler{session: session, responseTimeout: time.Duration(timeoutSec) * time.Second}
	// 随机生成60万-90万字符的阈值（约20-30条超长消息），避免固定间隔被检测
	h.convMsgThreshold.Store(int64(600000 + rand.Intn(300001)))
	return h
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
	// 模式选择：有图片走识图模式（需要 OCR），纯文件走快速模式（文本处理）
	mode := "image"
	if len(req.Images) == 0 && len(req.Files) > 0 {
		mode = "text"
		log.Printf("[chat] file-only request, using text mode (fast) instead of image mode")
	}
	return h.sendChat(ctx, mode, req.Text, req.Images, req.Files, shouldNewConv)
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

	// 客户端传的文件（路径已在 handler 中生成）
	var allFiles []string
	for _, f := range files {
		allFiles = append(allFiles, f)
	}

	// 上传文件
	if len(allFiles) > 0 {
		log.Printf("[chat] uploading %d file(s) to DeepSeek", len(allFiles))
		if err := chromedp.Run(h.session.Context(),
			chromedp.SetUploadFiles(`input[type="file"]`, allFiles, chromedp.ByQuery),
		); err != nil {
			return nil, fmt.Errorf("upload files: %w", err)
		}
		log.Println("[chat] files uploaded, waiting for send button ready...")
		// [Fix 2026-08-09] 等发送按钮恢复可用（替代固定 1 秒等待）。
		// CDP 实测：上传中按钮禁用（ds-button--disabled），上传完成立即恢复可用。
		// 大文件不会被提前发送；小文件不用白等。超时只告警不中断
		if !h.waitSendBtnReady(sendBtnReadyTimeout) {
			log.Printf("[chat] warn: send button not ready within %v after file upload", sendBtnReadyTimeout)
		}
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

		// 输入提示文字并发送
		prompt := "请处理附件中的内容"
		if err := h.clearTextarea(); err != nil {
			return nil, fmt.Errorf("clear textarea: %w", err)
		}
		if err := h.typeText(prompt); err != nil {
			return nil, fmt.Errorf("type prompt: %w", err)
		}
		// 模拟人类输入完成后的随机停顿
		time.Sleep(time.Duration(500+rand.Intn(1500)) * time.Millisecond)
		stillInBox, err := h.pressEnter()
		if err != nil {
			return nil, fmt.Errorf("press enter: %w", err)
		}
		if stillInBox {
			// 发送可能被页面错误遮挡，先检查错误再做兜底点击
			errType, errMsg := h.detectImmediateError()
			if errType == "serverBusy" {
				logDiagInfo(h, errType, errMsg)
				log.Printf("[chat] detected %s after file upload: %s", errType, errMsg)
				return h.retryWithAccountSwitch(ctx, mode, text, images, files)
			}
			if errType == "convLimit" {
				logDiagInfo(h, errType, errMsg)
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
			// [Fix 2026-08-09] NavigateHome 后页面销毁重建，拦截器丢失且 React 未就绪，
			// 重新注入拦截器（内部会先等页面稳定），页面稳定检测覆盖此重试场景
			if err := h.injectInterceptor(); err != nil {
				return nil, fmt.Errorf("inject interceptor after navigate home: %w", err)
			}
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
		logDiagInfo(h, errType, errMsg)
		log.Printf("[chat] detected %s immediately after send: %s", errType, errMsg)
		if errType == "serverBusy" {
			return h.retryWithAccountSwitch(ctx, mode, text, images, files)
		}
		return h.retryWithNewConversation(ctx, mode, text, images, files)
	}
	step("immediateErrorDetection")

	content, thinking, convLimit, serverBusy, err := h.waitForResponse(ctx, h.responseTimeout, text)
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

	// 拦截器内容为空时，尝试复制按钮兜底
	if content == "" {
		log.Printf("[chat] interceptor returned empty content, trying copy button fallback")
		if copyContent, err := h.fetchContentViaCopyButton(); err == nil && copyContent != "" {
			content = copyContent
			log.Printf("[chat] copy button fallback: replaced content with %d chars", len([]rune(content)))
		} else {
			log.Printf("[chat] copy button fallback failed: %v", err)
		}
	}

	log.Printf("[chat] got response: %d chars, thinking: %d chars", len([]rune(content)), len([]rune(thinking)))
	// [账号质量管理] 主路径首次成功才计入会话统计；重试路径成功不计数（避免同一请求重复计数）
	// [Fix 2026-08-11] 对话累计按"字符数"累加（此前误加 1 条/次，而阈值是 60-90 万字符，
	// count 永远到不了 threshold，ShouldNewConversationByCount 永不触发、从不主动开新对话，
	// 连续对话无限累加直到到达服务器对话长度上限）。
	// [Fix 2026-08-11 用户确认] 服务器对话长度上限按"完整上下文"计算（提示词+思考+回复），
	// 必须累加三者字符数之和（约 3.4 万/轮），主动检查才能真正反映服务器消耗、
	// 在到达上限前主动开新对话。此前只累加回复字符数（约 0.15 万/轮）会慢 20 倍以上，
	// 主动检查永远追不上服务器上限。
	if content != "" {
		h.session.sessionRequestCount++
		h.convMsgCount.Add(int64(len([]rune(text)) + len([]rune(thinking)) + len([]rune(content))))
		// [诊断日志 2026-08-11] 验证主动新对话统计是否生效：每次成功回复后打印当前累计值
		log.Printf("[chat][diag] convMsgCount now=%d chars (added text=%d think=%d content=%d, threshold=%d)",
			h.convMsgCount.Load(), len([]rune(text)), len([]rune(thinking)), len([]rune(content)), h.convMsgThreshold.Load())
	}
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

// ShouldNewConversationByCount 检查当前对话累计字符数是否达到随机阈值
// [Fix 2026-08-10] 由"消息条数30-60"改为"累计字符数60万-90万"：
// 每条消息约3万字符，按条数30-60统计时会远滞后于服务器对话长度上限（实测约50条即触发），
// 改为按字符数统计后约20-30条超长消息就会主动开新对话，从源头避免到达服务器上限
func (h *ChatHandler) ShouldNewConversationByCount() bool {
	count := h.convMsgCount.Load()
	threshold := h.convMsgThreshold.Load()
	if threshold == 0 {
		return false
	}
	return count >= threshold
}

func (h *ChatHandler) ensureReady() error {
	if !h.session.loggedIn.Load() {
		return fmt.Errorf("not logged in")
	}
	// 每次发送前清理多余标签页，确保只有一个 DeepSeek 页
	h.session.closeExtraTargets()

	// 长时间空闲后（如收盘到复盘之间数小时），Chrome 渲染进程可能进入睡眠状态，
	// 导致 chromedp.Evaluate 调用耗时极长甚至挂起。轻量唤醒页面（不重载），
	// 并带超时确认唤醒成功，避免后续流程无限等待。
	// 只在距离上次请求超过 idleRefreshInterval（且距离上次唤醒超过 idleRefreshInterval）
	// 时才唤醒，避免频繁请求时产生额外开销。后台保活 goroutine（StartIdleRefresher）也会执行
	// 同样的唤醒，保证长时间无请求时页面保持活跃，请求到来时无需现场唤醒。
	if h.shouldRefreshPage() {
		if err := h.refreshPageLocked(); err != nil {
			// [Fix 2026-08-10] 唤醒失败 = 页面被 Memory Saver 卸载，重建页面恢复服务
			log.Printf("[chat] wake failed before send, rebuilding page: %v", err)
			if rbErr := h.session.RebuildPage(); rbErr != nil {
				return fmt.Errorf("rebuild page after wake failure: %w", rbErr)
			}
			h.lastRefresh.Store(time.Now().UnixNano())
		}
	}

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

// idleDuration 返回距离上次活动的时间，用于判断是否需要做空闲恢复检查
func (h *ChatHandler) idleDuration() time.Duration {
	ns := h.lastActivity.Load()
	if ns == 0 {
		return time.Hour // 首次启动，视为长时间空闲
	}
	return time.Since(time.Unix(0, ns))
}

// shouldRefreshPage 判断是否需要进行空闲保活刷新：
//   - 距离上次请求超过 idleRefreshInterval（页面可能已进入休眠/被节流）
//   - 且距离上次刷新超过 idleRefreshInterval（保持"每 30 分钟最多刷一次"，避免重复刷新）
//   - 且已登录（登录/账号切换过程中不刷新，避免干扰切换流程）
func (h *ChatHandler) shouldRefreshPage() bool {
	if !h.session.loggedIn.Load() {
		return false
	}
	if h.idleDuration() <= idleRefreshInterval {
		return false
	}
	last := h.lastRefresh.Load()
	return last == 0 || time.Since(time.Unix(0, last)) > idleRefreshInterval
}

// refreshPageLocked 执行空闲保活刷新：导航回 DeepSeek 首页唤醒渲染进程，
// 并带超时确认页面恢复响应。调用方必须持有 h.mu（与 sendChat 互斥）。
// 刷新成功后记录 lastRefresh，避免在 30 分钟内重复刷新。
// refreshPageLocked 执行空闲保活：轻量唤醒页面，不重载。
// [Fix 2026-08-10] 原实现为整页导航重载：30 分钟太久页面已被 Chrome Memory Saver 卸载
// （渲染进程销毁），导航命令发给已卸载的标签页会永久挂起、锁被占死导致程序假死。
// 改为短间隔轻量唤醒（setWebLifecycleState active + bringToFront，毫秒级），
// 让 Chrome 始终认为页面活跃、永不触发卸载；若唤醒命令超时说明页面已死，返回错误触发重建。
// 调用方必须持有 h.mu（与 sendChat 互斥）。
// 唤醒成功后记录 lastRefresh，避免在 idleRefreshInterval 内重复唤醒。
func (h *ChatHandler) refreshPageLocked() error {
	log.Printf("[chat] idle > %v, waking page (lightweight, no reload)", idleRefreshInterval)
	wakeCtx, cancelWake := context.WithTimeout(h.session.Context(), 3*time.Second)
	defer cancelWake()
	// [Fix 2026-08-10] 看门狗：chromedp 的 3 秒超时在浏览器"半死"（页面被 Memory Saver 冻结、
	// TCP 连接仍在）时可能失效（实测 22:33-23:09 卡 36 分钟），导致 h.mu 被无限期占用、
	// 所有请求阻塞（"脚本自己死了"）。改为独立 goroutine 执行 + time.After 看门狗：
	// 5 秒未完成即强制断开浏览器连接，让卡住的命令返回、锁立即释放；
	// 后续请求通过 Session.Context() 检测到连接断开自动重启浏览器恢复。
	errCh := make(chan error, 1)
	go func() {
		errCh <- chromedp.Run(wakeCtx,
			page.SetWebLifecycleState(page.SetWebLifecycleStateStateActive),
			page.BringToFront(),
		)
	}()
	select {
	case err := <-errCh:
		if err != nil {
			log.Printf("[chat] idle wake failed (page unloaded by Memory Saver?): %v", err)
			return fmt.Errorf("idle wake: %w", err)
		}
	case <-time.After(5 * time.Second):
		// chromedp 超时失效，命令挂起：强制断开连接解除阻塞
		log.Printf("[chat] idle wake HUNG for 5s, aborting browser connection")
		h.session.AbortConnection()
		return fmt.Errorf("idle wake: hung, connection aborted")
	}
	log.Printf("[chat] page woken (lightweight, no reload)")
	h.session.checkToggleStates()
	h.lastRefresh.Store(time.Now().UnixNano())
	return nil
}

// StartIdleRefresher 启动后台空闲保活唤醒 goroutine。
// 每 idleRefreshCheck 检查一次：距离上次请求超过 idleRefreshInterval 且距离上次唤醒
// 超过 idleRefreshInterval 时，轻量唤醒页面（setWebLifecycleState active + bringToFront，
// 毫秒级，不重载）——让 Chrome 始终认为页面活跃，永不触发 Memory Saver 卸载。
// [Fix 2026-08-10] 唤醒失败（3 秒超时）说明页面已被 Memory Saver 卸载（渲染进程销毁），
// 此时导航/唤醒命令都会挂起，改为自动重建页面（关闭旧标签页→新建→等就绪）。
// 与 sendChat 共用 h.mu：活跃请求期间拿不到锁会跳过，绝不打断进行中的对话；
// 登录/账号切换期间（loggedIn=false）也会跳过。
// goroutine 随 session 关闭（browserCtx 取消）自动退出。
func (h *ChatHandler) StartIdleRefresher() {
	if h.session == nil {
		return
	}
	sessCtx := h.session.Context()
	go func() {
		ticker := time.NewTicker(idleRefreshCheck)
		defer ticker.Stop()
		for {
			select {
			case <-sessCtx.Done():
				log.Println("[chat] idle refresher stopped")
				return
			case <-ticker.C:
				if !h.shouldRefreshPage() {
					continue
				}
				h.mu.Lock()
				// 拿锁后重新检查：等待锁期间可能有请求完成并更新了 lastActivity
				if h.shouldRefreshPage() {
					if err := h.refreshPageLocked(); err != nil {
						// [Fix 2026-08-10] 唤醒失败 = 页面被 Memory Saver 卸载，
						// 重建页面恢复服务（登录态保留在 profile，重建后仍登录）
						log.Printf("[chat] idle wake failed, rebuilding page: %v", err)
						if rbErr := h.session.RebuildPage(); rbErr != nil {
							log.Printf("[chat] page rebuild failed: %v", rbErr)
						} else {
							h.lastRefresh.Store(time.Now().UnixNano())
						}
					}
				}
				h.mu.Unlock()
			}
		}
	}()
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
// 增强：增加 toggle button / button group / aria-pressed 选择器回退
// 注意：要排除"深度思考"和"智能搜索"toggle，它们不是文本/识图模式切换
const getCurrentModeJS = `(()=>{
	// 模式关键词（用于识别文本/识图模式切换控件）
	const modeKeywords = ['识图', '默认', '快速'];
	// 排除关键词（这些是 toggle，不是模式切换）
	const excludeKeywords = ['深度思考', '智能搜索', '联网搜索'];

	function isModeControl(text) {
		if (!text) return false;
		for (const ex of excludeKeywords) {
			if (text.indexOf(ex) !== -1) return false;
		}
		for (const kw of modeKeywords) {
			if (text.indexOf(kw) !== -1) return true;
		}
		return false;
	}

	function getDirectText(el) {
		let txt = '';
		for (const n of el.childNodes) {
			if (n.nodeType === 3) txt += n.textContent;
		}
		return txt.trim();
	}

	function normalizeModeText(txt) {
		if (!txt) return '';
		return txt.replace(/(识图模式)+/g, '识图模式').replace(/(默认模式)+/g, '默认模式').replace(/(快速模式)+/g, '快速模式');
	}

	// 1. 优先查 [role="radiogroup"] [role="radio"]
	let radios = document.querySelectorAll('[role="radiogroup"] [role="radio"]');
	if (radios.length === 0) {
		radios = document.querySelectorAll('[role="radio"]');
	}
	for (const r of radios) {
		if (r.getAttribute('aria-checked') === 'true') {
			let txt = getDirectText(r) || normalizeModeText((r.textContent || '').trim());
			if (isModeControl(txt)) return txt;
		}
	}

	// 2. 回退：查找 button / div.ds-toggle-button / [aria-pressed] 中含模式关键词的元素
	const toggleCandidates = document.querySelectorAll(
		'button, [role="button"], div.ds-toggle-button, [aria-pressed], [class*="mode"], [data-mode]'
	);
	for (const el of toggleCandidates) {
		let txt = getDirectText(el) || (el.textContent || '').trim();
		if (!isModeControl(txt)) continue;
		// 检查是否"选中"：aria-checked, aria-pressed, class 含 selected/active
		const isSelected =
			el.getAttribute('aria-checked') === 'true' ||
			el.getAttribute('aria-pressed') === 'true' ||
			el.getAttribute('data-state') === 'active' ||
			(el.className || '').includes('--selected') ||
			(el.className || '').includes('--active') ||
			(el.className || '').includes('active');
		if (isSelected) {
			return normalizeModeText(txt);
		}
	}

	// 3. 回退：检查是否有实际已上传的图片预览（对话进行中 radio 可能被隐藏）
	// 注意：input[type="file"] 在所有模式中都存在（附件按钮），不能作为识图模式的依据
	const previewImgs = document.querySelectorAll('img[src*="blob:"], img[src*="data:"]');
	if (previewImgs.length > 0) {
		return '识图模式';
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

// checkSendBtnReadyJS 判断发送按钮是否可用（CDP 实测 2026-08-09）：
// 发送按钮为 div.ds-button--primary，上传文件/图片过程中 className 含 ds-button--disabled（禁用），
// 上传完成后立即移除（可用）。aria-disabled / disabled 属性实测全程无变化，不可用作判断信号。
const checkSendBtnReadyJS = `(()=>{
	const btns = document.querySelectorAll('div.ds-button--primary');
	for (const b of btns) {
		const r = b.getBoundingClientRect();
		if (r.width === 0 && r.height === 0) continue;
		if (!(b.className || '').includes('ds-button--disabled')) return true;
	}
	return false;
})()`

// waitSendBtnReady 等待发送按钮恢复可用（上传完成信号）。
// 返回 true=按钮已可用；超时返回 false（只告警不中断，走原有流程）。
func (h *ChatHandler) waitSendBtnReady(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var ready bool
		if err := chromedp.Run(h.session.Context(),
			chromedp.Evaluate(checkSendBtnReadyJS, &ready),
		); err == nil && ready {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
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
	// [Fix 2026-08-09] 等发送按钮恢复可用（CDP 实测：上传完成按钮立即从禁用变可用，
	// 比固定等待更准确——大文件不会提前，小文件不会白等）。超时只告警不中断
	if !h.waitSendBtnReady(sendBtnReadyTimeout) {
		log.Printf("[chat] warn: send button not ready within %v after image upload", sendBtnReadyTimeout)
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
		// [Fix 2026-08-09] 页面刚重载过（拦截器丢失 = 页面销毁重建过），
		// 先等新页面 React 完全就绪再注入拦截器并继续。原因：新对话/刷新/重启后页面刚重建，
		// React 事件监听尚未挂载完成，此时直接输入长文本再按 Enter 会失效
		// （实测：20:03/20:37 两次失败均伴随 TARGET DESTROYED + interceptor 重新注入）。
		// 统一在此等待，覆盖所有调用 injectInterceptor 的场景（sendChat/prepareForRetry）。
		if !h.waitForPageStable(pageStableTimeout) {
			log.Printf("[chat] warn: page not stable within %v after reload, continuing anyway", pageStableTimeout)
		}
		err := chromedp.Run(h.session.Context(),
			chromedp.Evaluate(InjectScript, nil),
			chromedp.Evaluate(observeCaptureScript, nil),
		)
		if err != nil {
			return err
		}
		log.Println("[chat] interceptor injected (page reloaded or first time)")
	}

	// 基于网页状态等待旧 SSE 流完成（替代固定 300ms 等待）
	// 场景：NewConversation 后，DeepSeek 可能取消当前回复，但旧 SSE 流的残留数据可能还在推送
	// 检测：如果 __dsBrowserDone 为 false 且 capture 非空，说明有进行中的回复
	// 等待 done=true 或 capture 为空，超时 10 秒兜底（应对网络异常）
	if err := h.waitForOldStreamDone(10 * time.Second); err != nil {
		log.Printf("[chat] wait for old stream done: %v (continuing)", err)
	}

	// 重置所有捕获变量 + 重新注入 DOM 观察器（清除旧 setInterval，重置 __dsBrowserDOMDone）
	// [Fix 2026-07-31] 同步设置 __dsArticleBaseline 为当前 article 数
	// 原因：scan 函数在 observeCaptureScript 注入后立即执行，如果此时 __dsArticleBaseline 还是上一轮的值，
	// 上一轮回复后的 article 数 > 旧 baseline → scan 会继续扫描按钮 → 找到上一轮的15个按钮 → 误设 domDone=true
	// 解决：在重置 domDone 的同时把 baseline 设为当前 article 数，scan 第一次执行时就会因 articles.length <= baseline 而跳过
	if err := chromedp.Run(h.session.Context(),
		chromedp.Evaluate(`(()=>{
			window.__dsBrowserCapture = '';
			window.__dsBrowserThinking = '';
			window.__dsBrowserDone = false;
			window.__dsBrowserDOMDone = false;
			window.__dsCurrentFragmentType = '';
			window.__dsConvLimitHit = false;
			window.__dsServerBusy = false;
			window.__dsArticleBaseline = document.querySelectorAll('[class*="ds-markdown"]').length;
		})()`, nil),
		chromedp.Evaluate(observeCaptureScript, nil),
	); err != nil {
		return err
	}

	// 等待 capture 内容稳定（连续 2 次轮询不变），确保旧 SSE 流的残留数据全部到达
	// 这替代了原来的固定等待，基于实际状态检测
	h.waitForCaptureStable(2 * time.Second)

	// 第二次重置 capture 和 thinking，清空旧 SSE 流推送的残留数据
	return chromedp.Run(h.session.Context(),
		chromedp.Evaluate(`window.__dsBrowserCapture=''; window.__dsBrowserThinking='';`, nil),
	)
}

// waitForPageStable 页面销毁重建后，等待新页面 React 完全就绪（输入框可正常交互）。
// 检测信号：textarea 可见 + placeholder 已渲染（React 已接管输入区）+ 连续多次稳定。
// 超时返回 false，由调用方决定是否继续。
// 背景（2026-08-09）：新对话/刷新后页面刚重建，直接输入长文本再按 Enter 会失效，
// 因为 React 事件监听尚未挂载完成，DispatchKeyEvent 虽到达输入框但不触发发送。
func (h *ChatHandler) waitForPageStable(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	lastState := ""
	stableCount := 0
	for time.Now().Before(deadline) {
		var state string
		err := chromedp.Run(h.session.Context(),
			chromedp.Evaluate(`(()=>{
				const ta = document.querySelector('textarea');
				if (!ta) return 'no_ta';
				const r = ta.getBoundingClientRect();
				if (r.width === 0 || r.height === 0) return 'hidden';
				if (document.readyState !== 'complete') return 'loading';
				if (!ta.placeholder) return 'no_ph';
				return 'ready';
			})()`, &state),
		)
		if err != nil {
			lastState = "err"
			stableCount = 0
			time.Sleep(200 * time.Millisecond)
			continue
		}
		if state == "ready" && lastState == "ready" {
			stableCount++
			if stableCount >= 3 {
				log.Println("[chat] page stable after reload (React ready)")
				return true
			}
		} else {
			stableCount = 0
		}
		lastState = state
		time.Sleep(200 * time.Millisecond)
	}
	log.Printf("[chat] page stability check timed out after %v (last=%s)", timeout, lastState)
	return false
}

// waitForOldStreamDone 等待旧 SSE 流完成（基于拦截器 done 标志和 capture 状态）
// 当 done=true 或 capture 为空时返回；超时返回错误
func (h *ChatHandler) waitForOldStreamDone(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var state string
		err := chromedp.Run(h.session.Context(),
			chromedp.Evaluate(`JSON.stringify({
				done: window.__dsBrowserDone || false,
				capture: window.__dsBrowserCapture || ''
			})`, &state),
		)
		if err != nil {
			return err
		}
		var s struct {
			Done    bool   `json:"done"`
			Capture string `json:"capture"`
		}
		if json.Unmarshal([]byte(state), &s) == nil {
			// done=true 说明旧流已完成；capture 为空说明没有进行中的回复
			if s.Done || s.Capture == "" {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for old stream to finish after %v", timeout)
}

// waitForCaptureStable 等待 capture 内容稳定（连续 N 次轮询不变）或超时
// 用于确保旧 SSE 流的残留数据全部到达后再重置
func (h *ChatHandler) waitForCaptureStable(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	var lastCapture string
	stableCount := 0
	for time.Now().Before(deadline) {
		var capture string
		err := chromedp.Run(h.session.Context(),
			chromedp.Evaluate(`window.__dsBrowserCapture || ''`, &capture),
		)
		if err != nil {
			break
		}
		if capture == lastCapture {
			stableCount++
			if stableCount >= 2 {
				return
			}
		} else {
			stableCount = 0
			lastCapture = capture
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (h *ChatHandler) sendMessage(text string) error {
	// 记录发送前的文章数，用于后续定位最新回复的复制按钮
	chromedp.Run(h.session.Context(),
		chromedp.Evaluate(`window.__dsArticleBaseline = document.querySelectorAll('[class*="ds-markdown"]').length`, nil),
	)

	log.Printf("[chat] preparing to type %d chars", len([]rune(text)))
	if err := h.clearTextarea(); err != nil {
		return fmt.Errorf("clear textarea: %w", err)
	}
	if err := h.typeText(text); err != nil {
		return fmt.Errorf("type text: %w", err)
	}
	// 模拟人类输入完成后的随机停顿（500-2000ms），给 React 时间处理输入事件，让发送按钮变为可用状态
	humanDelay := time.Duration(500+rand.Intn(1500)) * time.Millisecond
	time.Sleep(humanDelay)
	stillInBox, err := h.pressEnter()
	if err != nil {
		return fmt.Errorf("press enter: %w", err)
	}
	if stillInBox {
		return h.ensureMessageSent()
	}
	return nil
}

// sendMessageOrUpload 发送消息，支持已有附件（供重试路径使用）
func (h *ChatHandler) sendMessageOrUpload(text string, files []string) error {
	// 记录发送前的文章数，用于后续定位最新回复的复制按钮
	chromedp.Run(h.session.Context(),
		chromedp.Evaluate(`window.__dsArticleBaseline = document.querySelectorAll('[class*="ds-markdown"]').length`, nil),
	)

	// 有附件需要上传时，统一 SetUploadFiles 后再发送提示文字
	if len(files) > 0 {
		log.Printf("[chat] retry uploading %d file(s)", len(files))
		if err := chromedp.Run(h.session.Context(),
			chromedp.SetUploadFiles(`input[type="file"]`, files, chromedp.ByQuery),
		); err != nil {
			return fmt.Errorf("upload files: %w", err)
		}
		time.Sleep(1 * time.Second)

		prompt := "请处理附件中的内容"
		if err := h.clearTextarea(); err != nil {
			return fmt.Errorf("clear textarea: %w", err)
		}
		if err := h.typeText(prompt); err != nil {
			return fmt.Errorf("type prompt: %w", err)
		}
		// 模拟人类输入完成后的随机停顿
		time.Sleep(time.Duration(500+rand.Intn(1500)) * time.Millisecond)
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
			// [Fix 2026-08-08] 输入失败必须立即返回错误，不能继续输入后续分块。
			// 否则会在连接断开/页面失效的状态下继续"假装成功"输入，导致消息实际未发送，
			// 程序却进入等待回复的死循环。返回错误后由 sendChat 走 NavigateHome+重试兜底。
			log.Printf("[chat] type chunk error: %v", err)
			return fmt.Errorf("type chunk: %w", err)
		}
		time.Sleep(typeDelay)
	}
	time.Sleep(50 * time.Millisecond)
	return nil
}

func (h *ChatHandler) pressEnter() (bool, error) {
	// [Fix 2026-08-08] 不再用 chromedp.SendKeys("\r") 发送 Enter：
	// chromedp 键盘编码表对 '\r' 会生成 keyDown + char(text="\r") + keyUp 三个事件，
	// 其中 char 事件会把 "\r" 当作文本插入 textarea（变成换行符 \n），
	// 导致：1) textarea 残留 \n 让 isTextareaStillFilled 误判发送失败；2) 按钮因此被判定不可用。
	// 改为手动发送 keyDown + keyUp（无 char 事件），Enter 一次即可发送成功。
	// 依据：CDP 实验验证——带 char 事件 100% 残留 \n 失败；仅 keyDown+keyUp 100% 成功。
	err := chromedp.Run(h.session.Context(),
		chromedp.ActionFunc(func(ctx context.Context) error {
			// 与 SendKeys 一致：先聚焦 textarea，确保 Enter 事件送达输入框
			if err := chromedp.Evaluate(`(()=>{const ta=document.querySelector('textarea');if(ta)ta.focus();})()`, nil).Do(ctx); err != nil {
				log.Printf("[chat] pressEnter focus error: %v", err)
			}
			if err := input.DispatchKeyEvent(input.KeyDown).
				WithKey("Enter").WithCode("Enter").
				WithWindowsVirtualKeyCode(13).WithNativeVirtualKeyCode(13).Do(ctx); err != nil {
				return err
			}
			return input.DispatchKeyEvent(input.KeyUp).
				WithKey("Enter").WithCode("Enter").
				WithWindowsVirtualKeyCode(13).WithNativeVirtualKeyCode(13).Do(ctx)
		}),
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
				return !!(ta && ta.value && ta.value.trim().length > 0);
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
			return !!(ta && ta.value && ta.value.trim().length > 0);
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
	// 注意：DeepSeek 的 React 按钮对 JS .click() 无效（参见 session.go checkToggleStates 注释）
	// 必须用 chromedp.MouseClickXY 真实点击。JS 仅用于定位按钮坐标。
	// 第一步：用 JS 查找发送按钮并获取坐标（不点击，只定位）
	var btnPos string
	err := chromedp.Run(h.session.Context(),
		chromedp.Evaluate(`(()=>{
			const ta = document.querySelector('textarea');
			if (!ta) return JSON.stringify({found:false, reason:'no_textarea'});

			// 候选选择器（按优先级排序）：
			// 1. 显式发送按钮（aria-label 含 send/发送）
			// 2. ds-button--primary（DeepSeek 发送按钮主样式）
			// 3. textarea 附近最右的可点击按钮
			const candidates = [];
			const seen = new Set();

			function addBtn(b, source) {
				if (!b || seen.has(b)) return;
				const r = b.getBoundingClientRect();
				if (r.width === 0 || r.height === 0) return;
				if (b.disabled === true || b.getAttribute('aria-disabled') === 'true') return;
				seen.add(b);
				candidates.push({
					el: b,
					x: Math.round(r.x + r.width/2),
					y: Math.round(r.y + r.height/2),
					cls: (b.getAttribute('class')||'').substring(0,60),
					text: (b.textContent||'').trim().substring(0,15),
					source: source,
					// 优先级分数：越高越优先
					score: 0
				});
			}

			// 1. aria-label 匹配
			document.querySelectorAll('button[aria-label*="send" i], button[aria-label*="发送"], [role="button"][aria-label*="send" i], [role="button"][aria-label*="发送"]').forEach(b => addBtn(b, 'aria_label'));

			// 2. ds-button--primary
			document.querySelectorAll('.ds-button--primary').forEach(b => addBtn(b, 'primary_class'));

			// 3. textarea 附近最右按钮（识图模式下发送按钮可能不是 primary）
			const taBottom = ta.getBoundingClientRect().bottom;
			const allBtns = document.querySelectorAll('button, [role="button"]');
			let rightmostBtn = null, rightmostX = -1;
			for (const b of allBtns) {
				const r = b.getBoundingClientRect();
				if (r.width === 0 || r.height === 0) continue;
				if (b.disabled === true || b.getAttribute('aria-disabled') === 'true') continue;
				// Y 范围放宽到 120px（识图模式下按钮可能因图片预览被推下）
				if (Math.abs(r.y + r.height/2 - taBottom) >= 120) continue;
				const centerX = r.x + r.width / 2;
				if (centerX > rightmostX) {
					rightmostX = centerX;
					rightmostBtn = b;
				}
			}
			if (rightmostBtn) addBtn(rightmostBtn, 'rightmost');

			if (candidates.length === 0) {
				return JSON.stringify({found:false, reason:'no_send_btn', taValue: (ta.value||'').substring(0,30)});
			}

			// 计算优先级分数
			for (const c of candidates) {
				if (c.source === 'aria_label') c.score += 100;
				if (c.source === 'primary_class') c.score += 50;
				if (c.source === 'rightmost') c.score += 10;
				// 文本含"发送"加分
				if (c.text.indexOf('发送') >= 0 || c.text.toLowerCase().indexOf('send') >= 0) c.score += 30;
				// class 含 send 加分
				if (c.cls.toLowerCase().indexOf('send') >= 0) c.score += 20;
			}
			candidates.sort((a,b) => b.score - a.score);
			const best = candidates[0];
			return JSON.stringify({
				found: true,
				x: best.x,
				y: best.y,
				cls: best.cls,
				text: best.text,
				source: best.source,
				score: best.score,
				candidateCount: candidates.length
			});
		})()`, &btnPos),
	)

	if err != nil || !strings.Contains(btnPos, `"found":true`) {
		log.Printf("[chat] ensureMessageSent: no button found (btnPos=%q, err=%v)", btnPos, err)
		// 最终回退：键盘 Enter
		log.Println("[chat] ensureMessageSent: trying keyboard enter fallback")
		return h.keyboardEnterFallback()
	}

	var pos struct {
		Found          bool   `json:"found"`
		X              int    `json:"x"`
		Y              int    `json:"y"`
		Cls            string `json:"cls"`
		Text           string `json:"text"`
		Source         string `json:"source"`
		Score          int    `json:"score"`
		CandidateCount int    `json:"candidateCount"`
	}
	json.Unmarshal([]byte(btnPos), &pos)
	log.Printf("[chat] ensureMessageSent: button at (%d,%d) cls=%q source=%s score=%d candidates=%d",
		pos.X, pos.Y, pos.Cls, pos.Source, pos.Score, pos.CandidateCount)

	// 第二步：用 chromedp.MouseClickXY 真实点击（React 按钮必须用此方式）
	// 尝试两次：第一次可能因时序问题失败
	for attempt := 1; attempt <= 2; attempt++ {
		if h.tryMouseClickXY(pos.X, pos.Y) {
			log.Printf("[chat] ensureMessageSent: mouse click succeeded (attempt %d)", attempt)
			return nil
		}
		log.Printf("[chat] ensureMessageSent: mouse click attempt %d failed", attempt)
		// 等待后重试
		if attempt < 2 {
			time.Sleep(500 * time.Millisecond)
		}
	}

	// 第三步：回退到 JS 点击 + 完整事件序列（某些非 React 按钮可能生效）
	log.Println("[chat] ensureMessageSent: trying JS event sequence")
	chromedp.Run(h.session.Context(),
		chromedp.Evaluate(fmt.Sprintf(`(()=>{
			const el = document.elementFromPoint(%d, %d);
			if (!el) return 'no_element';
			el.focus();
			el.dispatchEvent(new PointerEvent('pointerdown', {bubbles: true, cancelable: true}));
			el.dispatchEvent(new MouseEvent('mousedown', {bubbles: true, cancelable: true}));
			el.dispatchEvent(new PointerEvent('pointerup', {bubbles: true, cancelable: true}));
			el.dispatchEvent(new MouseEvent('mouseup', {bubbles: true, cancelable: true}));
			el.dispatchEvent(new MouseEvent('click', {bubbles: true, cancelable: true}));
			return 'dispatched';
		})()`, pos.X, pos.Y), nil),
	)
	time.Sleep(clickDelay)
	if !h.isTextareaStillFilled() {
		log.Println("[chat] ensureMessageSent: JS event sequence succeeded")
		return nil
	}

	// 最终回退：键盘 Enter
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
			return !!(ta && ta.value && ta.value.trim().length > 0);
		})()`, &stillFilled),
	)
	if err != nil {
		log.Printf("[chat] isTextareaStillFilled eval error: %v (assuming filled)", err)
		return true
	}
	return stillFilled
}

func (h *ChatHandler) waitForResponse(ctx context.Context, timeout time.Duration, question string) (content string, thinking string, convLimit bool, serverBusy bool, err error) {
	deadline := time.Now().Add(timeout)
	// [Fix 2026-08-10] 记录已打印过明文的短内容，内容变化才打印，避免同一短内容反复刷屏
	seenShort := ""
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "", "", false, false, ctx.Err()
		default:
		}
		// [Fix 2026-08-11] 系统提示兜底：detectImmediateError 可能漏检（如页面销毁重建后
		// lastMsg 是旧回复，此前误判 thinking 提前退出；或系统提示渲染略慢），
		// 此处每轮轮询额外扫 DOM，检测到 convLimit/serverBusy 立即返回，避免干等 120 秒超时。
		// 实测：2026-08-11 14:06 页面销毁后误判 thinking，AI 实际未回复，waitForResponse 干等超时。
		var domErr string
		if err := chromedp.Run(h.session.Context(), chromedp.Evaluate(errorDetectJS, &domErr)); err == nil {
			if strings.HasPrefix(domErr, "convLimit:") {
				log.Printf("[chat] convLimit detected via errorDetectJS in waitForResponse, returning early")
				return "", "", true, false, nil
			}
			if strings.HasPrefix(domErr, "serverBusy:") {
				log.Printf("[chat] serverBusy detected via errorDetectJS in waitForResponse, returning early")
				return "", "", false, true, nil
			}
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
				ptypes: window.__dsBrowserPTypes || {},
			unknownSamples: window.__dsBrowserUnknownSamples || []
			})`, &result),
		)
		if err == nil && result != "" {
			var r struct {
				D              bool           `json:"d"`
				DD             bool           `json:"dd"`
				C              string         `json:"c"`
				T              string         `json:"t"`
				Lim            bool           `json:"lim"`
				Busy           bool           `json:"busy"`
				PTypes         map[string]int `json:"ptypes"`
				UnknownSamples []string       `json:"unknownSamples"`
			}
			if json.Unmarshal([]byte(result), &r) != nil {
				time.Sleep(pollInterval)
				continue
			}
			// [Fix 2026-08-10] 拦截器检测到系统提示时立即返回，不依赖 r.D/r.DD。
			// 场景：对话达到长度上限/服务器繁忙时，AI 不会回复，页面 DOM 永远不会判定完成，
			// 旧逻辑（依赖 r.D||r.DD 才检查内容）会空等直到 120 秒超时，白白浪费等待时间。
			// 拦截器已在 checkFlags 中设置 __dsConvLimitHit/__dsServerBusy，直接使用即可。
			// 系统提示类型多样（对话上限/发送频繁/服务器繁忙等），不能仅凭字符数判断，
			// 必须检查明文关键词；短内容限制仅为避免误扫正常 AI 回复（长文）。
			shortC := len([]rune(r.C)) < 200 && r.C != ""
			// [Fix 2026-08-10] 捕获到疑似系统提示的短内容时，记录明文到日志（内容变化才打），
			// 便于确认系统提示的真实文本（此前日志只有字符数如"15 chars"，无法识别提示类型）
			if shortC && r.C != seenShort {
				seenShort = r.C
				log.Printf("[chat] short capture (%d chars): %q", len([]rune(r.C)), r.C)
			}
			if r.Lim || (shortC && hasConvLimit(r.C)) {
				log.Printf("[chat] convLimit detected: %q (c=%d chars, d=%v, dd=%v), returning early",
					r.C, len([]rune(r.C)), r.D, r.DD)
				return r.C, "", true, false, nil
			}
			if r.Busy || (shortC && hasServerBusy(r.C)) {
				log.Printf("[chat] serverBusy detected: %q (c=%d chars, d=%v, dd=%v), returning early",
					r.C, len([]rune(r.C)), r.D, r.DD)
				return r.C, "", false, true, nil
			}
			if r.C != "" && (r.D || r.DD) {
				log.Printf("[chat] captured: %d chars, thinking: %d chars (netDone=%v domDone=%v, convLimit=%v, serverBusy=%v, ptypes=%v)",
					len([]rune(r.C)), len([]rune(r.T)), r.D, r.DD, r.Lim, r.Busy, r.PTypes)
				if len(r.UnknownSamples) > 0 {
					log.Printf("[chat] unknown SSE events (first 5): %v", r.UnknownSamples)
				}
				// [Fix 2026-07-31] 兜底：DOM 已完成但拦截器只截到少量字符
				// 如果是错误提示（convLimit/serverBusy），直接返回让上层处理，不走复制按钮
				// 否则说明拦截器可能失效，用复制按钮接管获取完整内容
				if r.DD && !r.D && len([]rune(r.C)) < 50 {
					if hasConvLimit(r.C) {
						log.Printf("[chat] convLimit in short interceptor content: %d chars, returning early", len([]rune(r.C)))
						return r.C, "", true, false, nil
					}
					if hasServerBusy(r.C) {
						log.Printf("[chat] serverBusy in short interceptor content: %d chars, returning early", len([]rune(r.C)))
						return r.C, "", false, true, nil
					}
					log.Printf("[chat][WARN] DOM done but interceptor only %d chars (likely incomplete), trying copy button fallback", len([]rune(r.C)))
					fb, fbErr := h.fetchContentViaCopyButton()
					if fbErr == nil && fb != "" {
						log.Printf("[chat] captured (copy-btn fallback): %d chars (replaced %d chars from interceptor)", len([]rune(fb)), len([]rune(r.C)))
						log.Printf("[chat][diag] capture FALLBACK (%d chars): %q", len([]rune(fb)), truncateForLog(fb, 800))
						return fb, "", r.Lim, r.Busy, nil
					}
					log.Printf("[chat][WARN] copy button fallback failed: %v, returning intercepted content", fbErr)
				}
				// 保存原始 SSE 用于调试
				var rawSSE string
				chromedp.Run(h.session.Context(),
					chromedp.Evaluate(`window.__dsBrowserRawSSE||''`, &rawSSE),
				)
				if len(rawSSE) > 0 {
					os.WriteFile(filepath.Join(os.TempDir(), "ds_raw_sse.txt"), []byte(rawSSE), 0644)
				}
				// [Fix 2026-07-19] 截取 JSON 主体，丢弃 DeepSeek 在 JSON 后追加的"对话标题"
				// 根因：DeepSeek 在 AI 回复完成后，通过同一 SSE 流追加对话标题（如 "301277次日观望"），
				// 被拦截器当作回复内容捕获，导致 JSON 后多出非 JSON 文字，客户端解析失败。
				trimmedC := trimTrailingExtra(r.C)
				if len(trimmedC) != len(r.C) {
					log.Printf("[chat] trimmed trailing extra: %d -> %d chars (removed %d chars)",
						len([]rune(r.C)), len([]rune(trimmedC)), len([]rune(r.C))-len([]rune(trimmedC)))
				}
				c := deduplicateContent(trimmedC, question)
				t := deduplicateContent(r.T, question)
				// [诊断日志 2026-07-19] 记录 capture 原始内容和 dedup 后内容，用于定位 JSON 解析失败根因
				log.Printf("[chat][diag] capture RAW (%d chars): %q", len([]rune(r.C)), truncateForLog(r.C, 800))
				log.Printf("[chat][diag] capture DEDUP (%d chars): %q", len([]rune(c)), truncateForLog(c, 800))
				return c, t, r.Lim, r.Busy, nil
			}
			// [Fix 2026-07-31] 兜底：DOM 观察器认为回复完成但拦截器没截到内容
			// 场景：SSE 拦截器偶发失效（__dsBrowserDone=false, __dsBrowserCapture=""），
			// 但 AI 实际已在网页上完整回复（__dsBrowserDOMDone=true）
			// 不加此分支会陷入死循环：r.C=="" 跳过 captured 分支，r.DD==true 跳过 waiting 分支
			// 复用现有 fetchContentViaCopyButton：点击复制按钮读取 ds-assistant-message-main-content
			// 该元素只含 AI 回复正文，不含"本回答由 AI 生成"提示和 5 个按钮文字
			if r.DD && r.C == "" {
				log.Printf("[chat][WARN] DOM done but interceptor empty, trying copy button fallback")
				if fb, fbErr := h.fetchContentViaCopyButton(); fbErr == nil && fb != "" {
					log.Printf("[chat] captured (copy-btn fallback): %d chars", len([]rune(fb)))
					log.Printf("[chat][diag] capture FALLBACK (%d chars): %q", len([]rune(fb)), truncateForLog(fb, 800))
					return fb, "", r.Lim, r.Busy, nil
				}
				// 复制按钮也失败 → 报错，不再死循环
				return "", "", false, false, fmt.Errorf("DOM done but interceptor empty and copy button fallback failed")
			}
			// [Fix 2026-07-31] 兜底：SSE 流结束标志已置位但拦截器没截到内容
			// 场景：__dsBrowserDone=true 但 __dsBrowserCapture=""（拦截器异常）
			// 同样会陷入死循环，直接报错让上层处理
			if r.D && r.C == "" {
				return "", "", false, false, fmt.Errorf("stream done but interceptor empty")
			}
			if !r.D && !r.DD {
				// [Fix 2026-07-31] capture 为空但 thinking 有内容时，显示思考进度，避免误以为卡死
				if len([]rune(r.T)) > 0 {
					log.Printf("[chat] AI thinking... %d chars", len([]rune(r.T)))
				} else {
					log.Printf("[chat] waiting... %d chars so far", len([]rune(r.C)))
				}
			}
		}
		// [Fix 2026-07-18] 移除 errorDetectJS 调用
		// 系统提示只在 0-3 秒内出现，由 detectImmediateError 覆盖
		// waitForResponse 阶段 AI 正在回复，errorDetectJS 会误扫 AI 回复内容
		time.Sleep(pollInterval)
	}
	return "", "", false, false, fmt.Errorf("timeout waiting for response after %v", timeout)
}

// truncateForLog 截取字符串用于日志输出，避免日志过长
func truncateForLog(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "...[TRUNCATED]"
}

// trimTrailingExtra 截取 JSON 主体，丢弃 JSON 结束后追加的额外文字
// 场景：DeepSeek 在 AI 回复完成后，通过同一 SSE 流追加"对话标题"（如 "301277次日观望"），
// 被拦截器当作回复内容捕获，导致 JSON 后多出一段非 JSON 文字，客户端解析失败。
// 做法：如果 content 以 { 或 [ 开头，用括号配对找到 JSON 真正结束位置，丢弃后面的内容。
// 安全约束：只处理字符串字面量外的括号配对，避免被 JSON 字符串里的括号干扰。
func trimTrailingExtra(content string) string {
	if len(content) == 0 {
		return content
	}
	// 找第一个非空白字符
	start := 0
	for start < len(content) && (content[start] == ' ' || content[start] == '\t' || content[start] == '\n' || content[start] == '\r') {
		start++
	}
	if start >= len(content) {
		return content
	}
	var open, close byte
	switch content[start] {
	case '{':
		open, close = '{', '}'
	case '[':
		open, close = '[', ']'
	default:
		// 不是 JSON 开头，不处理
		return content
	}
	// 用括号配对找 JSON 结束位置，跳过字符串字面量内的括号
	depth := 0
	inString := false
	escape := false
	for i := start; i < len(content); i++ {
		ch := content[i]
		if escape {
			escape = false
			continue
		}
		if inString {
			if ch == '\\' {
				escape = true
			} else if ch == '"' {
				inString = false
			}
			continue
		}
		if ch == '"' {
			inString = true
			continue
		}
		if ch == open {
			depth++
		} else if ch == close {
			depth--
			if depth == 0 {
				// JSON 主体结束于位置 i（含）
				rest := strings.TrimSpace(content[i+1:])
				if rest == "" {
					// 后面没有额外文字，原样返回
					return content
				}
				// 后面有额外文字，截取到 JSON 结束位置
				return content[:i+1]
			}
		}
	}
	// 括号没配平（JSON 不完整），不处理
	return content
}

func deduplicateContent(content string, question string) string {
	if content == "" {
		return content
	}
	lines := strings.Split(content, "\n")
	if len(lines) <= 1 {
		// 单行场景：检测行内重复（如 "1+1=2 😄1+1=2"）
		return deduplicateSingleLineRepeat(content, question)
	}
	deduped := lineLevelDedup(lines)
	// 对每一行也应用单行去重（防止单行内重复）
	for i, line := range deduped {
		deduped[i] = deduplicateSingleLineRepeat(line, question)
	}
	return strings.Join(deduped, "\n")
}

// isAlnum 判断字符是否为字母、数字或中文
func isAlnum(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || (r >= 0x4e00 && r <= 0x9fff)
}

// deduplicateSingleLineRepeat 检测并去除单行内的重复模式
// 处理三重拦截器（fetch/XHR/EventSource）或 DeepSeek SSE 用不同 p 类型
// 重复发送同一 content 导致的重复，包括：
//   - "X 😄X" 模式（X 为相同子串，中间有分隔符）
//   - "部分 + 完整" 模式（DeepSeek 先发部分内容，再发完整内容，如 "2"+"1+1=2"="21+1=2"）
//   - "完整 + 部分重复" 模式（DeepSeek 先发完整内容，再发部分内容，如 "1+1=2"+"1+1"="1+1=21+1"）
//
// 安全约束：
//   - 模式0要求 full 长度 >=4，partial 长度 >=2，避免误删短文本
//   - 模式1要求 rest 不含空格（避免误判 "1+1=2 1+1" 等有效内容）
//   - 模式2要求子串长度 >=5，避免误删短文本
func deduplicateSingleLineRepeat(line string, question string) string {
	runes := []rune(line)
	n := len(runes)

	// 模式3：[Disabled 2026-07-20] 剥离问题回显（XHR 拦截器可能将对话标题追加到 capture 末尾）
	// 原设计：capture="42+2"，question="2+2=?"，末尾"2+2"是问题子串，剥离后得到"4"
	// 禁用原因：该模式对每一行单独处理，会误删 JSON 字段值。例如：
	//   - 行 `  "order_type": "LIMIT",` 末尾 `"LIMIT",` 在 question 中能找到 → 被删
	//   - 行 `  "next_day_expect": {`     整行在 question 中能找到 → 被删
	//   - 行 `  "plan_price": 6.11,`      末尾 `6.11,` 在 question 中能找到 → 被删
	// 该模式的原始目标（处理 JSON 后追加的对话标题）已由 trimTrailingExtra 接管，
	// trimTrailingExtra 用括号配对精确识别 JSON 边界，只删除整个 JSON 结束后的额外文字，不会破坏 JSON 内部结构。
	// if question != "" && n >= 4 && n <= 30 {
	// 	...（已禁用，原代码保留在注释中作为参考）
	// }

	if n < 6 {
		return line
	}

	// 模式0：检测"完整 + 完整的前缀"模式（完整内容后跟着部分重复）
	// 场景：DeepSeek 先发完整内容（如 "1+1=2"），再发部分内容（如 "1+1"）
	// 拦截器都追加，导致 capture="1+1=21+1"
	// 安全约束：full 长度 >=4，partial 长度 >=2，full 以 partial 开头
	//           full 不含空格（避免误判 "1+1=2 1+1" 等含空格内容）
	//           full 以字母数字开头（避免误判 "=21+1=2" 等"部分+完整"模式）
	for L := n - 1; L >= n/2 && L >= 4; L-- {
		full := string(runes[0:L])
		partial := string(runes[L:])
		if len([]rune(partial)) >= 2 && strings.HasPrefix(full, partial) &&
			!strings.Contains(full, " ") && isAlnum(runes[0]) {
			return full
		}
	}

	// 模式1：检测"短前缀 + 完整内容"，其中短前缀是完整内容的末尾部分
	// 场景：DeepSeek 先发送部分内容（如 "2"），再发送完整内容（如 "1+1=2"）
	// 拦截器都追加，导致 capture="21+1=2"
	// 安全约束：前缀长度 1-2，rest 长度 >=4，rest 不含空格，且 rest 以 prefix 结尾
	for prefixLen := 1; prefixLen <= 2 && prefixLen*2 < n; prefixLen++ {
		prefix := string(runes[:prefixLen])
		rest := string(runes[prefixLen:])
		restRunes := len([]rune(rest))
		if restRunes >= 4 && !strings.Contains(rest, " ") && strings.HasSuffix(rest, prefix) {
			return rest
		}
	}

	// 模式2：检测"X + 分隔符 + X" 前缀重复（三重拦截器各自捕获同一完整内容）
	// 安全约束：子串长度 >=5，避免误删短文本
	if n >= 10 {
		for sLen := 5; sLen <= n/2; sLen++ {
			s := string(runes[0:sLen])
			for sep := 0; sep <= 3 && sLen+sep+sLen <= n; sep++ {
				secondStart := sLen + sep
				second := string(runes[secondStart : secondStart+sLen])
				if s == second {
					rest := string(runes[secondStart+sLen:])
					rest = strings.TrimSpace(rest)
					if rest == "" {
						return s
					}
					return s + " " + rest
				}
			}
		}
	}

	return line
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
	// 重置对话累计字符数，生成新的随机阈值（60万-90万字符，约20-30条超长消息）
	h.convMsgCount.Store(0)
	threshold := int64(600000 + rand.Intn(300001))
	h.convMsgThreshold.Store(threshold)
	log.Printf("[chat] new conv threshold: %d chars", threshold)

	var btnPos string
	err := chromedp.Run(h.session.Context(),
		chromedp.Evaluate(`(()=>{
			// [Fix 2026-08-11] 只定位不点击：DeepSeek 是 React 页面，JS .click() 对按钮无效，
			// 必须用 chromedp.MouseClickXY 真实点击（与 checkToggleStates/ensureMessageSent 一致）。
			// 此前用 JS click 点"新对话"按钮后直接返回"成功"（found=true 但实际没开），
			// 导致重试发送还在旧对话、再次触发"对话长度上限"、give up 把系统提示原文返回客户端
			// （实测 2026-08-11 13:43-13:56 共 28 次）。
			const candidates = document.querySelectorAll('a[href="/"], a[href*="/chat"], [class*="new"], [class*="New"], [class*="sidebar"]');
			for (const el of candidates) {
				const text = (el.textContent || '').trim();
				if (text === 'New Chat' || text === '新对话' || text === '新话题') {
					const r = el.getBoundingClientRect();
					if (r.width > 0 && r.height > 0) {
						return JSON.stringify({found: true, x: Math.round(r.x + r.width/2), y: Math.round(r.y + r.height/2)});
					}
				}
			}
			return JSON.stringify({found: false});
		})()`, &btnPos),
	)
	if err == nil && strings.Contains(btnPos, `"found":true`) {
		var pos struct {
			X int `json:"x"`
			Y int `json:"y"`
		}
		if json.Unmarshal([]byte(btnPos), &pos) == nil {
			log.Printf("[chat] clicking new conversation button at (%d,%d)", pos.X, pos.Y)
			if err := chromedp.Run(h.session.Context(), chromedp.MouseClickXY(float64(pos.X), float64(pos.Y))); err == nil {
				// 验证新对话是否真的打开（空对话、无历史消息），没开成功则继续尝试 Ctrl+J
				if h.waitForEmptyTextarea(3 * time.Second) {
					log.Println("[chat] new conversation confirmed (empty chat)")
					// 新对话后 DeepSeek 默认关闭深度思考，需要重新检测并开启
					h.session.checkToggleStates()
					return nil
				}
				log.Println("[chat] new conversation button clicked but chat not empty, trying Ctrl+J")
			} else {
				log.Printf("[chat] new conversation click error: %v, trying Ctrl+J", err)
			}
		}
	} else {
		log.Println("[chat] UI element not found, trying Ctrl+J")
	}
	log.Println("[chat] trying Ctrl+J")
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
		// 新对话后 DeepSeek 默认关闭深度思考，需要重新检测并开启
		h.session.checkToggleStates()
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
// [Fix 2026-07-31] 改为只检查新增 ds-message，不再全页面扫描
// __dsMessageBaseline 在 sendMessage 时已设置
// 检测窗口 3 秒：系统提示一定在"正在思考"之前出现，只在前 3 秒内检测
// 检测到"正在思考/已思考"时早停（AI 正常工作）
func (h *ChatHandler) detectImmediateError() (string, string) {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var domText string
		domErr := chromedp.Run(h.session.Context(),
			chromedp.Evaluate(errorDetectJS, &domText),
		)
		if domErr == nil && domText != "" {
			if domText == "thinking" {
				log.Printf("[chat] AI thinking detected (DOM), early exit")
				return "", ""
			}
			parts := strings.SplitN(domText, ":", 2)
			if len(parts) == 2 {
				return parts[0], parts[1]
			}
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// 3 秒内未检测到系统提示——系统提示一定在"正在思考"之前出现，没有系统提示说明请求正常
	// 继续进入正常等待回复流程
	log.Printf("[chat] detectImmediateError: 3s passed, no error detected, continuing")

	return "", ""
}

// logDiagInfo 读取 window.__dsDiagLog 并打印到日志
// 用于在系统提示出现时捕获 DOM 结构证据
func logDiagInfo(h *ChatHandler, promptType, keyword string) {
	var diagInfo string
	chromedp.Run(h.session.Context(),
		chromedp.Evaluate(`window.__dsDiagLog || ''`, &diagInfo),
	)
	if diagInfo != "" {
		log.Printf("[diag] system prompt detected: type=%s keyword=%s, domInfo=%s", promptType, keyword, diagInfo)
	} else {
		log.Printf("[diag] system prompt detected: type=%s keyword=%s, but domInfo is empty", promptType, keyword)
	}
}

// fetchContentViaCopyButton 通过点击 DeepSeek 页面上的复制按钮获取回复内容
// 当拦截器捕获的内容不可信时（空内容），作为兜底方案
// 原理：找到最后一个 ds-message 中的复制按钮 → CDP 点击 → 读取文章 textContent
// 复制按钮在 ds-message 容器内（不在 ds-markdown 内部），通过 SVG 2个 <path> 识别
func (h *ChatHandler) fetchContentViaCopyButton() (string, error) {
	// [Fix 2026-07-31] 基线检查：确认本轮有新回复，避免读到上一轮的旧内容
	// __dsArticleBaseline 在 sendMessage/sendMessageOrUpload 时记录（发送前的 ds-markdown 数量）
	// 如果当前 ds-markdown 数量 <= baseline，说明本轮 AI 还没输出新回复，不读旧内容
	var hasNewReply bool
	chromedp.Run(h.session.Context(),
		chromedp.Evaluate(`(document.querySelectorAll('[class*="ds-markdown"]').length > (window.__dsArticleBaseline || 0))`, &hasNewReply),
	)
	if !hasNewReply {
		log.Printf("[chat][WARN] copy button fallback: no new reply since request (baseline not exceeded)")
		return "", fmt.Errorf("no new reply since request")
	}

	// 1. 注入剪贴板钩子：拦截 navigator.clipboard.writeText
	chromedp.Run(h.session.Context(),
		chromedp.Evaluate(`(()=>{
			if (!window.__dsClipboardHooked) {
				var orig = navigator.clipboard.writeText.bind(navigator.clipboard);
				navigator.clipboard.writeText = function(text) {
					window.__dsCopyContent = text;
					return orig(text);
				};
				window.__dsClipboardHooked = true;
			}
			window.__dsCopyContent = '';
			return true;
		})()`, nil),
	)

	// 2. 找最后一个 ds-message 中的复制按钮，点击后读取文章文本内容
	// 复制按钮在 ds-message 容器内（不在 ds-markdown 内部），只取最后一个消息避免误点击
	// 注意：程序化点击不触发 Clipboard API，所以点完按钮后直接读文章 textContent
	var clicked bool
	chromedp.Run(h.session.Context(),
		chromedp.Evaluate(`(()=>{
			function isCopyBtn(btn) {
				var svg = btn.querySelector('svg');
				if (!svg) return false;
				return svg.querySelectorAll('path').length === 2;
			}

			// 取最后一个 ds-message（最新 AI 回复），在其中查找复制按钮
			var messages = document.querySelectorAll('[class*="ds-message"]');
			if (!messages.length) return false;
			var lastMsg = messages[messages.length - 1];
			var divs = lastMsg.querySelectorAll('div');
			for (var i = 0; i < divs.length; i++) {
				if (isCopyBtn(divs[i])) {
					divs[i].click();
					return true;
				}
			}
			return false;
		})()`, &clicked),
	)

	if !clicked {
		return "", fmt.Errorf("copy button not found")
	}

	// 3. 等待复制完成，然后读取最后一个 ds-message 中 ds-assistant-message-main-content 的文本
	// ds-message 内部有两个子元素：思考过程 + 回复内容，只取后者
	time.Sleep(300 * time.Millisecond)

	var content string
	chromedp.Run(h.session.Context(),
		chromedp.Evaluate(`(()=>{
			var messages = document.querySelectorAll('[class*="ds-message"]');
			if (!messages.length) return '';
			var lastMsg = messages[messages.length - 1];
			var mainContent = lastMsg.querySelector('[class*="ds-assistant-message-main-content"]');
			if (!mainContent) return '';
			return mainContent.textContent || '';
		})()`, &content),
	)

	log.Printf("[chat] copy button fallback: got %d chars", len([]rune(content)))
	return content, nil
}

// retryWithAccountSwitch 切换账号后重新发送消息（支持多账号轮询）
func (h *ChatHandler) retryWithAccountSwitch(ctx context.Context, mode string, text string, images []string, files []string) (*ChatResponse, error) {
	// 客户端已取消请求，不重试，让客户端自己决定是否重试
	if ctx.Err() != nil {
		log.Printf("[chat] retryWithAccountSwitch: client canceled, skipping retry")
		return nil, ctx.Err()
	}
	// [账号质量管理] 进入重试说明当前账号触发了系统限制，记录当前账号的限制触发次数
	// 必须在 SwitchAccount 调用前记录，否则 currentEmail 已被切换
	if email := h.session.currentEmail; email != "" {
		h.session.stats.RecordLimitTrigger(email)
		log.Printf("[chat] recorded limit trigger for account: %s", email)
	}
	accountCount := h.session.AvailableAccountCount()
	totalAccounts := h.session.AccountCount()
	log.Printf("[chat] starting account switch retry, available accounts: %d/%d, mode=%q, text_len=%d, images=%d, files=%d",
		accountCount, totalAccounts, mode, len([]rune(text)), len(images), len(files))

	// 只尝试其他可用账号（accountCount-1 次），不重复登录当前账号
	// 如果可用账号少于2个，则直接返回错误
	if accountCount <= 1 {
		disabledCount := totalAccounts - accountCount
		log.Printf("[chat] retryWithAccountSwitch: only %d available account(s) (%d disabled), giving up",
			accountCount, disabledCount)
		return nil, fmt.Errorf("服务器繁忙，可用账号不足（禁用%d个）", disabledCount)
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
			// SwitchAccount 内部已通过 currentSortedIdx 前进实现跳过，无需外层再前进索引
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
			logDiagInfo(h, errType, errMsg)
			// [账号质量管理] 记录当前重试账号的限制触发（SwitchAccount 已切换 currentEmail）
			if email := h.session.currentEmail; email != "" {
				h.session.stats.RecordLimitTrigger(email)
			}
			log.Printf("[chat] attempt %d failed with serverBusy: %s, will try next account", attempt+1, errMsg)
			continue
		}
		if errType == "convLimit" {
			logDiagInfo(h, errType, errMsg)
			log.Printf("[chat] attempt %d hit convLimit, will retry with new conversation", attempt+1)
			return h.retryWithNewConversation(ctx, mode, text, images, files)
		}
		if errType != "" {
			logDiagInfo(h, errType, errMsg)
			log.Printf("[chat] attempt %d detected other error: %s:%s", attempt+1, errType, errMsg)
		}

		// 等待响应
		content, thinking, convLimit, serverBusy, err := h.waitForResponse(ctx, h.responseTimeout, text)
		if err != nil {
			log.Printf("[chat] attempt %d waitForResponse error: %v (content=%d chars)", attempt+1, err, len([]rune(content)))
			return &ChatResponse{Content: content, Thinking: thinking}, err
		}
		log.Printf("[chat] attempt %d waitForResponse: content=%d chars, thinking=%d chars, convLimit=%v, serverBusy=%v",
			attempt+1, len([]rune(content)), len([]rune(thinking)), convLimit, serverBusy)
		if serverBusy {
			// [账号质量管理] 记录当前重试账号的限制触发
			if email := h.session.currentEmail; email != "" {
				h.session.stats.RecordLimitTrigger(email)
			}
			log.Printf("[chat] attempt %d serverBusy from waitForResponse, will try next account", attempt+1)
			continue
		}
		if convLimit {
			// [Fix 2026-08-11] 切换账号后仍 convLimit 时直接放弃（不再回 retryWithNewConversation），
			// 防止"新对话→切账号→新对话"无限循环。重试链：主路径 convLimit → 新对话 → 仍 convLimit → 切账号 → 放弃。
			log.Printf("[chat] attempt %d convLimit after account switch, giving up (chain: new conv -> account switch)", attempt+1)
			return nil, fmt.Errorf("切换账号后仍触发对话长度上限")
		}
		if hasServerBusy(content) {
			// [账号质量管理] 记录当前重试账号的限制触发
			if email := h.session.currentEmail; email != "" {
				h.session.stats.RecordLimitTrigger(email)
			}
			log.Printf("[chat] attempt %d hasServerBusy in content, will try next account", attempt+1)
			continue
		}
		// 成功
		log.Printf("[chat] attempt %d succeeded: content=%d chars", attempt+1, len([]rune(content)))
		if content == "" {
			log.Printf("[chat] attempt %d empty content, trying copy button fallback", attempt+1)
			if copyContent, err := h.fetchContentViaCopyButton(); err == nil && copyContent != "" {
				content = copyContent
			}
		}
		return &ChatResponse{Content: content, Thinking: thinking}, nil
	}

	log.Printf("[chat] all %d accounts exhausted, giving up", accountCount)
	return nil, fmt.Errorf("所有账号都繁忙，请稍后重试")
}

// retryWithNewConversation 开启新对话后重新发送消息
func (h *ChatHandler) retryWithNewConversation(ctx context.Context, mode string, text string, images []string, files []string) (*ChatResponse, error) {
	// 客户端已取消请求，不重试，让客户端自己决定是否重试
	if ctx.Err() != nil {
		log.Printf("[chat] retryWithNewConversation: client canceled, skipping retry")
		return nil, ctx.Err()
	}
	if err := h.NewConversation(ctx); err != nil {
		log.Printf("[chat] new conversation failed: %v", err)
		// [Fix 2026-08-11] 不再把"达到对话长度上限"当作正常响应返回客户端
		// （木偶说实测收到 len=15 的该系统提示，靠异常短内容检测才发现）。
		// 改为返回明确错误，让客户端知道开新对话失败的真实原因。
		return nil, fmt.Errorf("开新对话失败: %w", err)
	}
	if err := h.prepareForRetry(ctx, mode, images); err != nil {
		return &ChatResponse{Content: "新开对话后准备失败"}, err
	}
	if err := h.sendMessageOrUpload(text, files); err != nil {
		return &ChatResponse{Content: "新开对话后重新发送失败"}, err
	}
	content, thinking, convLimit, serverBusy, err := h.waitForResponse(ctx, h.responseTimeout, text)
	if err != nil {
		return &ChatResponse{Content: content, Thinking: thinking}, err
	}
	// 新对话后仍然繁忙，切换账号重试
	if serverBusy || hasServerBusy(content) {
		log.Printf("[chat] new conversation still serverBusy, switching account")
		return h.retryWithAccountSwitch(ctx, mode, text, images, files)
	}
	// 新对话后仍然命中上限：切换账号重试（用户要求：拦截到大量系统提示应"新开对话或切换账号后重试"）
	if convLimit || hasConvLimit(content) {
		// [Fix 2026-08-11] 用户确认：新对话（干净空对话）不可能触发"对话长度上限"，
		// 若新对话后仍出现 convLimit，一定是脚本问题（新对话未真正开启 / 上次系统提示
		// 残留被误判为本次结果），切换账号解决不了脚本问题，直接返回明确错误。
		log.Printf("[chat] new conversation still hit convLimit (unexpected for fresh chat), returning error: %q", truncateForLog(content, 200))
		return nil, fmt.Errorf("新对话后仍触发对话长度上限（新对话不应触发，疑似脚本问题，请检查日志）")
	}
	if content == "" {
		log.Printf("[chat] new conversation empty content, trying copy button fallback")
		if copyContent, err := h.fetchContentViaCopyButton(); err == nil && copyContent != "" {
			content = copyContent
		}
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
