# DeepSeek 页面 DOM 结构文档（真相源）

> 本文档记录通过 CDP/debug 端点实际探测到的页面元素结构，作为代码修改的依据。
> 任何检测逻辑修改前，必须先核对本文档；新探测到的结构必须追加到本文档。
> 最后更新：2026-08-10（新增 __dsConvLimitHit 拦截器检查；保活轻量唤醒 + 页面重建；新对话按字符数阈值）

## ⚠️ 顶线原则：本文档是"基线"，不是"答案"

DeepSeek 官方会不定期改前端结构（class 名、按钮位置、DOM 层级等）。
当系统出问题时，**第一反应不是改代码，而是用 /v1/debug?msgs=1 重新探测页面**，对照本文档检查：

1. **className 是否变了**（如 `d29f3d7d`、`ds-message`、`ds-assistant-message-main-content` 是否还在）
2. **元素位置是否变了**（如 5 个按钮是否还在 `ds-message` 的兄弟元素里）
3. **按钮数量是否变了**（如 5 个按钮是否变成 4 个或 6 个）
4. **是否新增了元素**（如新出现了某种类型的消息容器）
5. **是否删除了元素**（如某个标志元素消失）

### ⚠️ 系统提示检查原则（2026-07-31 用户教导）

**系统提示出现时绝对不能关闭服务或重启程序！**

系统提示（如"消息发送过于频繁"、"对话长度上限"）由 DeepSeek 服务器触发，可遇不可求。重启后页面刷新，系统提示就消失了，无法再检查 DOM 结构。

正确做法：
1. **保持程序运行不关闭**（可以暂停接收新请求，但不要关闭程序）
2. **立即用 `/v1/debug?msgs=1` 检查页面元素**
3. **把探测到的 DOM 结构记录到本文档**
4. 如果需要修改代码，**先检查完元素再改**，不要急于重启

### 发现变化时的处理流程
1. 在本文档"变更记录"追加一条：日期 + 变化点 + 探测证据
2. 更新相关章节的结构描述
3. 同步修改依赖该结构的代码
4. 不要在不更新文档的情况下直接改代码（否则下次又会失忆）

### 没发现变化时
说明是逻辑问题，不是结构问题，继续按现有文档分析。

---

## 一、消息容器总览

页面所有消息都用 `[class*="ds-message"]` 作为外层容器。
通过 `document.querySelectorAll('[class*="ds-message"]')` 可拿到所有消息，按出现顺序排列（index=0 是最早的消息，最后一个 index 是最新消息）。

### 消息类型区分（关键判据）

| 类型 | className 特征 | 含 ds-assistant-message-main-content | 含 ds-markdown | 内部按钮数 |
|------|---------------|--------------------------------------|----------------|-----------|
| 用户消息 | **含 `d29f3d7d`** 前缀，如 `d29f3d7d ds-message _63c77b1` | 否 | 否 | 0 |
| AI 回复 | 不含 `d29f3d7d`，如 `ds-message _63c77b1` | **是** | **是** | 0 |
| AI 思考 | 不是独立消息，是 AI 回复内部的子元素 | — | — | — |
| 系统提示 | 不含 `d29f3d7d`，不含 `ds-assistant-message-main-content`（待复现验证） | 否 | 否 | 0 |

**最快区分用户消息 vs AI 回复**：看 className 是否含 `d29f3d7d`。
- 含 → 用户消息
- 不含 + 含 `ds-assistant-message-main-content` → AI 回复
- 不含 + 不含 `ds-assistant-message-main-content` → 系统提示（推测）

---

## 二、各类型消息详细结构

### 1. 用户消息（已验证 2026-07-31）

```text
DIV.d29f3d7d.ds-message._63c77b1           ← 用户消息容器（注意 d29f3d7d 是用户消息特有）
└── DIV.fbb737a4                            ← 文本容器
    └── (用户输入的文字)
```

- textPreview 示例：`1+1=? ??????`
- childCount: 1
- buttonCount: 0
- hasAssistantContent: false
- hasMarkdown: false

### 2. AI 回复消息（已验证 2026-07-31）

