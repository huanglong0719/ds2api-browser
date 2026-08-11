# ds2api-browser

基于 Chrome 浏览器的 DeepSeek API 代理服务。通过 chromedp 操控浏览器登录 DeepSeek 网页版，支持**文本聊天**和**图片识别**两种模式，自动捕获 SSE 流式响应并分离思考内容与回复内容。

> **模式支持现状（2026-08-09 探测确认）**：DeepSeek 网页版现有 **快速 / 专家 / 识图** 三种模式。本程序当前支持其中两种——**快速模式**（文本聊天默认）和**识图模式**（发图片自动切换）；**专家模式尚未支持**（程序没有切换专家模式的代码，已列入待办，暂不改动）。详情见"已知限制"一节。

## 功能特性

- **文本聊天** — 纯文本消息，自动切换到快速（默认）模式发送
- **图片识别 (识图模式)** — 上传图片到 DeepSeek 识图模式，获取 AI 视觉分析结果
- **连续对话** — 重用现有浏览器标签页，同一话题可连续多轮对话
- **智能新对话检测** — 自动判断是否需要开启新对话（空闲超过 10 分钟，或当前对话累计字符数达到随机阈值 60-90 万）
- **手动新对话控制** — 通过 `X-New-Conversation` 头或 `?new=true` 参数显式控制
- **深度思考分离** — 解析 SSE 流中的 `THINK` 和 `RESPONSE` 片段，分别返回 `reasoning_content` 和 `content`，自动适配 DeepSeek 协议变化（纯 v 增量包格式）
- **OpenAI 兼容接口** — 标准的 `/v1/chat/completions` 端点，支持第三方客户端直接连接
- **流式/非流式响应** — 同时支持 `stream: true/false`
- **并发安全** — 内置互斥锁保护，串行处理请求避免浏览器冲突
- **长文本直接输入** — 超长文本（超过 3000 字符）直接分块输入 textarea，不生成临时文件（原"自动转 txt 上传"功能已于 2026-07 移除）
- **文件附件支持** — 客户端可通过 `file` / `file_url` 类型发送文件附件（`.txt` / `.pdf` / `.csv` 等），自动上传到 DeepSeek 输入框
- **标签页自动清理** — 启动时和每次请求前自动关闭多余标签页，仅保留一个 DeepSeek 聊天页，减少内存占用
- **空闲保活刷新** — 长时间无请求（如收盘到复盘之间数小时）时后台每 30 分钟自动刷新页面，保持 Chrome 渲染进程活跃，避免请求突然到来时因页面休眠而卡顿

## 已知限制与实测记录

### 模式支持（2026-08-09 实测）
- DeepSeek 网页版现有 **快速 / 专家 / 识图** 三种模式，本程序当前支持 **快速** 和 **识图** 两种，**专家模式暂未支持**（程序无切换专家模式的代码，`getCurrentModeJS` 不认识"专家"关键词）。当前若页面处于专家模式，文本请求会被当快速模式处理，不做切换。
- **专家模式支持已列入待办**：计划为 `switchMode` 增加 expert 分支（仿照识图模式切换实现），并在 API 增加 `mode=expert` 触发入口。**因当前用户要求暂不改动代码，故先记录于此。**

### 识图模式真实测试记录（2026-08-09）
- 测试方法：生成含红色圆形、蓝色长方形、文字 "TEST 123" 的 PNG，通过 `/v1/chat/completions` 发送。
- 结果：**5 次真实请求全部成功**（含服务重启后首请求），AI 正确识别形状、颜色、文字，深度思考正常，`model_type=vision` 生效。
- 完整链路日志：切识图模式（~819ms）→ 上传图片（~549ms）→ 输入文字 → 发送（一次成功）→ AI 思考 → 响应捕获，单次请求全程约 7 秒。
- 曾观察到 1 次"服务刚启动后首请求切换模式失败"（`failed to switch to image mode`），重启复现 5 次均未再出现，判定为启动瞬间 DOM 未就绪的偶发边缘情况，非系统性问题。

### 模式切换 DOM 要点（详见 DOM_STRUCTURE.md）
- 模式 radio 仅在新对话首页渲染（`DIV[role="radio"]`）；**进入对话后 radio 消失**，程序靠图片预览元素回退判断识图模式。
- JS `r.click()` 对模式 radio 有效（2026-08-09 CDP 实测：点击后 `aria-checked` 从 false 变 true）。

