package browser

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestDeduplicateContent(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "empty string",
			input: "",
			want:  "",
		},
		{
			name:  "single line",
			input: "hello",
			want:  "hello",
		},
		{
			name:  "no duplicates",
			input: "line1\nline2\nline3",
			want:  "line1\nline2\nline3",
		},
		{
			name:  "adjacent duplicate lines",
			input: "hello\nhello\nworld",
			want:  "hello\nworld",
		},
		{
			name:  "non-adjacent duplicates preserved",
			input: "a\nb\na",
			want:  "a\nb\na",
		},
		{
			name:  "incremental update (SSE growth, both >20 chars)",
			input: "this is a short line that is long enough\nthis is a short line that is long enough and continues",
			want:  "this is a short line that is long enough and continues",
		},
		{
			name:  "markdown separator dedup",
			input: "text\n---\n---\nmore",
			want:  "text\n---\nmore",
		},
		{
			name:  "*** separator dedup",
			input: "a\n***\n***\nb",
			want:  "a\n***\nb",
		},
		{
			name:  "empty line as separator",
			input: "a\n\n\nb",
			want:  "a\n\nb",
		},
		{
			name:  "realistic SSE scenario (short lines preserved)",
			input: "你好\n你好\n你好世界",
			want:  "你好\n你好世界",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := deduplicateContent(tt.input, "")
			if got != tt.want {
				t.Errorf("deduplicateContent() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDeduplicateSingleLineRepeat(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		question string
		want     string
	}{
		{name: "empty", input: "", question: "", want: ""},
		{name: "short preserved", input: "abc", question: "", want: "abc"},
		// P1 场景：DeepSeek SSE 先发部分后发完整，如 "2"+"1+1=2"="21+1=2"
		{name: "partial prefix single char", input: "21+1=2", question: "", want: "1+1=2"},
		{name: "partial prefix two chars", input: "=21+1=2", question: "", want: "1+1=2"},
		// P1 场景：DeepSeek SSE 先发完整后发部分，如 "1+1=2"+"1+1"="1+1=21+1"
		{name: "full then partial repeat", input: "1+1=21+1", question: "", want: "1+1=2"},
		{name: "full then partial repeat v2", input: "1+1=21+1=2", question: "", want: "1+1=2"},
		// 不含空格约束保护：含空格的不应当作"部分+完整"模式1
		{name: "with space not deduped", input: "1+1=2 1+1", question: "", want: "1+1=2 1+1"},
		// "X + 分隔符 + X" 模式（三重拦截器各自捕获同一完整内容并加分隔符）
		{name: "triple interceptor repeat", input: "1+1=21+1=2", question: "", want: "1+1=2"},
		{name: "triple interceptor with space sep", input: "1+1=2 1+1=2", question: "", want: "1+1=2"},
		// 不应误删普通内容
		{name: "normal content preserved", input: "Hello World!", question: "", want: "Hello World!"},
		{name: "long normal content", input: "这是一段普通的中文内容，不应被去重。", question: "", want: "这是一段普通的中文内容，不应被去重。"},
		// 边界：n<6 不处理
		{name: "too short", input: "abab", question: "", want: "abab"},
		// 不应误判短重复（full 长度 < 4）
		{name: "short full not deduped", input: "abcab", question: "", want: "abcab"},
		// 模式3：问题回显剥离 [Disabled 2026-07-20]
		// 禁用原因：该模式对每一行单独处理，会误删 JSON 字段值（如 "LIMIT"、6.11、"next_day_expect"）
		// 原始目标（处理 JSON 后追加的对话标题）已由 trimTrailingExtra 用括号配对精确接管
		// 以下 case 的 want 反映模式3 禁用后的实际行为：短输入原样返回，不剥离
		{name: "echo strip disabled: 42+2 with question 2+2=?", input: "42+2", question: "2+2=? answer with number only", want: "42+2"},
		{name: "echo strip disabled: 21+1=? with question 1+1=?", input: "21+1=?", question: "1+1=? answer with number only", want: "21+1=?"},
		{name: "echo strip disabled: normal answer preserved", input: "Paris", question: "what is the capital of France?", want: "Paris"},
		{name: "echo strip disabled: no question no strip", input: "42+2", question: "", want: "42+2"},
		{name: "echo strip disabled: long content no strip", input: "This is a long response that should not be stripped of any echo", question: "echo", want: "This is a long response that should not be stripped of any echo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := deduplicateSingleLineRepeat(tt.input, tt.question)
			if got != tt.want {
				t.Errorf("deduplicateSingleLineRepeat(%q, %q) = %q, want %q", tt.input, tt.question, got, tt.want)
			}
		})
	}
}