```text
DIV.ds-message._63c77b1                     ← AI 回复容器（无 d29f3d7d）
├── DIV._74c0879                            ← 思考过程容器
│   └── (已思考/正在思考 文字 + 思考内容)
└── DIV.ds-markdown.ds-assistant-message-main-content   ← 回复正文
    └── (AI 回复的 markdown 内容)
```

- childCount: 2
- buttonCount: 0  ⚠️ **重要：按钮不在 ds-message 内部！**
- hasAssistantContent: true
- hasMarkdown: true

textPreview 示例：`已思考（用时 1 秒）我们需要回答用户问题...`

### 3. AI 思考（已验证 2026-07-31）

**思考不是独立消息**，是 AI 回复消息内部的子元素 `DIV._74c0879`。

- 文字特征：
  - 进行中：`正在思考...`
  - 完成：`已思考（用时 X 秒）`
- 位于 AI 回复消息的第 1 个子元素
- 思考内容跟在"已思考（用时 X 秒）"文字之后

### 4. 系统提示（已复现 2026-07-31）

**已复现场景**："消息发送过于频繁，请稍后重试"

#### 真实 DOM 结构（来自 `/v1/debug?msgs=1`）

```
DIV.d29f3d7d.ds-message._63c77b1            ← 最后一条是用户消息
└── DIV.fbb737a4                            ← 用户消息内容容器
    └── (用户发送的问题)

DIV._11d6b3a                                 ← ✅ 系统提示在这里！
└── "消息发送过于频繁，请稍后重试"
```

#### 关键发现

1. **系统提示不在 `ds-message` 内部**，而在**最后一个 `ds-message` 的下一个兄弟元素**中
2. 系统提示元素 className：`DIV._11d6b3a`
3. 系统提示旁有 2 个操作按钮（复制/编辑），这是用户之前描述的特征
4. 系统提示出现时：**最后一个 ds-message 是用户消息**（`d29f3d7d`），不是 AI 回复
5. 无思考过程、无 5 个操作按钮

#### 已观测到的文字

- `消息发送过于频繁，请稍后重试`
- `达到对话长度上限，请开启新对话`（用户历史截图，待再次复现确认结构）
- `服务器繁忙`（待复现）
- `有消息正在生成`（待复现）

#### 检测要点

检测系统提示时必须同时检查：
1. 最后一个 `ds-message` 的 `textContent`
2. 最后一个 `ds-message` 的 `nextElementSibling.textContent`

当前 `errorDetectJS` 已按此实现（chat.go 第42-91行，先检查 lastMsg 及其兄弟元素，再依次检测 serverBusy → convLimit → thinking）

---

## 三、操作按钮结构（关键发现 2026-07-31）

### 5 个操作按钮
- 名称：复制、重新生成、点赞、点踩、分享
- 触发条件：**AI 回复完成后才出现**
- **位置：不在 ds-message 内部，在 ds-message 的兄弟元素中**

### 按钮容器结构

```text
DIV._4f9bf79.d7dc56a8._43c05b5              ← 父级容器（含消息+按钮）
├── DIV.ds-message._63c77b1                 ← 消息本身（按钮数=0）
└── DIV.ds-flex._0a3d93b                    ← 按钮容器（消息的兄弟元素，按钮数=15）
    ├── (复制按钮组)
    │   ├── DIV.ds-button.ds-button--iconLabelPrimary...
    │   ├── DIV.ds-button__background
    │   └── DIV.ds-button__icon.ds-button__icon--last-child
    ├── (重新生成按钮组)
    │   └── (同上 3 个 div)
    ├── (点赞按钮组)
    │   └── (同上 3 个 div)
    ├── (点踩按钮组)
    │   └── (同上 3 个 div)
    └── (分享按钮组)
        └── (同上 3 个 div)
```

### 按钮计数规则
- 每个操作按钮由 **3 个 div** 组成（本体 + 背景 + 图标）
- 5 个操作按钮 × 3 = **15 个 `[class*="ds-button"]` 元素**
- 判定回复完成的阈值：`>= 15`（不是 >= 5）

### 5 个按钮的 className 差异
前 3 个按钮（复制、重新生成、点赞）使用 `ds-button--iconLabelPrimary`
后 2 个按钮（点踩、分享）使用 `ds-button--iconLabelTertiary`
尺寸：前 4 个用 `ds-button--m`，最后一个用 `ds-button--xs`

