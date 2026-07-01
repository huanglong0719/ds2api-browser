# ds2api-browser

基于 Chrome 浏览器的 DeepSeek API 代理服务。通过 chromedp 操控浏览器登录 DeepSeek 网页版，支持**文本聊天**和**图片识别**两种模式，自动捕获 SSE 流式响应并分离思考内容与回复内容。

## 功能特性

- **文本聊天** — 纯文本消息，自动切换到默认模式发送
- **图片识别 (识图模式)** — 上传图片到 DeepSeek 识图模式，获取 AI 视觉分析结果
- **连续对话** — 重用现有浏览器标签页，同一话题可连续多轮对话
- **智能新对话检测** — 自动判断是否需要开启新对话（无 assistant 历史记录时）
- **手动新对话控制** — 通过 `X-New-Conversation` 头或 `?new=true` 参数显式控制
- **深度思考分离** — 解析 SSE 流中的 `THINK` 和 `RESPONSE` 片段，分别返回 `reasoning_content` 和 `content`
- **OpenAI 兼容接口** — 标准的 `/v1/chat/completions` 端点，支持第三方客户端直接连接
- **流式/非流式响应** — 同时支持 `stream: true/false`
- **并发安全** — 内置互斥锁保护，串行处理请求避免浏览器冲突

## 快速开始

### 前置要求

- Go 1.24+（使用标准 Go 工具链，go.mod 锁定 go 1.24.0）
- Chrome 浏览器（支持 Chrome 150+，含便携版）
- DeepSeek 网页版账号

### 方式一：一键启动（推荐）

```powershell
# Windows PowerShell
.\start.ps1

# 停止服务
.\stop.ps1
```

### 方式二：手动编译运行

```bash
# 1. 复制配置文件
cp browser_config.example.json browser_config.json

# 2. 编辑配置，填入账号和 API Key

# 3. 编译运行（vendor 目录已包含 cdproto 补丁）
go build -mod=vendor -o ds2api-browser.exe .
./ds2api-browser.exe
```

### 方式三：仅运行（无需编译，直接使用预编译 exe）

```powershell
cd D:\ds2api-browser
.\ds2api-browser.exe
```

### 验证服务

```powershell
# 检查健康状态
Invoke-RestMethod http://127.0.0.1:8766/healthz

# 测试新对话检测
.\check_new_conv.ps1
```

服务启动后监听 `http://127.0.0.1:8766`。

## 配置说明

编辑 `browser_config.json`：

```json
{
  "port": 8766,
  "api_key": "sk-your-api-key",
  "auto_new_conversation": false,
  "accounts": [
    {
      "email": "your-phone-or-email",
      "password": "your-password"
    }
  ]
}
```

| 字段 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `port` | int | 8766 | API 服务监听端口 |
| `api_key` | string | - | 客户端认证密钥（Authorization: Bearer），为空则跳过认证 |
| `response_timeout_sec` | int | 120 | 等待回复的超时时间（秒） |
| `chrome_path` | string | 自动检测 | Chrome 浏览器可执行文件路径 |
| `auto_new_conversation` | bool | false | 是否每次请求都强制开启新对话（true 忽略消息历史，强制新对话）|
| `accounts` | array | - | DeepSeek 登录账号列表，支持多账号轮询切换 |

### 新对话行为

当 `auto_new_conversation: false`（默认）时：

| 场景 | 行为 | 判断依据 |
|------|------|----------|
| 单条用户消息 | 开启新对话 | 无 assistant 历史 |
| 多轮对话（含 assistant 回复） | 继续当前对话 | 有 assistant 历史 |
| 显式指定 `X-New-Conversation: true` | 强制新对话 | HTTP 头覆盖 |
| 显式指定 `?new=true` | 强制新对话 | URL 参数覆盖 |

## API 接口

### 端点

```
POST /v1/chat/completions
GET  /v1/account
POST /v1/account/switch
GET  /v1/debug
GET  /healthz
```

### 认证

```
Authorization: Bearer <api_key>
```

### 文本聊天请求

```json
{
  "model": "deepseek-chat",
  "messages": [
    {"role": "user", "content": "你好"}
  ]
}
```

### 图片识别请求

