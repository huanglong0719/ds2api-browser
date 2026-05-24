# ds2api-browser

基于 Chrome 浏览器的 DeepSeek 图片识别 API 服务。通过 chromedp 操控浏览器，自动上传图片并捕获 DeepSeek 识图模式的 SSE 流式响应，分离思考内容和回复内容。

## 与 deepseek-proxy 的配合

```
用户/客户端
    │
    ▼
deepseek-proxy (端口 8765)
    │  ├── 有图片（最后一条用户消息）→ 转发到 ds2api-browser
    │  └── 无图片 → 直连 DeepSeek API
    │
    ▼
ds2api-browser (端口 8766)
    │  操控 Chrome 浏览器 → 登录 DeepSeek → 上传图片 → 捕获 SSE 响应
    │  返回 { content, reasoning_content }
    │
    ▼
deepseek-proxy 将结果转为 SSE 事件流返回给客户端
```

### 关键设计原则

1. **图片检测只看最后一条用户消息**：避免多轮对话中历史图片导致后续纯文本消息被错误路由到浏览器。
2. **思考内容与回复内容分离**：通过解析 DeepSeek SSE 中的 `fragments[].type` 元数据，将 `THINK` 片段内容路由到 `reasoning_content`，`RESPONSE` 片段路由到 `content`。
3. **浏览器会话管理**：每次请求创建独立浏览器上下文，完成后导航回首页，避免会话污染。

## 快速开始

### 前置要求

- Go 1.21+
- Chrome 浏览器
- DeepSeek 网页版账号

### 配置

```bash
cp browser_config.example.json browser_config.json
```

编辑 `browser_config.json`，填入你的 DeepSeek 账号和 API Key。

### 编译运行

```bash
go build -o ds2api-browser.exe .
./ds2api-browser.exe
```

服务启动后监听 `http://127.0.0.1:8766`。

### API

```
POST /v1/chat/completions
Authorization: Bearer <api_key>

{
  "model": "deepseek-v4-pro",
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

响应：

```json
{
  "choices": [{
    "message": {
      "role": "assistant",
      "content": "回复内容...",
      "reasoning_content": "思考过程..."
    }
  }]
}
```

## 项目结构

```
ds2api-browser/
├── main.go              # 入口：启动 HTTP 服务 + 浏览器会话
├── api/
│   └── handler.go       # API 路由：/v1/chat/completions，提取图片并调用 ChatHandler
├── browser/
│   ├── session.go       # 浏览器会话管理：登录、导航、上下文创建
│   ├── chat.go          # 图片聊天核心：模式切换、图片上传、消息发送、响应等待
│   └── injector.go      # JS 拦截器注入：拦截 XHR/fetch/EventSource，解析 SSE 数据流
├── config/
│   └── config.go        # 配置加载
├── cmd/
│   └── minitest/        # 最小化测试工具
├── browser_config.example.json
└── go.mod
```

## 关联项目

- **[deepseek-proxy](https://github.com/huanglong0719/deepseek-proxy)** - Python 协议转换代理，作为主入口路由图片请求到本服务（作为 git submodule 集成在 `vendor/ds2api-browser/`）
- **[ds2api](https://github.com/huanglong0719/ds2api)** - 主项目，完整的 DeepSeek API 代理服务