### 父级链（从 ds-message 向上 5 层）

| 层级 | className | buttonCount |
|------|-----------|-------------|
| 0 (消息本身) | `ds-message _63c77b1` | 0 |
| 1 (消息+按钮容器) | `_4f9bf79 d7dc56a8 _43c05b5` | 15 |
| 2 (可见列表) | `ds-virtual-list-visible-items` | 21 |
| 3 (列表容器) | `ds-virtual-list-items _6f2c522` | 21 |
| 4 (滚动区) | `ds-virtual-list ds-virtual-list--printable ds-scroll-area...` | 27 |

---

## 四、检测逻辑正确写法

### 1. 判断 AI 回复完成（5 个按钮出现）

❌ **错误写法**（当前代码，永远不生效）：
```javascript
const lastMsg = messages[messages.length - 1];
const btns = lastMsg.querySelectorAll('[class*="ds-button"]');  // 永远是 0
if (btns.length >= 5) { ... }
```

✅ **正确写法 A**（检查父级容器，推荐）：
```javascript
const lastMsg = messages[messages.length - 1];
const parent = lastMsg.parentElement;
if (parent && parent.querySelectorAll('[class*="ds-button"]').length >= 15) {
    window.__dsBrowserDOMDone = true;
}
```

✅ **正确写法 B**（检查兄弟元素）：
```javascript
const lastMsg = messages[messages.length - 1];
const next = lastMsg.nextElementSibling;
if (next && next.querySelectorAll('[class*="ds-button"]').length >= 15) {
    window.__dsBrowserDOMDone = true;
}
```

### 2. 区分消息类型

```javascript
const lastMsg = messages[messages.length - 1];
const cls = lastMsg.className || '';

// 用户消息
const isUserMsg = cls.indexOf('d29f3d7d') !== -1;

// AI 回复
const isAIReply = !!lastMsg.querySelector('[class*="ds-assistant-message-main-content"]');

// 系统提示（推测：既不是用户消息也不是 AI 回复）
const isSystemPrompt = !isUserMsg && !isAIReply;
```

### 3. 检测系统提示（只检查最后一个 ds-message）

```javascript
// 拦截器标志位优先（2026-08-10 新增 __dsConvLimitHit）
if (window.__dsServerBusy) return 'serverBusy:interceptor';
if (window.__dsConvLimitHit) return 'convLimit:interceptor';

const lastMsg = messages[messages.length - 1];
const text = lastMsg.textContent || '';

// 系统提示关键词
const busyKeywords = ['消息发送过于频繁', '发送过于频繁', '服务器繁忙',
                      '服务繁忙', '请稍后重试', '请稍后再试', '有消息正在生成'];
const limitKeywords = ['达到对话长度上限', '请开启新对话', '对话长度上限'];

for (const kw of busyKeywords) {
    if (text.indexOf(kw) !== -1) return 'serverBusy:' + kw;
}
for (const kw of limitKeywords) {
    if (text.indexOf(kw) !== -1) return 'convLimit:' + kw;
}
```

### 4. 检测思考状态

```javascript
const lastMsg = messages[messages.length - 1];
const text = lastMsg.textContent || '';
const thinkingKeywords = ['正在思考', '已思考'];
for (const kw of thinkingKeywords) {
    if (text.indexOf(kw) !== -1) return 'thinking';
}
```

### ⚠️ 关键规则：系统提示与思考的互斥关系（2026-07-31 确认）

**系统提示如果出现，永远在"正在思考"之前；一旦出现系统提示，就不可能再出现"正在思考"。**

这意味着：
1. **检测顺序必须是：先检查系统提示，再检查思考**——当前代码符合此顺序（errorDetectJS：toast 扫描 → lastMsg 系统提示 → thinking）
2. **检测到系统提示 → 立即返回错误，不等待思考**——当前代码符合（直接 return）
3. **检测到思考 → 100%确认没有系统提示**——可以安全早停，当前代码符合（return "thinking"）
4. **3秒内都没检测到 → 检查逻辑可能有问题**（按用户要求，0-3秒内一定能检测到其中之一）
5. **两者互斥，不会同时出现**——所以不需要考虑"系统提示和思考同时存在"的复杂场景