func TestLineLevelDedup(t *testing.T) {
	tests := []struct {
		name  string
		input []string
		want  []string
	}{
		{
			name:  "nil slice",
			input: nil,
			want:  nil,
		},
		{
			name:  "empty slice",
			input: []string{},
			want:  []string{},
		},
		{
			name:  "single element",
			input: []string{"hello"},
			want:  []string{"hello"},
		},
		{
			name:  "dedup adjacent",
			input: []string{"a", "a", "b"},
			want:  []string{"a", "b"},
		},
		{
			name:  "short lines not merged below 20-char threshold",
			input: []string{"short", "short and longer text here"},
			want:  []string{"short", "short and longer text here"},
		},
		{
			name:  "short line not treated as incremental",
			input: []string{"ab", "abc"},
			want:  []string{"ab", "abc"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := lineLevelDedup(tt.input)
			if len(got) != len(tt.want) {
				t.Fatalf("lineLevelDedup() = %v (len=%d), want %v (len=%d)", got, len(got), tt.want, len(tt.want))
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("lineLevelDedup()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestIsMarkdownSeparator(t *testing.T) {
	tests := []struct {
		name string
		line string
		want bool
	}{
		{"empty string", "", true},
		{"just spaces", "   ", true},
		{"three dashes", "---", true},
		{"dashes with spaces", " --- ", true},
		{"three asterisks", "***", true},
		{"three underscores", "___", true},
		{"four dashes", "----", true},
		{"not separator - text", "hello", false},
		{"not separator - short dash", "--", false},
		{"not separator - text after dashes", "---text", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isMarkdownSeparator(tt.line); got != tt.want {
				t.Errorf("isMarkdownSeparator(%q) = %v, want %v", tt.line, got, tt.want)
			}
		})
	}
}

func TestHasServerBusy(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{"empty", "", false},
		{"正常内容", "你好，有什么可以帮助你的？", false},
		{"服务器繁忙", "服务器繁忙，请稍后重试", true},
		{"请稍后重试", "系统检测到异常，请稍后重试", true},
		{"消息发送过于频繁", "消息发送过于频繁，请稍后再试", true},
		{"请稍后再试", "请稍后再试", true},
		{"发送过于频繁", "发送过于频繁", true},
		{"部分匹配-服务", "服务正在进行维护", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasServerBusy(tt.content); got != tt.want {
				t.Errorf("hasServerBusy(%q) = %v, want %v", tt.content, got, tt.want)
			}
		})
	}
}

func TestHasConvLimit(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{"empty", "", false},
		{"正常内容", "继续我们的对话", false},
		{"达到对话长度上限", "已达到对话长度上限", true},
		{"请开启新对话", "请开启新对话继续", true},
		{"对话长度上限", "已达到对话长度上限，请开启新对话", true},
		{"部分匹配-新对话", "让我们开始新的对话吧", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasConvLimit(tt.content); got != tt.want {
				t.Errorf("hasConvLimit(%q) = %v, want %v", tt.content, got, tt.want)
			}
		})
	}
}

func TestSendTextChatEmptyText(t *testing.T) {
	h := &ChatHandler{}
	_, err := h.SendTextChat(context.TODO(), "", false)
	if err == nil {
		t.Error("SendTextChat with empty text should return error")
	}
}

func TestSendImageChatNoImages(t *testing.T) {
	h := &ChatHandler{}
	_, err := h.SendImageChat(context.TODO(), &ChatRequest{Text: "hello"}, false)
	if err == nil {
		t.Error("SendImageChat with no images should return error")
	}
}

func TestNewChatHandlerDefaultTimeout(t *testing.T) {
	h := NewChatHandler(nil, 0)
	if h.responseTimeout != 120*time.Second {
		t.Errorf("default timeout should be 120s, got %v", h.responseTimeout)
	}
}

func TestNewChatHandlerCustomTimeout(t *testing.T) {
	h := NewChatHandler(nil, 60)
	if h.responseTimeout != 60*time.Second {
		t.Errorf("expected 60s timeout, got %v", h.responseTimeout)
	}
}

func TestShouldNewConversation_NoActivity(t *testing.T) {
	h := NewChatHandler(nil, 120)
	// lastActivity is zero, should return true
	if !h.ShouldNewConversation() {
		t.Error("ShouldNewConversation() with zero lastActivity should return true")
	}
}

func TestShouldNewConversation_RecentActivity(t *testing.T) {
	h := NewChatHandler(nil, 120)
	h.lastActivity.Store(time.Now().UnixNano())
	if h.ShouldNewConversation() {
		t.Error("ShouldNewConversation() with recent activity should return false")
	}
}