```json
{
  "model": "deepseek-chat",
  "messages": [
    {
      "role": "user",
      "content": [
        {"type": "text", "text": "这张图片是什么？"},
        {"type": "image_url", "image_url": {"url": "data:image/png;base64,..."}}
      ]
    }
  ]
}
```

### 新对话控制

```bash
# 方式一：HTTP 头
curl -H "X-New-Conversation: true" ...

# 方式二：URL 参数
curl "http://127.0.0.1:8766/v1/chat/completions?new=true" ...
```

### 响应格式（非流式）

```json
{
  "id": "browser-1234567890",
  "object": "chat.completion",
  "created": 1700000000,
  "model": "deepseek-v4-pro",
  "choices": [{
    "index": 0,
    "message": {
      "role": "assistant",
      "content": "回复内容...",
      "reasoning_content": "思考过程..."
    },
    "finish_reason": "stop"
  }]
}
```

### 响应格式（流式 stream: true）

```
data: {"id":"browser-...","object":"chat.completion.chunk","choices":[{"delta":{"role":"assistant","content":"...","reasoning_content":"..."}}]}

data: {"id":"browser-...","object":"chat.completion.chunk","choices":[{"delta":{},"finish_reason":"stop"}]}

data: [DONE]
```

### 错误响应

```json
{"error": {"message": "错误描述", "type": "invalid_request_error"}}
```

### 账号管理

```bash
# 查看所有账号和当前登录账号
curl http://127.0.0.1:8766/v1/account

# 切换到下一个账号（轮询切换）
curl -X POST http://127.0.0.1:8766/v1/account/switch
```

### 调试端点

```bash
# 查看拦截器状态、页面 DOM 摘要、当前 URL
curl http://127.0.0.1:8766/v1/debug
```

返回示例：
```json
{
  "interceptor": {
    "capture": "...",          // 最终回复内容（前500字符）
    "thinking": "...",         // 深度思考内容（前500字符）
    "done": true,              // 拦截器完成标志
    "domDone": true,           // DOM 观察器完成标志
    "log": ["MATCH_F:...", "FRAG_TYPE:RESPONSE", "DONE_F"],  // 拦截日志（最近30条）
    "ptypes": {"RESPONSE": 5, "THINK": 2},  // 片段类型统计
    "convLimit": false,        // 是否检测到对话长度上限
    "serverBusy": false,       // 是否检测到服务器繁忙
    "url": "https://chat.deepseek.com/..."
  },
  "page": {
    "textareaExists": true,    // 输入框是否存在
    "textareaDisabled": false, // 输入框是否被禁用
    "textareaValue": "...",    // 输入框内容（前100字符）
    "articleCount": 3,         // Markdown 消息数量
    "lastArticlePreview": "...", // 最后一条消息预览（前200字符）
    "bodyText": "..."          // 页面 body 文本（前1000字符）
  }
}
```

## 关键设计原则

1. **标签页复用** — 不为每次请求创建新标签页，通过 CDP Target 跟踪重用现有标签页，实现连续对话。
2. **智能模式切换** — 根据请求内容自动切换"默认模式"或"识图模式"，无需客户端关心底层细节。
3. **三段式发送保障** — Enter 键 → JS 事件链点击 → MouseClickXY 坐标点击 → 键盘回车重试，确保消息在各种场景下都能成功发送。
4. **SSE 三重拦截** — fetch + XHR + EventSource 三通道独立拦截，Go 层通过 `deduplicateContent()` 去重，三重保障响应完整性。
5. **并发串行化** — 使用 sync.Mutex 保护 ChatHandler，同一时间只处理一个请求，避免浏览器操作冲突。
6. **超时保护** — 请求级超时由 `response_timeout_sec` 配置（默认 120 秒），防止 Chrome 卡死导致 goroutine 永久阻塞。

## 数据流架构