### 4.5 errorDetectJS 检测失败对后续流程的影响（2026-08-09 用户重点关注）

**检测环节是 sendChat 流程的"哨兵"**：发送消息后 3 秒内（50ms 间隔轮询，最多 60 次）检测页面状态，决定后续走"等待回复"还是"立即切换账号/新对话"。**检测结果决定后续步骤，检测出错会导致后续步骤执行错误。**

**正常检测结果 → 后续动作**：

| 检测结果 | 含义 | 后续动作（sendChat） |
|---|---|---|
| `serverBusy:xxx` | 服务器繁忙/发送频繁 | 立即 `retryWithAccountSwitch`（切换账号重试） |
| `convLimit:xxx` | 对话长度上限 | 立即 `retryWithNewConversation`（新对话重试） |
| `thinking` | AI 正常思考中（含"正在阅读"——大文件发送后先显示"正在阅读"再"正在思考"，2026-08-09 加入） | 早停，进入 `waitForResponse` 等待回复 |
| `''`（3 秒无异常） | 无系统提示，请求正常 | 进入 `waitForResponse` 等待回复 |

**检测失败/出错的 4 种情况及其影响**（按影响严重程度排序）：

| # | 失败类型 | 表现 | 对后续步骤的影响 | 严重度 |
|---|---|---|---|---|
| 1 | **漏检系统提示**（如 Toast 弹窗未被扫到，2026-08-09 修复前的缺陷） | 返回 `''` → 进入 waitForResponse | **干等 120 秒超时**（AI 不会回复），而不是立即切换账号；客户端收到超时错误 | 🔴 高 |
| 2 | **误判系统提示**：busy 关键词出现在用户问题/AI 回复中 | 返回 `serverBusy` → 切换账号 | 错误切换账号；已修复为只扫短文本元素（toast/lastMsg+兄弟，非全文）降低误报 | 🟠 中 |
| 3 | **漏检"正在思考"**（thinking 未被扫到） | 轮询满 3 秒 → 进入 waitForResponse | 影响小：仅延迟约 2.5 秒进入等待，功能正常 | 🟢 低 |
| 4 | **检测执行异常**（DOM 查询报错） | 跳过该轮继续轮询，3 秒后返回空 | 影响小：最多延迟 3 秒，功能正常 | 🟢 低 |

> **注**：原"textarea 短暂禁用误判 inputDisabled → 错误切换账号"一项已于 2026-08-09 撤销——CDP 实测聊天页面 textarea **任何时刻均不禁用**（AI 回复期间照常可输入），该判断是永远不会触发的死代码，已从 errorDetectJS 删除。

**防护机制**：
- `waitForResponse` 本身有超时保护（response_timeout_sec 默认 120 秒），即使检测漏检也不会永久卡死，但会浪费整个超时时间
- 检测顺序保证：系统提示（toast + lastMsg 链）→ 思考，系统提示优先，不会因"先检测到思考"而漏掉系统提示

**2026-08-09 修复记录**：Toast 形式系统提示（"服务器繁忙，请稍后重试"以右上角 `ds-notification-container` 弹窗出现）此前漏检——2026-07-31 重构为"只查 lastMsg"时删除了 Toast 扫描，文档却仍记载"已修复"。CDP 实测：注入 Toast 后检测仍返回 `thinking`（漏检）。修复：Toast 扫描重新加回，并放在 messages 检查**之前**（因为服务器繁忙时页面可能连用户消息都不显示，必须先扫弹窗）。修复后实测：注入"服务器繁忙"→ `serverBusy:服务器繁忙`；注入"对话长度上限"→ `convLimit`；真实请求 thinking 检测正常（872ms 检出）。

**2026-08-09 二次修复记录**：`thinkingKeywords` 加入"正在阅读"——用户观察到大文件发送后网页先显示"正在阅读"再"正在思考"。加入前：大文件请求发送后 3 秒检测窗口内识别不到 thinking（白等满 3 秒才进入 waitForResponse）；加入后：页面显示"正在阅读"即识别为处理中，立即早停进入 waitForResponse。CDP 实测：注入"正在阅读"→ `thinking`；原有关键词（"正在思考"/"已思考"/busy/limit）回归全部正常。

---

## 五、其他页面元素

