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
	mux.HandleFunc("/healthz", h.handleHealth)
	return mux
}

func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(200)
	w.Write([]byte(`{"status":"ok"}`))
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "invalid request body")
		return
	}

	text, images := extractContent(req.Messages)
	if len(images) == 0 {
		writeError(w, 400, "no images found in request, use API mode for text-only chat")
		return
	}

	log.Printf("[api] image chat request: text=%q, images=%d", text, len(images))

	ctx := context.Background()
	resp, err := h.chatHandler.SendImageChat(ctx, &browser.ChatRequest{
		Text:   text,
		Images: images,
	})
	if err != nil {
		log.Printf("[api] image chat error: %v", err)
		writeError(w, 500, fmt.Sprintf("image chat failed: %v", err))
		return
	}

	w.Header().Set("Content-Type", "application/json")
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
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(errorResponse{Error: struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	}{Message: msg, Type: "error"}})
}
