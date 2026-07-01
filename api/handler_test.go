package api

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestExtractContentTextOnly(t *testing.T) {
	messages := []chatMessage{
		{Role: "user", Content: contentParts{{Type: "text", Text: "你好"}}},
	}
	text, images := extractContent(messages)
	if text != "你好" {
		t.Errorf("extractContent() text = %q, want %q", text, "你好")
	}
	if len(images) != 0 {
		t.Errorf("extractContent() images = %v, want empty", images)
	}
}

func TestExtractContentImageOnly(t *testing.T) {
	messages := []chatMessage{
		{Role: "user", Content: contentParts{
			{Type: "image_url", ImageURL: &struct {
				URL string `json:"url"`
			}{URL: "data:image/png;base64,iVBORw0KGgo="}},
		}},
	}
	text, images := extractContent(messages)
	if text != "请识别图片中的内容" {
		t.Errorf("extractContent() text = %q, want default prompt", text)
	}
	if len(images) != 1 {
		t.Errorf("extractContent() images = %v, want 1 image", images)
	}
}

func TestExtractContentMixed(t *testing.T) {
	messages := []chatMessage{
		{Role: "user", Content: contentParts{
			{Type: "text", Text: "这张图是什么"},
			{Type: "image_url", ImageURL: &struct {
				URL string `json:"url"`
			}{URL: "data:image/png;base64,abc="}},
		}},
	}
	text, images := extractContent(messages)
	if text != "这张图是什么" {
		t.Errorf("extractContent() text = %q, want %q", text, "这张图是什么")
	}
	if len(images) != 1 {
		t.Errorf("extractContent() images = %v, want 1 image", images)
	}
}

func TestExtractContentLastUserOnly(t *testing.T) {
	messages := []chatMessage{
		{Role: "system", Content: contentParts{{Type: "text", Text: "be helpful"}}},
		{Role: "user", Content: contentParts{{Type: "text", Text: "first"}}},
		{Role: "assistant", Content: contentParts{{Type: "text", Text: "response"}}},
		{Role: "user", Content: contentParts{{Type: "text", Text: "second"}}},
	}
	text, images := extractContent(messages)
	if text != "second" {
		t.Errorf("extractContent() should return last user message, got %q", text)
	}
	if len(images) != 0 {
		t.Errorf("extractContent() images = %v, want empty", images)
	}
}

func TestExtractContentNoUser(t *testing.T) {
	messages := []chatMessage{
		{Role: "assistant", Content: contentParts{{Type: "text", Text: "hello"}}},
	}
	text, images := extractContent(messages)
	if text != "" {
		t.Errorf("extractContent() with no user should return empty text, got %q", text)
	}
	if len(images) != 0 {
		t.Errorf("extractContent() images = %v, want empty", images)
	}
}

func TestContentPartsUnmarshal(t *testing.T) {
	tests := []struct {
		name string
		json string
		want string
	}{
		{"plain string", `"hello"`, "hello"},
		{"empty string", `""`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cp contentParts
			if err := json.Unmarshal([]byte(tt.json), &cp); err != nil {
				t.Fatalf("Unmarshal error: %v", err)
			}
			if len(cp) != 1 {
				t.Fatalf("expected 1 part, got %d", len(cp))
			}
			if cp[0].Type != "text" {
				t.Errorf("expected type 'text', got %q", cp[0].Type)
			}
			if cp[0].Text != tt.want {
				t.Errorf("expected text %q, got %q", tt.want, cp[0].Text)
			}
		})
	}
}

func TestContentPartsUnmarshalArray(t *testing.T) {
	data := `[{"type":"text","text":"hello"},{"type":"image_url","image_url":{"url":"data:image/png;base64,abc"}}]`
	var cp contentParts
	if err := json.Unmarshal([]byte(data), &cp); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	if len(cp) != 2 {
		t.Fatalf("expected 2 parts, got %d", len(cp))
	}
	if cp[0].Type != "text" || cp[0].Text != "hello" {
		t.Errorf("first part should be text/hello, got %s/%s", cp[0].Type, cp[0].Text)
	}
	if cp[1].Type != "image_url" {
		t.Errorf("second part type should be image_url, got %s", cp[1].Type)
	}
}

func TestWriteError(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		wantStatus int
		wantMsg    string
		wantType   string
	}{
		{"bad request", 400, 400, "bad request", "error"},
		{"unauthorized", 401, 401, "unauthorized", "error"},
		{"method not allowed", 405, 405, "method not allowed", "error"},
		{"internal error", 500, 500, "chat failed: timeout", "error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			writeError(w, tt.statusCode, tt.wantMsg)

			resp := w.Result()
			if resp.StatusCode != tt.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}

			var errResp errorResponse
			if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
				t.Fatalf("decode error response: %v", err)
			}
			resp.Body.Close()

			if errResp.Error.Message != tt.wantMsg {
				t.Errorf("error message = %q, want %q", errResp.Error.Message, tt.wantMsg)
			}
			if errResp.Error.Type != tt.wantType {
				t.Errorf("error type = %q, want %q", errResp.Error.Type, tt.wantType)
			}

			// Verify content-type header
			ct := resp.Header.Get("Content-Type")
			if ct != "application/json; charset=utf-8" {
				t.Errorf("Content-Type = %q, want %q", ct, "application/json; charset=utf-8")
			}
		})
	}
}

func TestExtractContentMultiImage(t *testing.T) {
	messages := []chatMessage{
		{Role: "user", Content: contentParts{
			{Type: "text", Text: "describe these"},
			{Type: "image_url", ImageURL: &struct{ URL string `json:"url"` }{URL: "data:image/png;base64,aaa"}},
			{Type: "image_url", ImageURL: &struct{ URL string `json:"url"` }{URL: "data:image/png;base64,bbb"}},
		}},
	}
	text, images := extractContent(messages)
	if text != "describe these" {
		t.Errorf("text = %q, want %q", text, "describe these")
	}
	if len(images) != 2 {
		t.Errorf("images len = %d, want 2", len(images))
	}
}

func TestExtractContentMultipleUserMessages(t *testing.T) {
	// Should get the LAST user message, not the first
	messages := []chatMessage{
		{Role: "user", Content: contentParts{{Type: "text", Text: "first"}}},
		{Role: "assistant", Content: contentParts{{Type: "text", Text: "reply"}}},
		{Role: "user", Content: contentParts{{Type: "text", Text: "final"}}},
	}
	text, _ := extractContent(messages)
	if text != "final" {
		t.Errorf("text = %q, want %q", text, "final")
	}
}

func TestExtractContentSystemOnly(t *testing.T) {
	messages := []chatMessage{
		{Role: "system", Content: contentParts{{Type: "text", Text: "you are helpful"}}},
	}
	text, _ := extractContent(messages)
	if text != "" {
		t.Errorf("text = %q, want empty", text)
	}
}

func TestExtractContentEmptyTextWithImages(t *testing.T) {
	messages := []chatMessage{
		{Role: "user", Content: contentParts{
			{Type: "image_url", ImageURL: &struct{ URL string `json:"url"` }{URL: "data:image/png;base64,x"}},
		}},
	}
	text, images := extractContent(messages)
	if text != "请识别图片中的内容" {
		t.Errorf("text = %q, want default image prompt", text)
	}
	if len(images) != 1 {
		t.Errorf("images = %v, want 1", images)
	}
}

func TestContentPartsEmptyArray(t *testing.T) {
	data := `[]`
	var cp contentParts
	if err := json.Unmarshal([]byte(data), &cp); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	if len(cp) != 0 {
		t.Errorf("expected 0 parts, got %d", len(cp))
	}
}