### 输入框
- 选择器：`textarea`
- `disabled` 属性：**实测（2026-08-09）聊天页面任何时刻均为 `false`**——AI 思考/回复期间、服务器繁忙时都可正常输入。旧文档"服务器繁忙/有消息生成时为 true"的记录已被实测推翻，且 errorDetectJS 中基于 `ta.disabled` 的 `serverBusy:inputDisabled` 判断（死代码）已删除
- **就绪信号（2026-08-09 页面稳定检测）**：页面销毁重建后（新对话/刷新/重启），React 是否完全接管输入区以 `textarea` 是否**渲染出 placeholder** 为判据（`!ta.placeholder` = 未就绪）。完整检测：textarea 可见（getBoundingClientRect 非 0）+ `document.readyState === 'complete'` + placeholder 已渲染，连续 3 次（200ms 间隔）均通过即视为稳定（`waitForPageStable`，超时 10s 只告警不中断）。**背景**：页面刚重建时 React 事件监听尚未挂载，此时直接输入长文本再按 Enter，事件到达输入框但不触发发送（2026-08-09 实测：新对话 20:03/20:37 两次失败均伴随 TARGET DESTROYED + interceptor 重新注入）

### 发送按钮
- 选择器（实际探测 2026-08-08 确认）：`div.ds-button--primary`（`<div role="button">`，**不是** `button` 标签）
- 完整 class：`ds-button ds-button--primary ds-button--filled ds-button--circle ds-button--m ds-button--icon-relative-m ds-button--disabled _52c986b bd74640a`
- 子结构：`ds-button__background` + `ds-button__icon`（内含 SVG，2 个 `<path>`）
- 禁用状态：class 含 `ds-button--disabled` 且 React fiber 状态 `D.current=true`（此时点击无效，onClick 第一行直接 return）
- ⚠️ **重要**：旧选择器 `button[aria-label="send"]` 已过时（页面中不存在），diag_dom.go 诊断脚本**已同步更新**为 `div.ds-button--primary`（2026-08-09 确认，diagDOMJS 含 disabledClass 字段）
- ⚠️ **Enter 发送注意事项（2026-08-08 修复）**：不能用 `chromedp.SendKeys("\r")`——它生成的 char 事件会把 `\r` 当文本插入 textarea（残留换行符 `\n`），导致误判发送失败、按钮被判禁用。必须手动发 keyDown+keyUp（无 char 事件）

### 模式切换（快速/专家/识图）
- 选择器：`[role="radiogroup"] [role="radio"]`
- **2026-08-09 探测确认：DeepSeek 现有 3 种模式**（均在新对话首页可见）：
  - **快速模式**（默认选中）— radio 文本 "快速模式快速模式"，`aria-checked="true"`
  - **专家模式** — radio 文本 "专家模式专家模式"，`aria-checked="false"`
  - **识图模式** — radio 文本 "识图模式识图模式"，`aria-checked="false"`
- 每个 radio 是 `DIV[role="radio"]`（不是 `input[type="radio"]`），文本在 DOM 中重复出现两次（如 "快速模式快速模式"），去重后为单次
- 模式关键词：`识图`、`默认`、`快速`、`专家`
- 排除关键词（这些是 toggle，不是模式切换）：`深度思考`、`智能搜索`、`联网搜索`
- 选中状态：`aria-checked="true"`（class 无 selected 标记，`--selected` 不适用，靠 aria-checked 判断）
- ⚠️ **对话进行中 radio 消失（2026-08-09 探测确认）**：发送消息进入对话后，`[role="radio"]` 返回空数组，模式 radio 不再渲染；仅新对话首页可见。程序通过图片预览元素（`img[src*="blob:"]`/`img[src*="data:"]`）回退检测识图模式
- ⚠️ **程序现状（2026-08-09）**：代码仅支持"快速(text)/识图(image)"两种模式的切换；**专家模式未被程序识别**（`getCurrentModeJS` 的 modeKeywords 不含 "专家"，`switchMode` 无 expert 分支）。当前若页面处于专家模式，文本请求会被当快速模式处理（不做切换）。**专家模式支持已列入待办，暂不改动代码，详见 README**

### 滚动列表
- `ds-virtual-list` 是虚拟滚动列表
- `ds-virtual-list-visible-items` 是当前可见部分
- 消息按顺序追加在列表末尾

