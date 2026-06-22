package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
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
	_ = browser.RunEval(h.chatHandler.Session().Context(),
		`JSON.stringify({
			capture: (window.__dsBrowserCapture || '').substring(0, 2000),
			thinking: (window.__dsBrowserThinking || '').substring(0, 2000),
			done: window.__dsBrowserDone || false,
			domDone: window.__dsBrowserDOMDone || false,
			log: (window.__dsBrowserLog || []).slice(-30),
			ptypes: window.__dsBrowserPTypes || {},
			convLimit: window.__dsConvLimitHit || false,
			serverBusy: window.__dsServerBusy || false,
			url: window.location.href
		})`, &interceptorStr)

	// 获取页面 DOM 文本的摘要
	var pageStr string
	_ = browser.RunEval(h.chatHandler.Session().Context(),
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
		})()`, &pageStr)

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

	if len(images) > 0 {
		log.Printf("[api] image chat request: text=%q, images=%d, msgs=%d", text, len(images), len(req.Messages))
	} else if text != "" {
		log.Printf("[api] text chat request: text=%q, msgs=%d", text, len(req.Messages))
	} else {
		writeError(w, 400, "no content found in request")
		return
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
	} else {
		log.Printf("[api] continuous chat (reusing existing conversation, msgs=%d)", len(req.Messages))
	}

	if shouldNewConv {
		log.Println("[api] starting new conversation")
		t0 := time.Now()
		if err := h.chatHandler.NewConversation(ctx); err != nil {
			log.Printf("[api] new conversation failed: %v", err)
		}
		log.Printf("[api⏱] NewConversation: %dms", time.Since(t0)/time.Millisecond)
	}

	var resp *browser.ChatResponse
	var err error

	t0 := time.Now()
	if len(images) > 0 {
		resp, err = h.chatHandler.SendImageChat(ctx, &browser.ChatRequest{
			Text:   text,
			Images: images,
		})
	} else {
		resp, err = h.chatHandler.SendTextChat(ctx, text)
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
			}
		}
	}
	if text == "" && len(images) > 0 {
		text = "请识别图片中的内容"
	}
	return text, images
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(errorResponse{Error: struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	}{Message: msg, Type: "error"}})
}
