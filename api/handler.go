package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ds2api-browser/browser"
	"ds2api-browser/config"
)

type Handler struct {
	cfg         *config.Config
	chatHandler *browser.ChatHandler
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
}

type chatMessage struct {
	Role    string       `json:"role"`
	Content contentParts `json:"content"`
}

type contentParts []contentPart

type contentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url,omitempty"`
	FileURL *struct {
		URL string `json:"url"`
	} `json:"file_url,omitempty"`
	File *struct {
		URL      string `json:"url,omitempty"`
		FileData string `json:"file_data,omitempty"`
		Name     string `json:"name,omitempty"`
		Filename string `json:"filename,omitempty"`
	} `json:"file,omitempty"`
}

func (c *contentParts) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*c = contentParts{{Type: "text", Text: s}}
		return nil
	}
	var parts []contentPart
	if err := json.Unmarshal(b, &parts); err != nil {
		return err
	}
	*c = parts
	return nil
}

type chatResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []choice `json:"choices"`
}

type choice struct {
	Index        int     `json:"index"`
	Message      message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

type message struct {
	Role             string `json:"role"`
	Content          string `json:"content"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

type streamChunk struct {
	ID      string  `json:"id"`
	Object  string  `json:"object"`
	Created int64   `json:"created"`
	Model   string  `json:"model"`
	Choices []delta `json:"choices"`
}

type delta struct {
	Index        int       `json:"index"`
	Delta        *msgDelta `json:"delta,omitempty"`
	FinishReason string    `json:"finish_reason"`
}

type msgDelta struct {
	Role             string `json:"role,omitempty"`
	Content          string `json:"content,omitempty"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

type errorResponse struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

func NewHandler(cfg *config.Config, chatHandler *browser.ChatHandler) *Handler {
	return &Handler{cfg: cfg, chatHandler: chatHandler}
}

func (h *Handler) Router() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", h.handleChat)
	mux.HandleFunc("/v1/account", h.handleAccount)
	mux.HandleFunc("/v1/account/switch", h.handleAccountSwitch)
	mux.HandleFunc("/v1/debug", h.handleDebug)
	mux.HandleFunc("/healthz", h.handleHealth)
	return mux
}

func (h *Handler) handleAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "method not allowed")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"current_account": h.chatHandler.Session().CurrentAccount(),
		"current_index":   h.chatHandler.Session().CurrentIndex(),
		"total_accounts":  h.chatHandler.Session().AccountCount(),
		"accounts":        h.getAccountList(),
	})
}

func (h *Handler) handleAccountSwitch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed")
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	oldEmail := h.chatHandler.Session().CurrentAccount()
	newEmail, err := h.chatHandler.Session().SwitchAccount()
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":     false,
			"error":       err.Error(),
			"old_account": oldEmail,
		})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":       true,
		"old_account":   oldEmail,
		"new_account":   newEmail,
		"current_index": h.chatHandler.Session().CurrentIndex(),
	})
}

func (h *Handler) getAccountList() []map[string]interface{} {
	result := make([]map[string]interface{}, 0, len(h.cfg.Accounts))
	for i, acc := range h.cfg.Accounts {
		result = append(result, map[string]interface{}{
			"index": i,
			"email": acc.Email,
		})
	}
	return result
}

func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(200)
	w.Write([]byte(`{"status":"ok"}`))
}