---

## 六、拦截器全局变量

| 变量名 | 含义 |
|--------|------|
| `window.__dsBrowserCapture` | 拦截器捕获的回复内容 |
| `window.__dsBrowserThinking` | 拦截器捕获的思考内容 |
| `window.__dsBrowserDone` | 拦截器判断 SSE 流结束（成功标志） |
| `window.__dsBrowserDOMDone` | DOM 观察器判断回复完成 |
| `window.__dsBrowserLog` | 拦截器日志数组 |
| `window.__dsBrowserPTypes` | 已处理的 SSE 事件类型计数 |
| `window.__dsBrowserRawSSE` | fetch 通道累积的原始 SSE 文本（上限 20000 字符，保留末尾） |
| `window.__dsBrowserSamples` | 各 SSE 事件类型首个样本（诊断用） |
| `window.__dsBrowserUnknownSamples` | 未知 SSE 事件类型样本（最多 5 个，诊断用） |
| `window.__dsConvLimitHit` | 拦截器检测到对话上限 |
| `window.__dsServerBusy` | 拦截器检测到服务器繁忙 |
| `window.__dsDiagLog` | 诊断日志（检测到系统提示时自动写入） |
| `window.__dsInjectDone` | 拦截器是否已注入（页面重载后丢失变 false，作为"页面是否刚重建"的检测依据，见 chat.go injectInterceptor） |
| `window.__dsArticleBaseline` | 发送前的 ds-markdown（AI 回复正文）数量基线，用于判断是否有新回复（sendMessage/sendMessageOrUpload 时记录，sendMessageOrUpload 兜底时比较） |
| `window.__dsCurrentFragmentType` | 当前 SSE 片段类型（'THINK'/'THINKING' 等），用于区分思考与正文内容 |
| `window.__dsObserveActive` | DOM 观察器是否激活 |
| `window.__dsObserveInterval` | DOM 观察器定时器 ID |

---

## 七、Toast 通知元素（2026-08-07 发现）

### 发现过程
用户反馈"服务器繁忙，请稍后重试"系统提示在页面上可见但 `errorDetectJS` 未检测到。通过 `diagDOMJS` 日志确认页面存在 `ds-notification-container` Toast 通知容器（`toastElements=2`）。

### Toast 容器结构
```text
DIV.ds-notification-container.ds-theme.ds-notification-container--top-right
```

### 关键发现
- 系统提示"服务器繁忙，请稍后重试"以 **Toast 通知** 形式出现在页面右上角，而非在 `ds-message` 消息列表中
- 旧版 `errorDetectJS`（`chat.go.bak` 第89-101行）有 toast 扫描，但 **2026-07-31 重构为"只查 lastMsg"时该步骤被删除**，导致漏检（当时文档误记为"已修复"）
- **2026-08-09 重新修复**：`errorDetectJS` 在最前面（messages 检查之前）扫描 `[class*="toast"], [class*="notification"], [role="alert"]`，跳过长度>200的内容避免误扫。**必须放在 messages 检查之前**——服务器繁忙时页面可能连用户消息都不显示，若放在其后会提前 return 扫不到弹窗
- 页面常驻 notification 容器（启动日志 `toastElements=2`）文本若无系统提示关键词，不会误报

### 已观测到的 Toast 通知文字
- `服务器繁忙，请稍后重试`（用户复现，2026-08-07）

---

## 八、待探测/待验证项

