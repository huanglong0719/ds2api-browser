package browser

import (
	"context"
	"log"

	"github.com/chromedp/chromedp"
)

const diagDOMJS = `JSON.stringify((()=>{var info={};var gs=document.querySelectorAll('svg');var copyBtnSvg=null;for(var s of gs){if(s.querySelectorAll('path').length===2){var parent=s.closest('button,[role="button"],div');if(parent){copyBtnSvg={tag:s.tagName,parentTag:parent.tagName,parentClass:parent.className.substring(0,200),parentRole:parent.getAttribute('role')||'',parentAria:parent.getAttribute('aria-label')||''};break}}}info.copyBtn=copyBtnSvg||'NOT_FOUND';var msgs=document.querySelectorAll('.ds-message');info.messageContainers=msgs.length;if(msgs.length>0){var last=msgs[msgs.length-1];info.lastMessage={tag:last.tagName,className:last.className.substring(0,200),childCount:last.children.length}}var sendBtns=document.querySelectorAll('div.ds-button--primary');info.sendBtn=sendBtns.length>0?{tag:sendBtns[0].tagName,disabledClass:(sendBtns[0].className||'').includes('ds-button--disabled')}:'NOT_FOUND';var ta=document.querySelector('textarea');info.textarea=ta?{tag:ta.tagName,placeholder:ta.getAttribute('placeholder')||'',disabled:ta.disabled||false}:'NOT_FOUND';var toasts=document.querySelectorAll('[class*="toast"],[class*="notification"]');info.toastElements=toasts.length;if(toasts.length>0){var t0=toasts[0];info.firstToast={tag:t0.tagName,className:t0.className.substring(0,200)}}return info;})())`

// DiagnoseDOM 诊断前端页面 DOM 结构变化
func (h *ChatHandler) DiagnoseDOM(ctx context.Context) {
	var result string
	err := chromedp.Run(h.session.Context(), chromedp.Evaluate(diagDOMJS, &result))
	if err != nil {
		log.Printf("[diag] DiagnoseDOM failed: %v", err)
		return
	}
	log.Printf("[diag] DOM structure: %s", result)
}

// DiagnoseAllMessages 诊断页面所有 ds-message 的详细结构
// 用于区分用户消息、系统提示、AI思考、AI回复
const diagAllMessagesJS = `JSON.stringify((()=>{
	var info = {messages: []};
	var msgs = document.querySelectorAll('[class*="ds-message"]');
	for (var i = 0; i < msgs.length; i++) {
		var msg = msgs[i];
		var item = {
			index: i,
			tag: msg.tagName,
			className: msg.className,
			childClasses: [],
			textPreview: (msg.textContent || '').substring(0, 100),
			hasAssistantContent: !!msg.querySelector('[class*="ds-assistant-message-main-content"]'),
			hasMarkdown: !!msg.querySelector('[class*="ds-markdown"]'),
			hasThinking: !!msg.querySelector('[class*="thinking"], [class*="ds-thinking"], [class*="reasoning"]'),
			buttonCount: msg.querySelectorAll('[class*="ds-button"]').length,
			childCount: msg.children.length
		};
		for (var c = 0; c < msg.children.length && c < 10; c++) {
			item.childClasses.push(msg.children[c].className);
		}
		info.messages.push(item);
	}
	info.totalMessages = msgs.length;
	return info;
})())`

// DiagnoseAllMessages 输出页面所有 ds-message 的详细结构
func (h *ChatHandler) DiagnoseAllMessages(ctx context.Context) {
	var result string
	err := chromedp.Run(h.session.Context(), chromedp.Evaluate(diagAllMessagesJS, &result))
	if err != nil {
		log.Printf("[diag] DiagnoseAllMessages failed: %v", err)
		return
	}
	log.Printf("[diag] All messages structure: %s", result)
}