func (h *Handler) handleDebug(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "method not allowed")
		return
	}
	// debug 端点也需要认证
	if h.cfg.APIKey != "" {
		auth := r.Header.Get("Authorization")
		key := strings.TrimPrefix(auth, "Bearer ")
		if key != h.cfg.APIKey {
			writeError(w, 401, "unauthorized")
			return
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	// 查询浏览器中的拦截器状态
	var interceptorStr string
	if err := browser.RunEval(h.chatHandler.Session().Context(),
		`JSON.stringify({
			capture: (window.__dsBrowserCapture || '').substring(0, 2000),
			thinking: (window.__dsBrowserThinking || '').substring(0, 2000),
			done: window.__dsBrowserDone || false,
			domDone: window.__dsBrowserDOMDone || false,
			observeActive: window.__dsObserveActive || false,
			observeInterval: !!window.__dsObserveInterval,
			scanResult: (function(){
				var messages = document.querySelectorAll('[class*="ds-message"]');
				if (!messages.length) return {error: 'no messages'};
				var lastMsg = messages[messages.length - 1];
				var hasAssistant = !!lastMsg.querySelector('[class*="ds-assistant-message-main-content"]');
				var parent = lastMsg.parentElement;
				var btns = parent ? parent.querySelectorAll('[class*="ds-button"]') : [];
				return {
					totalMessages: messages.length,
					lastMsgClass: lastMsg.className,
					hasAssistant: hasAssistant,
					parentClass: parent ? parent.className : 'null',
					parentBtnCount: btns.length,
					wouldSetDone: hasAssistant && parent && btns.length >= 15
				};
			})(),
			log: (window.__dsBrowserLog || []).slice(-30),
			ptypes: window.__dsBrowserPTypes || {},
			convLimit: window.__dsConvLimitHit || false,
			serverBusy: window.__dsServerBusy || false,
			url: window.location.href
		})`, &interceptorStr); err != nil {
		log.Printf("[api] debug interceptor eval error: %v", err)
		interceptorStr = "{}"
	}

	// 获取页面 DOM 文本的摘要
	var pageStr string
	if err := browser.RunEval(h.chatHandler.Session().Context(),
		`(()=>{
			const ta = document.querySelector('textarea');
			const articles = document.querySelectorAll('[class*="ds-markdown"]');
			const lastArticle = articles.length > 0 ? (articles[articles.length-1].textContent || '').substring(0, 200) : '';
			return JSON.stringify({
				textareaExists: !!ta,
				textareaDisabled: ta ? ta.disabled : false,
				textareaValue: ta ? (ta.value || '').substring(0, 100) : '',
				articleCount: articles.length,
				lastArticlePreview: lastArticle,
				bodyText: (document.body && document.body.textContent || '').substring(0, 1000)
			});
		})()`, &pageStr); err != nil {
		log.Printf("[api] debug page eval error: %v", err)
		pageStr = "{}"
	}

	// 如果 ?msgs=1，输出所有 ds-message 的详细结构
	if r.URL.Query().Get("msgs") == "1" {
		var msgsStr string
		if err := browser.RunEval(h.chatHandler.Session().Context(),
			`JSON.stringify((()=>{
				var info = {messages: []};
				var msgs = document.querySelectorAll('[class*="ds-message"]');
				for (var i = 0; i < msgs.length; i++) {
					var msg = msgs[i];
					var item = {
						index: i,
						tag: msg.tagName,
						className: msg.className,
						textPreview: (msg.textContent || '').substring(0, 150),
						hasAssistantContent: !!msg.querySelector('[class*="ds-assistant-message-main-content"]'),
						hasMarkdown: !!msg.querySelector('[class*="ds-markdown"]'),
						hasThinking: !!(msg.querySelector('[class*="thinking"]') || msg.querySelector('[class*="reasoning"]')),
						buttonCount: msg.querySelectorAll('[class*="ds-button"]').length,
						childCount: msg.children.length,
						childClasses: []
					};
					for (var c = 0; c < msg.children.length && c < 10; c++) {
						item.childClasses.push(msg.children[c].className);
					}
					info.messages.push(item);
				}
				info.totalMessages = msgs.length;

				// 额外检查：最后一个 ds-message 的父级链和兄弟元素中的按钮
				if (msgs.length > 0) {
					var lastMsg = msgs[msgs.length - 1];
					var chain = [];
					var el = lastMsg;
					for (var j = 0; j < 5 && el; j++) {
						chain.push({
							tag: el.tagName,
							className: el.className,
							buttonCount: el.querySelectorAll('button, [class*="ds-button"]').length,
							childCount: el.children.length
						});
						el = el.parentElement;
					}
					info.lastMsgParentChain = chain;

					var next = lastMsg.nextElementSibling;
					info.lastMsgNextSibling = next ? {
						tag: next.tagName,
						className: next.className,
						buttonCount: next.querySelectorAll('button, [class*="ds-button"]').length,
						textPreview: (next.textContent || "").substring(0, 200)
					} : null;

					// 检查页面所有 button 元素及其所属消息
					var allBtns = document.querySelectorAll('button, [class*="ds-button"]');
					var btnInfo = [];
					for (var b = 0; b < allBtns.length && b < 20; b++) {
						var btn = allBtns[b];
						var btnParent = btn.closest('[class*="ds-message"]');
						btnInfo.push({
							tag: btn.tagName,
							className: (btn.className || '').substring(0, 100),
							ariaLabel: btn.getAttribute('aria-label') || '',
							parentMsg: btnParent ? 'IN_MESSAGE' : 'OUTSIDE'
						});
					}
					info.allButtons = btnInfo;
					info.totalButtons = allBtns.length;
				}
				return info;
			})())`, &msgsStr); err != nil {
			msgsStr = `{"error":"` + err.Error() + `"}`
		}
		json.NewEncoder(w).Encode(map[string]json.RawMessage{
			"messages": json.RawMessage(msgsStr),
		})
		return
	}

	json.NewEncoder(w).Encode(map[string]json.RawMessage{
		"interceptor": json.RawMessage(interceptorStr),
		"page":        json.RawMessage(pageStr),
	})
}

func (h *Handler) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed")
		return
	}

	auth := r.Header.Get("Authorization")
	if h.cfg.APIKey != "" {
		key := strings.TrimPrefix(auth, "Bearer ")
		if key != h.cfg.APIKey {
			writeError(w, 401, "unauthorized")
			return
		}
	}

	var req chatRequest
	r.Body = http.MaxBytesReader(w, r.Body, 10<<20)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "invalid request body")
		return
	}

	text, images := extractContent(req.Messages)
	fileRefs := extractFileData(req.Messages)

	// 将文件数据保存到临时文件
	var filePaths []string
	for _, f := range fileRefs {
		p, err := saveFileData(f)
		if err != nil {
			log.Printf("[api] save file data error: %v", err)
			continue
		}
		filePaths = append(filePaths, p)
	}
	// 请求结束后清理临时文件
	defer func() {
		for _, p := range filePaths {
			os.Remove(p)
		}
	}()

	hasContent := text != "" || len(images) > 0 || len(filePaths) > 0
	if !hasContent {
		writeError(w, 400, "no content found in request")
		return
	}

	if len(images) > 0 {
		log.Printf("[api] image chat request: text=%q, images=%d, files=%d, msgs=%d", text, len(images), len(filePaths), len(req.Messages))
	} else if len(filePaths) > 0 {
		log.Printf("[api] file chat request: text=%q, files=%d, msgs=%d", text, len(filePaths), len(req.Messages))
	} else if text != "" {
		log.Printf("[api] text chat request: text=%q, msgs=%d", text, len(req.Messages))
	}

	timeout := time.Duration(h.cfg.ResponseTimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	shouldNewConv := false
	if r.Header.Get("X-New-Conversation") == "true" {
		shouldNewConv = true
		log.Println("[api] new conversation (header)")
	} else if r.URL.Query().Get("new") == "true" {
		shouldNewConv = true
		log.Println("[api] new conversation (param)")
	} else if h.cfg.AutoNewConversation {
		shouldNewConv = true
		log.Printf("[api] new conversation (config auto_new_conversation=true)")
	} else if h.chatHandler.ShouldNewConversation() {
		shouldNewConv = true
		log.Printf("[api] new conversation (idle > 10min)")
	} else if h.chatHandler.ShouldNewConversationByCount() {
		shouldNewConv = true
		log.Printf("[api] new conversation (msg count reached random threshold)")
	} else {
		log.Printf("[api] continuous chat (reusing existing conversation, msgs=%d)", len(req.Messages))
	}

	var resp *browser.ChatResponse
	var err error

	t0 := time.Now()
	if len(images) > 0 || len(filePaths) > 0 {
		resp, err = h.chatHandler.SendImageChat(ctx, &browser.ChatRequest{
			Text:   text,
			Images: images,
			Files:  filePaths,
		}, shouldNewConv)
	} else {
		resp, err = h.chatHandler.SendTextChat(ctx, text, shouldNewConv)
	}
	log.Printf("[api⏱] SendChat: %dms", time.Since(t0)/time.Millisecond)

	if err != nil {
		log.Printf("[api] chat error: %v", err)
		writeError(w, 500, fmt.Sprintf("chat failed: %v", err))
		return
	}

	if req.Stream {
		h.writeStreamResponse(w, resp)
	} else {
		h.writeJSONResponse(w, resp)
	}
}