### 发送后 0-3 秒错误检测（2026-08-09 修复 Toast 漏检）
- 发送后 3 秒内（50ms 间隔）检测系统提示与思考状态，检测结果决定后续动作（切换账号 / 新对话 / 等待回复）。
- **检测失败会影响后续步骤**：漏检系统提示会导致干等 120 秒超时。完整失败影响矩阵见 [DOM_STRUCTURE.md 4.5 节](file:///d:/ds2api-browser/DOM_STRUCTURE.md)。（注：textarea 实测任何时刻均不禁用，原"误判 inputDisabled"风险不存在，相关死代码已删除）
- 2026-08-09 修复：右上角弹窗（Toast）形式的系统提示此前漏检（检测脚本只查消息列表，扫不到弹窗；且服务器繁忙时页面可能无消息），已修复并实测验证。

### 页面重载后输入发送（2026-08-09 修复）
- **现象**：新对话/刷新后立刻输入长文本再按 Enter，消息发不出去（事件到达输入框但不触发发送）。
- **根因**：新对话/idle 刷新后页面被销毁重建（日志 TARGET DESTROYED + 拦截器重新注入），React 事件监听尚未挂载完成，程序输入到了"半新页面"。
- **修复**：拦截器注入入口检测到页面刚重载（拦截器丢失）时，先等页面稳定再继续——判据为输入框可见 + placeholder 已渲染（React 已接管输入区）+ 连续 3 次稳定（`waitForPageStable`，超时 10s 只告警不中断）。**检测只在页面重载时触发，连续对话自动跳过**。
- **覆盖范围**：所有发送路径（正常发送 / 换号重试 / 新对话重试 / 回首页重试）。
- **验证**：新对话 → 连续对话 ×2 → 新对话 4 次请求全部一次发送成功。

### 服务启动（2026-08-09 实测）
- 必须前台直接运行 `.\ds2api-browser.exe`；后台 `Start-Process` 启动会导致 Chrome 自动关闭、服务退出。

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

> ⚠️ **必须前台运行（2026-08-09 实测）**：请直接在前台运行 `.\ds2api-browser.exe`，**不要**用 `Start-Process -RedirectStandardOutput/-RedirectStandardError` 等方式后台启动——实测后台启动会导致服务自带的 Chrome 在启动数秒后自动关闭、服务随之退出（前台运行则完全正常）。

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
| 距离上次请求超过 10 分钟 | 开启新对话 | 空闲超时（`ShouldNewConversation`） |
| 当前对话累计字符数达到随机阈值（60-90 万，含提示词+思考+回复） | 开启新对话 | 字符计数（`ShouldNewConversationByCount`） |
| 显式指定 `X-New-Conversation: true` | 强制新对话 | HTTP 头覆盖 |
| 显式指定 `?new=true` | 强制新对话 | URL 参数覆盖 |
| 配置 `auto_new_conversation: true` | 每次强制新对话 | 配置覆盖 |

> 注 1：自动新对话判断依据是**空闲时长**和**累计字符数**（60-90 万），不检查 assistant 历史记录（README 旧版描述"无 assistant 历史"已不准确）。
> 注 2（2026-08-11）：累计字符数统计口径为"**提示词 + 思考 + 回复**"三者之和，与 DeepSeek 服务器"对话长度上限"（约 100 万字符）的计算口径一致。此前只累加回复字符数导致主动检查永远追不上服务器上限（服务器先触发系统提示），已修正并在 17:09 实盘验证自动开新对话全链路正常。服务器触发"对话长度上限"时自动新开对话重试；新对话后仍触发则视为脚本问题返回错误（新对话为空对话，不可能触发上限）。

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

### 文件上传请求

支持通过 `file` 或 `file_url` 类型发送文件附件：

```json
{
  "model": "deepseek-chat",
  "messages": [
    {
      "role": "user",
      "content": [
        {"type": "text", "text": "分析这个文件"},
        {"type": "file_url", "file_url": {"url": "https://example.com/report.pdf"}}
      ]
    }
  ]
}
```

也支持 data URI 和 base64 编码的文件数据：

```json
{
  "role": "user",
  "content": [
    {"type": "file", "file": {"file_data": "data:text/plain;base64,SGVsbG8gV29ybGQ=", "filename": "hello.txt"}}
  ]
}
```

长文本（超过 3000 字符）会直接分块输入 textarea 发送，无需客户端额外操作（不再自动转 txt 文件上传）。

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
2. **智能模式切换** — 根据请求内容自动切换"快速模式（默认）"或"识图模式"，无需客户端关心底层细节。对话进行中 radio 按钮隐藏时，自动通过图片预览元素回退检测当前模式。
3. **多级发送保障** — 输入完成后按顺序尝试：Enter 键（手动 keyDown+keyUp，无 char 事件）→ Ctrl+Enter 组合键 → MouseClickXY 坐标点击 → JS 事件序列 → 键盘回车兜底，确保消息在各种场景下都能成功发送。注：不能用 `SendKeys("\r")` 发送 Enter（其 char 事件会把换行符插入 textarea，导致误判发送失败，详见 DOM_STRUCTURE.md）。
4. **长文本直接输入** — 超过 3000 字符的文本直接分块输入 textarea（每块 3000 字符），不生成临时文件、不切换模式，避免 textarea 撑大导致按钮移出视口。
5. **文件附件上传** — 客户端 API 请求中的 `file` / `file_url` 类型内容自动提取并上传到 DeepSeek 输入框，支持 data URI / base64 / URL 三种格式。
6. **标签页自动清理** — 启动时和每次请求前通过 Chrome DevTools Protocol HTTP API 关闭多余标签页（`chrome://newtab/` 等），仅保留一个 DeepSeek 聊天页，减少内存占用。
7. **SSE 三重拦截** — fetch + XHR + EventSource 三通道独立拦截，每个通道均完整支持各种 SSE 包格式（含纯 v 字符串增量包），Go 层通过 `deduplicateContent()` 去重，三重保障响应完整性。
8. **并发串行化** — 使用 sync.Mutex 保护 ChatHandler，同一时间只处理一个请求，避免浏览器操作冲突。
9. **超时保护** — 请求级超时由 `response_timeout_sec` 配置（默认 120 秒），防止 Chrome 卡死导致 goroutine 永久阻塞。
10. **空闲保活刷新** — 后台 goroutine 每 5 分钟检查一次：距离上次请求超过 30 分钟且距离上次刷新超过 30 分钟时自动刷新页面唤醒渲染进程。与请求共用互斥锁，活跃对话期间绝不刷新；登录/账号切换期间跳过；随浏览器会话关闭自动退出。

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
│  │   ├── sendMessage()      │ ← 清空→输入→多方式发送→重试
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
│   ├── chat.go                # 聊天核心：模式切换、消息输入、多方式发送、响应等待、重试机制
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