- [x] 系统提示的 DOM 结构（2026-07-31 已复现"消息发送过于频繁"，见"二、各类型消息详细结构 > 4. 系统提示"）
- [x] 系统提示是否有兄弟按钮容器（2026-07-31 已确认：系统提示旁有复制/编辑 2 个操作按钮）
- [ ] 图片上传后的预览元素结构（代码已用 `img[src*="blob:"]`/`img[src*="data:"]` 检测，具体 DOM 结构待探测记录）
- [ ] 文件上传后的附件元素结构（代码用 `input[type="file"]` 上传，上传后的附件卡片 DOM 结构待探测记录）
- [ ] 新对话按钮的精确选择器
- [ ] **系统提示真实出现后的完整链路（未验证 2026-08-11）**：拦截器 `isSystemPrompt` 置位 `__dsServerBusy`/`__dsConvLimitHit` 标志 → errorDetectJS 检测 → Go 层 waitForResponse 提前返回 → 新开对话/切换账号重试。**此链路目前仅代码实现，未在真实系统提示（"达到对话长度上限"/"服务器繁忙"/"消息发送过于频繁"）出现时实测过**。原因：主动新对话检查（累计字符 60-90 万 > 阈值）已在服务器触发系统提示（约 100 万字符）之前自动开新对话，真实系统提示难以自然出现。如遇真实系统提示，需确认：①拦截器标志正确置位；②waitForResponse 不干等超时；③重试链正确执行（convLimit→新对话，serverBusy→切账号）；④系统提示文本不会返回给客户端。

---

## 九、变更记录

- **2026-08-11**：**修复"对话长度上限"系统提示被当回复返回 + 主动新对话检查失效 + 新对话点击假成功**（完整链路修复，实盘验证通过）。
  - **拦截器系统提示识别（injector.go）**：拦截器新增 `isSystemPrompt` 检测，SSE 流中出现"对话长度上限/服务器繁忙/消息频繁"等关键词时**置位 `__dsServerBusy`/`__dsConvLimitHit` 标志**，**不清空捕获内容**（用户确认：拦截到系统提示 = 当次不可能有回复，由 Go 层检测标志后判定失败并重试，捕获内容不会返回客户端，无需过滤）。此前版本曾尝试"命中关键词清空 capture"的过滤方案，实测不需要，已回退。
  - **新对话后仍 convLimit 不再切账号（chat.go retryWithNewConversation）**：用户确认新对话（干净空对话）不可能触发"对话长度上限"，若新对话后仍检测到 convLimit，一定是脚本问题（新对话未真正开启 / 上次系统提示残留被误判），切换账号解决不了，直接返回明确错误，终止重试链。
  - **主动新对话统计口径修正（chat.go convMsgCount）**：由"只累加回复字符数"（约 0.15 万/轮）改为"**提示词+思考+回复**"三者之和（约 1.2-3.4 万/轮）。根因：服务器"对话长度上限"按完整上下文（约 100 万字符）计算，旧统计口径到 60-90 万阈值需约 400 轮、远慢于服务器（约 29 轮即触发），主动检查永远追不上。修正后约 20-30 轮即达阈值，在服务器上限前主动开新对话。**实盘验证（2026-08-11 17:09）**：累计 819,803 字符 > 阈值 809,669 → 触发 `[api] new conversation (cumulative chars reached random threshold)` → `new conversation confirmed (empty chat)` → 统计清零（14,501）重新累计，全链路闭环正常。
  - **新对话真实点击 + 验证空对话（chat.go NewConversation）**：由 JS `click()`（React 页面无效，点击后直接返回"成功"但实际没开）改为 `chromedp.MouseClickXY` 真实鼠标点击，并用 `waitForEmptyTextarea` 验证对话为空（ds-markdown==0）才算成功，失败依次尝试 Ctrl+J / NavigateHome。根因：新对话假成功导致重试发送仍在旧对话、再次触发"对话长度上限"（实测 13:43-13:56 共 28 次循环）。
- **2026-08-10**：**修复系统提示漏检（`__dsConvLimitHit`）+ 保活从整页刷新改为轻量唤醒 + 页面重建机制**。
  - errorDetectJS 新增拦截器 `__dsConvLimitHit` 标志位检查（第 60 行），在 `__dsServerBusy` 之后、Toast 扫描之前。根因：拦截器已设置 `__dsConvLimitHit` 但 errorDetectJS 只检查了 `__dsServerBusy`，导致"达到对话长度上限"系统提示出现后仍检测不到，请求干等 120 秒超时。
  - `waitForResponse` 新增短内容早期返回：轮询中检查捕获内容 <200 字符且含 convLimit/serverBusy 关键词时立即返回，不依赖 r.D/r.DD（对话达到上限时 AI 不会回复，DOM 永远不会判定完成，旧逻辑会空等 120 秒超时）。
  - 保活机制重构：`idleRefreshInterval` 从 30 分钟改为 12 分钟，`idleRefreshCheck` 从 5 分钟改为 3 分钟；`refreshPageLocked` 从整页导航重载改为轻量唤醒（`SetWebLifecycleState active` + `BringToFront`，3 秒超时，毫秒级不重载）。根因：30 分钟太久，Chrome Memory Saver 可能在此之前已卸载页面（渲染进程销毁），导航命令发给已卸载的标签页会永久挂起导致程序假死。
  - 新增 `RebuildPage`（session.go）：唤醒失败时自动重建页面。流程：先建新标签页导航回 DeepSeek → 等待就绪 → 清理旧标签页。注意顺序：先建后关，避免唯一标签页关闭导致 Chrome 进程退出。
  - `StartIdleRefresher` 和 `ensureReady` 中唤醒失败时自动触发 `RebuildPage` 重建。
  - 新对话阈值从 30-60 条消息改为累计字符数 60-90 万（`ShouldNewConversationByCount` + `convMsgCount`/`convMsgThreshold`），从源头避免到达服务器对话长度上限。