// writeJSONResponse 构造并写入非流式 JSON 响应
func (h *Handler) writeJSONResponse(w http.ResponseWriter, resp *browser.ChatResponse) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(chatResponse{
		ID:      "browser-" + fmt.Sprintf("%d", time.Now().UnixNano()),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   "deepseek-v4-pro",
		Choices: []choice{{
			Index: 0,
			Message: message{
				Role:             "assistant",
				Content:          resp.Content,
				ReasoningContent: resp.Thinking,
			},
			FinishReason: "stop",
		}},
	})
}

func (h *Handler) writeStreamResponse(w http.ResponseWriter, resp *browser.ChatResponse) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		log.Println("[api] stream not supported, falling back to JSON")
		h.writeJSONResponse(w, resp)
		return
	}

	id := fmt.Sprintf("browser-%d", time.Now().UnixNano())
	created := time.Now().Unix()

	chunk1 := streamChunk{
		ID: id, Object: "chat.completion.chunk", Created: created,
		Model: "deepseek-v4-pro",
		Choices: []delta{{
			Index: 0,
			Delta: &msgDelta{Role: "assistant", Content: resp.Content, ReasoningContent: resp.Thinking},
		}},
	}
	data, err := json.Marshal(chunk1)
	if err != nil {
		log.Printf("[api] marshal chunk1 error: %v", err)
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", string(data))
	flusher.Flush()

	chunk2 := streamChunk{
		ID: id, Object: "chat.completion.chunk", Created: created,
		Model:   "deepseek-v4-pro",
		Choices: []delta{{Index: 0, Delta: &msgDelta{}, FinishReason: "stop"}},
	}
	data, err = json.Marshal(chunk2)
	if err != nil {
		log.Printf("[api] marshal chunk2 error: %v", err)
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", string(data))
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func extractContent(messages []chatMessage) (text string, images []string) {
	var hasFile bool
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Role != "user" {
			continue
		}
		for _, part := range msg.Content {
			switch part.Type {
			case "text":
				if part.Text != "" && text == "" {
					text = strings.TrimSpace(part.Text)
				}
			case "image_url":
				if part.ImageURL != nil && part.ImageURL.URL != "" {
					images = append(images, part.ImageURL.URL)
				}
			case "file", "file_url":
				hasFile = true
			}
		}
	}
	if text == "" && len(images) > 0 {
		text = "请识别图片中的内容"
	}
	if text == "" && hasFile {
		text = "请处理附件中的内容"
	}
	return text, images
}

// extractFileData 从消息中提取文件内容（URL 或 base64 数据）
func extractFileData(messages []chatMessage) []string {
	var files []string
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Role != "user" {
			continue
		}
		for _, part := range msg.Content {
			switch part.Type {
			case "file":
				if part.File != nil {
					if part.File.FileData != "" {
						files = append(files, part.File.FileData)
					} else if part.File.URL != "" {
						files = append(files, part.File.URL)
					}
				}
			case "file_url":
				if part.FileURL != nil && part.FileURL.URL != "" {
					files = append(files, part.FileURL.URL)
				}
			}
		}
	}
	return files
}