```
第三方客户端 (ChatBox / LobeChat / 自定义)
    │
    ▼ POST /v1/chat/completions
┌─────────────────────────────┐
│  api/handler.go             │
│  ├── API Key 认证            │
│  ├── extractContent()       │ ← 提取文本/图片
│  ├── 新对话检测              │ ← header/param/history
│  └── 调用 ChatHandler       │
└──────────┬──────────────────┘
           │
           ▼ (mutex 保护, 串行执行)
┌─────────────────────────────┐
│  browser/chat.go            │
│  ├── sendChat(mode)         │ ← 文本/图片统一入口
│  │   ├── switchTo*Mode()   │ ← 模式切换
│  │   ├── injectInterceptor()│ ← 注入 SSE 拦截器
│  │   ├── sendMessage()      │ ← 清空→输入→Enter→重试
│  │   ├── waitForResponse()  │ ← 轮询 __dsBrowserCapture
│  │   └── deduplicateContent()│ ← 三重拦截去重
│  └── NewConversation()     │ ← UI 点击或 Ctrl+J
└──────────┬──────────────────┘
           │ CDP 协议
           ▼
┌─────────────────────────────┐
│  Chrome 浏览器               │
│  chat.deepseek.com          │
│  ┌─────────────────────┐    │
│  │ browser/injector.go  │    │
│  │ 拦截 fetch/XHR/SSE   │    │
│  │ → __dsBrowserCapture │    │
│  │ → __dsBrowserDone    │    │
│  └─────────────────────┘    │
└─────────────────────────────┘
```

## 项目结构

```
ds2api-browser/
├── main.go                    # 入口：加载配置、启动 Chrome、HTTP 服务、优雅关闭
├── api/
│   ├── handler.go             # API 路由、认证、内容提取、新对话检测、响应格式化
│   └── handler_test.go        # API 单元测试（extractContent、writeError 等）
├── browser/
│   ├── session.go             # Chrome 进程管理、CDP 连接、登录、导航、目标跟踪
│   ├── chat.go                # 聊天核心：模式切换、消息输入、三段发送、响应等待
│   ├── chat_test.go           # 聊天核心单元测试（去重、错误检测等）
│   └── injector.go            # JavaScript 拦截器：SSE/EventSource/DOM 观察器注入
├── config/
│   └── config.go              # 配置文件加载与解析
├── vendor/                    # 依赖包（含 cdproto IPAddressSpace Loopback 补丁）
├── cmd/
│   ├── minitest/              # 最小化 Chrome 测试工具
│   ├── capture_requests/      # SSE 捕获验证工具
│   └── alloc_test/            # Chrome 分配器稳定性测试
├── start.ps1                  # 一键启动脚本
├── stop.ps1                   # 一键停止脚本
├── check_new_conv.ps1         # 新对话检测测试脚本
├── browser_config.example.json # 配置模板
├── go.mod / go.sum
├── README.md
└── ds2api-browser.exe          # 编译后的可执行文件
```

## 第三方客户端配置

通用配置项：
- **API Host**: `http://127.0.0.1:8766`
- **API Key**: （browser_config.json 中设置的 api_key）
- **Model**: `deepseek-chat` 或任意值（不传递给 DeepSeek）

支持的客户端：
- ChatBox
- LobeChat
- NextChat
- OpenAI 兼容的任何客户端

## 环境依赖

| 组件 | 版本 | 说明 |
|------|------|------|
| Go | 1.24.0+ (`go 1.24.0` in go.mod) | 标准工具链 |
| chromedp | v0.13.0 | Chrome DevTools Protocol 驱动 |
| cdproto | v0.0.0-20250222（含补丁） | CDP 类型定义，vendor 中已打 `IPAddressSpace Loopback` 补丁 |
| Chrome | 150+ | 便携版位于 `chrome-portable/` |

### 依赖管理

- **vendor 目录**：依赖已 vendor，编译时使用 `go build -mod=vendor`
- **cdproto 补丁**：`vendor/github.com/chromedp/cdproto/network/types.go` 中添加了 `IPAddressSpaceLoopback` 枚举值
- **升级依赖**：修改 go.mod 后执行 `go mod vendor` 重新 vendor

## 测试

```bash
# 运行所有单元测试
go test ./... -v

# 运行特定包测试
go test ./browser/... -v -run TestDeduplicateContent
go test ./api/... -v -run TestExtractContent

# 手动功能验证
Invoke-RestMethod http://127.0.0.1:8766/healthz
Invoke-RestMethod http://127.0.0.1:8766/v1/debug
```