func TestShouldNewConversation_StaleActivity(t *testing.T) {
	h := NewChatHandler(nil, 120)
	h.lastActivity.Store(time.Now().Add(-11 * time.Minute).UnixNano())
	if !h.ShouldNewConversation() {
		t.Error("ShouldNewConversation() after 11 min should return true")
	}
}

// newTestSession 构造指定登录态的 Session（loggedIn 为 atomic.Bool，不能字面量初始化）
func newTestSession(loggedIn bool) *Session {
	s := &Session{}
	s.loggedIn.Store(loggedIn)
	return s
}

func TestShouldRefreshPage_IdleNeverRefreshed(t *testing.T) {
	h := NewChatHandler(newTestSession(true), 120)
	// lastActivity 为零（视为长时间空闲）且从未刷新过 → 应刷新
	if !h.shouldRefreshPage() {
		t.Error("should refresh when idle long and never refreshed")
	}
}

func TestShouldRefreshPage_JustRefreshed(t *testing.T) {
	h := NewChatHandler(newTestSession(true), 120)
	h.lastRefresh.Store(time.Now().UnixNano())
	// 刚刷新过，即使空闲超 30 分钟也不应重复刷新
	if h.shouldRefreshPage() {
		t.Error("should not refresh within 30min of last refresh")
	}
}

func TestShouldRefreshPage_Active(t *testing.T) {
	h := NewChatHandler(newTestSession(true), 120)
	h.lastRefresh.Store(time.Now().Add(-31 * time.Minute).UnixNano())
	h.lastActivity.Store(time.Now().UnixNano())
	// 距上次刷新超 30 分钟但最近有请求 → 不应刷新
	if h.shouldRefreshPage() {
		t.Error("should not refresh when active")
	}
}

func TestShouldRefreshPage_StaleRefreshNotLoggedIn(t *testing.T) {
	h := NewChatHandler(newTestSession(false), 120)
	h.lastRefresh.Store(time.Now().Add(-31 * time.Minute).UnixNano())
	// 空闲且距上次刷新超 30 分钟，但未登录（账号切换中）→ 不应刷新
	if h.shouldRefreshPage() {
		t.Error("should not refresh when not logged in")
	}
}

func TestShouldRefreshPage_StaleActivityAndRefresh(t *testing.T) {
	h := NewChatHandler(newTestSession(true), 120)
	h.lastRefresh.Store(time.Now().Add(-31 * time.Minute).UnixNano())
	h.lastActivity.Store(time.Now().Add(-31 * time.Minute).UnixNano())
	// 空闲超 30 分钟且距上次刷新超 30 分钟 → 应刷新
	if !h.shouldRefreshPage() {
		t.Error("should refresh when both idle and last refresh stale")
	}
}

func TestChatRequestUnmarshal(t *testing.T) {
	data := `{"text":"hello","images":["url1","url2"],"model":"test"}`
	var req ChatRequest
	if err := json.Unmarshal([]byte(data), &req); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	if req.Text != "hello" {
		t.Errorf("Text = %q, want %q", req.Text, "hello")
	}
	if len(req.Images) != 2 {
		t.Errorf("Images len = %d, want 2", len(req.Images))
	}
	if req.Model != "test" {
		t.Errorf("Model = %q, want %q", req.Model, "test")
	}
}

func TestChatResponseMarshal(t *testing.T) {
	resp := ChatResponse{
		Content:  "hello world",
		Thinking: "let me think...",
	}
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}
	var decoded ChatResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	if decoded.Content != "hello world" {
		t.Errorf("Content = %q, want %q", decoded.Content, "hello world")
	}
	if decoded.Thinking != "let me think..." {
		t.Errorf("Thinking = %q, want %q", decoded.Thinking, "let me think...")
	}
}

func TestChatResponseNoThinking(t *testing.T) {
	resp := ChatResponse{
		Content: "simple answer",
	}
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}
	var decoded ChatResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	if decoded.Thinking != "" {
		t.Errorf("Thinking should be omitted, got %q", decoded.Thinking)
	}
	_ = data
}

func TestNewChatHandlerNegativeTimeout(t *testing.T) {
	h := NewChatHandler(nil, -1)
	if h.responseTimeout != 120*time.Second {
		t.Errorf("negative timeout should default to 120s, got %v", h.responseTimeout)
	}
}

func TestNewChatHandlerZeroTimeout(t *testing.T) {
	h := NewChatHandler(nil, 0)
	if h.responseTimeout != 120*time.Second {
		t.Errorf("zero timeout should default to 120s, got %v", h.responseTimeout)
	}
}