// saveFileData 将文件数据（base64 或 data URI）保存到临时文件，返回路径
func saveFileData(data string) (string, error) {
	var rawData []byte
	var ext string

	if strings.HasPrefix(data, "data:") {
		// data URI 格式: data:application/pdf;base64,xxxx
		comma := strings.Index(data, ",")
		if comma >= 0 {
			mime := data[5:comma]
			if strings.Contains(mime, "text/plain") || strings.Contains(mime, "text/markdown") {
				ext = ".txt"
			} else if strings.Contains(mime, "application/pdf") {
				ext = ".pdf"
			} else if strings.Contains(mime, "text/csv") {
				ext = ".csv"
			} else if strings.Contains(mime, "text/html") {
				ext = ".html"
			} else if strings.Contains(mime, "application/json") {
				ext = ".json"
			} else if strings.Contains(mime, "image/") {
				ext = ".img"
			} else if strings.Contains(mime, "application/vnd.openxmlformats-officedocument") {
				if strings.Contains(mime, "wordprocessingml") {
					ext = ".docx"
				} else if strings.Contains(mime, "spreadsheetml") {
					ext = ".xlsx"
				} else if strings.Contains(mime, "presentationml") {
					ext = ".pptx"
				}
			}
			if ext == "" {
				ext = ".bin"
			}
			b64 := data[comma+1:]
			rawData, _ = base64.StdEncoding.DecodeString(b64)
		}
	} else if isBase64(data) {
		rawData, _ = base64.StdEncoding.DecodeString(data)
		ext = ".txt"
	} else {
		// 普通文本内容
		rawData = []byte(data)
		ext = ".txt"
	}

	if len(rawData) == 0 {
		rawData = []byte(data)
		ext = ".txt"
	}

	filePath := filepath.Join(os.TempDir(), fmt.Sprintf("ds_file_%d%s", time.Now().UnixNano(), ext))
	if err := os.WriteFile(filePath, rawData, 0644); err != nil {
		return "", err
	}
	return filePath, nil
}

func isBase64(s string) bool {
	if len(s) < 10 {
		return false
	}
	_, err := base64.StdEncoding.DecodeString(s)
	return err == nil
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(errorResponse{Error: struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	}{Message: msg, Type: "error"}})
}