- **2026-08-09**：**新增页面稳定检测（`waitForPageStable`），统一应用到所有发送场景**。根因：新对话/刷新/重启后页面销毁重建（TARGET DESTROYED），React 事件监听尚未挂载，此时直接输入长文本再按 Enter 事件到达输入框但不触发发送（实测 20:03/20:37 两次失败均伴随 interceptor 重新注入）。修复：`injectInterceptor` 检测到拦截器丢失（= 页面刚重载）时，先等页面稳定（textarea 可见 + placeholder 已渲染 + 连续 3 次稳定）再注入拦截器。**检测只在页面重载时触发，连续对话（拦截器还在）自动跳过**，不浪费等待时间。覆盖所有发送路径：sendChat / prepareForRetry / NavigateHome 重试。实盘验证：新对话 → 连续对话 ×2 → 新对话 4 次请求全部一次发送成功（pressEnter cleared after 1 checks）。详见"五、其他页面元素 > 输入框"。
- **2026-08-09**：**修复 Toast 系统提示漏检**。发现 errorDetectJS 扫不到右上角弹窗形式的"服务器繁忙"（2026-07-31 重构删除了 toast 扫描，文档误记"已修复"）。修复：toast 扫描加回并置于 messages 检查之前。CDP 实测验证通过。详见 4.5 节。
- **2026-08-09**：探测确认 DeepSeek 现有 **快速/专家/识图** 三种模式（`DIV[role="radio"]`，文本重复两次，选中靠 `aria-checked="true"`）。程序仅支持快速/识图两种，专家模式未支持（列入待办）。确认对话进行中 radio 消失、JS click 对模式 radio 有效。同步修正文档与代码一致性：errorDetectJS 行号、diag_dom.go 已更新、拦截器变量补全。证据：CDP 直接探测 + 5 次识图模式真实请求测试。
- **2026-08-07**：发现系统提示"服务器繁忙，请稍后重试"以 Toast 通知形式出现（`ds-notification-container`），不在 `ds-message` 区域。`errorDetectJS` 漏检根因确认：旧版有 toast 扫描但被简化删除。修复：新增步骤2 扫描 `[class*="toast"], [class*="notification"]` 元素，跳过长度>200 的内容避免误扫。证据：`diagDOMJS` 日志确认 `toastElements=2`；`chat.go.bak` 第89-101行有旧版实现。
- **2026-07-31**：初次建立文档。通过 `/v1/debug?msgs=1` 实际探测，发现 5 个操作按钮不在 `ds-message` 内部（在兄弟元素 `ds-flex _0a3d93b` 中），修正了历史代码中"在 ds-message 内部找按钮"的根本性错误。
- **2026-07-31**：修复 `observeCaptureScript` 的 scan 函数。原代码 `lastMsg.querySelectorAll('[class*="ds-button"]')` 永远返回 0（按钮不在 ds-message 内部），改为 `lastMsg.parentElement.querySelectorAll('[class*="ds-button"]')` 并加阈值 `>= 15`。同时增加判据：必须确认 lastMsg 含 `ds-assistant-message-main-content`（是 AI 回复），避免用户消息的父级容器误判。实盘测试通过：`domDone=true`，`scanResult.wouldSetDone=true`，`parentBtnCount=15`。
