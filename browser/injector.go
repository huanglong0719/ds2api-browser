package browser

const InjectScript = `
(() => {
if (window.__dsInjectDone) {
	window.__dsBrowserCapture = '';
	window.__dsBrowserThinking = '';
	window.__dsBrowserDone = false;
}
window.__dsBrowserRawSSE = '';
window.__dsBrowserCapture = '';
window.__dsBrowserThinking = '';
window.__dsBrowserDone = false;
window.__dsBrowserLog = [];
window.__dsBrowserPTypes = {};
window.__dsBrowserSamples = {};
window.__dsCurrentFragmentType = '';
window.__dsInjectDone = true;
window.__dsServerBusy = false;
window.__dsConvLimitHit = false;

function checkFlags() {
	var all = (window.__dsBrowserCapture || '') + (window.__dsBrowserThinking || '');
	if (all.indexOf('消息发送过于频繁') !== -1 || all.indexOf('发送过于频繁') !== -1 || all.indexOf('服务器繁忙') !== -1 || all.indexOf('服务繁忙') !== -1 || all.indexOf('请稍后重试') !== -1) {
		window.__dsServerBusy = true;
	}
	if (all.indexOf('达到对话长度上限') !== -1 || all.indexOf('请开启新对话') !== -1 || all.indexOf('对话长度上限') !== -1) {
		window.__dsConvLimitHit = true;
	}
}

// Intercept fetch
const origFetch = window.fetch;
window.fetch = async function(...args) {
	const url = args[0] && typeof args[0] === 'string' ? args[0] : (args[0] && args[0].url ? args[0].url : '');
	const pathname = (() => { try { return new URL(url, location.href).pathname; } catch(e) { return url; } })();
	window.__dsBrowserLog.push('f:' + pathname.substring(0,60));
	if (window.__dsBrowserLog.length > 200) window.__dsBrowserLog.shift();
	const resp = await origFetch.apply(this, args);
	if (!resp) return resp;
	const isChat = pathname.includes('/api/v0/');
	if (isChat) {
		window.__dsBrowserLog.push('MATCH_F:' + pathname.substring(0,60));
		const reader = resp.clone().body.getReader();
		const decoder = new TextDecoder();
		let buf = '';
		(function pump() {
			reader.read().then(({done, value}) => {
				if (done) return;
				buf += decoder.decode(value, {stream: true});
				const lines = buf.split('\n');
				buf = lines.pop() || '';
				for (const line of lines) {
					if (!line.startsWith('data: ')) continue;
					try {
						const d = JSON.parse(line.slice(6));
						var ptype = d.p || (d.content ? 'direct_content' : (d.thinking ? 'direct_thinking' : (d.v && d.v.response ? 'v_response' : 'unknown')));
						window.__dsBrowserPTypes[ptype] = (window.__dsBrowserPTypes[ptype] || 0) + 1;
						if (!window.__dsBrowserSamples[ptype]) window.__dsBrowserSamples[ptype] = JSON.stringify(d).substring(0, 200);
						if (d.p === 'response/fragments' && d.o === 'APPEND' && Array.isArray(d.v)) {
							for (const f of d.v) {
								if (f && f.content) {
									if (f.type === 'RESPONSE') {
										window.__dsBrowserCapture = (window.__dsBrowserCapture || '') + f.content;
									} else if (f.type === 'THINKING') {
										window.__dsBrowserThinking = (window.__dsBrowserThinking || '') + f.content;
									} else {
										window.__dsBrowserCapture = (window.__dsBrowserCapture || '') + f.content;
									}
								}
							}
							var lastFrag = d.v[d.v.length - 1];
							if (lastFrag && lastFrag.type) {
								window.__dsCurrentFragmentType = lastFrag.type;
							}
						} else if (d.p && d.p.indexOf('response/fragments/') === 0 && typeof d.v === 'string' && (!d.o || d.o === 'APPEND')) {
							if (window.__dsCurrentFragmentType === 'THINK') {
								window.__dsBrowserThinking = (window.__dsBrowserThinking || '') + d.v;
							} else {
								window.__dsBrowserCapture = (window.__dsBrowserCapture || '') + d.v;
							}
						} else if (d.p === 'response/content' && typeof d.v === 'string' && (!d.o || d.o === 'APPEND')) {
							window.__dsBrowserCapture = (window.__dsBrowserCapture || '') + d.v;
						}
						if (d.p === 'response/status' && d.v === 'FINISHED') {
							window.__dsBrowserDone = true;
							window.__dsBrowserLog.push('DONE_F:' + JSON.stringify(d).substring(0,50));
							checkFlags();
						}
						if (d.p === 'response/status' && d.o === 'SET' && d.v === 'FINISHED') {
							window.__dsBrowserDone = true;
							window.__dsBrowserLog.push('DONE_F_SET');
							checkFlags();
						}
					} catch(e) {
						window.__dsBrowserLog.push('PE:' + line.substring(0,60));
					}
				}
				pump();
			});
		})();
	}
	return resp;
};

// Intercept XMLHttpRequest
const OrigXHR = window.XMLHttpRequest;
window.XMLHttpRequest = function() {
	const xhr = new OrigXHR();
	const origOpen = xhr.open;
	const origSend = xhr.send;
	let xhrUrl = '';
	xhr.open = function(method, url, ...rest) {
		xhrUrl = (() => { try { return new URL(url, location.href).pathname; } catch(e) { return url; } })();
		window.__dsBrowserLog.push('x:' + xhrUrl.substring(0,60));
		if (window.__dsBrowserLog.length > 200) window.__dsBrowserLog.shift();
		return origOpen.apply(this, [method, url, ...rest]);
	};
	xhr.send = function(...args) {
		const self = this;
		const origOnReady = self.onreadystatechange;
		let xhrBuf = '';
		let xhrMatched = false;
		self.onreadystatechange = function(ev) {
			if (self.readyState === 4) {
				const text = self.responseText || '';
				if (text && xhrUrl.includes('/api/v0/')) {
					if (!xhrMatched) {
						xhrMatched = true;
					window.__dsBrowserLog.push('MATCH_X:' + xhrUrl.substring(0,60));
					if (xhrUrl.indexOf('chat/completion') >= 0) {
						window.__dsBrowserRawSSE = text.substring(0,10000);
					}
				}
					xhrBuf = text;
					const lines = xhrBuf.split('\n');
					let lastDirectContentLen = 0;
					let lastDirectThinkingLen = 0;
					let lastVContentLen = 0;
					let lastVThinkingLen = 0;
					for (const line of lines) {
						if (!line.startsWith('data: ')) continue;
						try {
							const d = JSON.parse(line.slice(6));
							var ptype = d.p || (d.content ? 'direct_content' : (d.thinking ? 'direct_thinking' : (d.v && d.v.response ? 'v_response' : 'unknown')));
							window.__dsBrowserPTypes[ptype] = (window.__dsBrowserPTypes[ptype] || 0) + 1;
							if (!window.__dsBrowserSamples[ptype]) window.__dsBrowserSamples[ptype] = JSON.stringify(d).substring(0, 200);
							if (d.p === 'response/fragments' && d.o === 'APPEND' && Array.isArray(d.v)) {
								for (const f of d.v) {
									if (f && f.content) {
										if (f.type === 'RESPONSE') {
											window.__dsBrowserCapture = (window.__dsBrowserCapture || '') + f.content;
										} else if (f.type === 'THINKING') {
											window.__dsBrowserThinking = (window.__dsBrowserThinking || '') + f.content;
										} else {
											window.__dsBrowserCapture = (window.__dsBrowserCapture || '') + f.content;
										}
									}
								}
								var lastFrag = d.v[d.v.length - 1];
								if (lastFrag && lastFrag.type) {
									window.__dsCurrentFragmentType = lastFrag.type;
									window.__dsBrowserLog.push('FRAG_TYPE:' + lastFrag.type);
								}
							} else if (d.p && d.p.indexOf('response/fragments/') === 0 && typeof d.v === 'string' && (!d.o || d.o === 'APPEND')) {
								if (window.__dsCurrentFragmentType === 'THINK') {
									window.__dsBrowserThinking = (window.__dsBrowserThinking || '') + d.v;
								} else {
									window.__dsBrowserCapture = (window.__dsBrowserCapture || '') + d.v;
								}
							} else if (d.p === 'response/content' && typeof d.v === 'string' && (!d.o || d.o === 'APPEND')) {
								window.__dsBrowserCapture = (window.__dsBrowserCapture || '') + d.v;
							} else if (typeof d.v === 'string' && !d.p && !d.o && !d.content && !d.thinking) {
								if (window.__dsCurrentFragmentType === 'THINK') {
									window.__dsBrowserThinking = (window.__dsBrowserThinking || '') + d.v;
								} else {
									window.__dsBrowserCapture = (window.__dsBrowserCapture || '') + d.v;
								}
							} else if (typeof d.content === 'string' && !d.p) {
								var nc = d.content.substring(lastDirectContentLen);
								lastDirectContentLen = d.content.length;
								if (nc) window.__dsBrowserCapture = (window.__dsBrowserCapture || '') + nc;
							} else if (typeof d.thinking === 'string' && !d.p) {
								var nt = d.thinking.substring(lastDirectThinkingLen);
								lastDirectThinkingLen = d.thinking.length;
								if (nt) window.__dsBrowserThinking = (window.__dsBrowserThinking || '') + nt;
							} else if (d.v && d.v.response) {
								var r = d.v.response;
								if (Array.isArray(r.fragments) && r.fragments.length > 0) {
									var lastFrag = r.fragments[r.fragments.length - 1];
									if (lastFrag && lastFrag.type) {
										window.__dsCurrentFragmentType = lastFrag.type;
										window.__dsBrowserLog.push('FRAG_TYPE:' + lastFrag.type);
									}
								}
								if (typeof r.content === 'string') {
									var nc = r.content.substring(lastVContentLen);
									lastVContentLen = r.content.length;
									if (nc) window.__dsBrowserCapture = (window.__dsBrowserCapture || '') + nc;
								}
								if (typeof r.thinking === 'string') {
									var nt = r.thinking.substring(lastVThinkingLen);
									lastVThinkingLen = r.thinking.length;
									if (nt) window.__dsBrowserThinking = (window.__dsBrowserThinking || '') + nt;
								}
								if (typeof r.delta === 'string' && r.delta) {
									window.__dsBrowserCapture = (window.__dsBrowserCapture || '') + r.delta;
								}
							}
							if (d.p === 'response/status' && d.v === 'FINISHED') {
								window.__dsBrowserDone = true;
								window.__dsBrowserLog.push('DONE_X');
								checkFlags();
							}
						} catch(e) {}
					}
				}
			}
			if (origOnReady) origOnReady.call(self, ev);
		};
		return origSend.apply(this, args);
	};
	// Copy static properties
	for (const key of Object.getOwnPropertyNames(OrigXHR)) {
		if (key === 'prototype' || key === 'name' || key === 'length') continue;
		const desc = Object.getOwnPropertyDescriptor(OrigXHR, key);
		if (desc) Object.defineProperty(window.XMLHttpRequest, key, desc);
	}
	window.XMLHttpRequest.prototype = OrigXHR.prototype;
	return xhr;
};

// Intercept EventSource (SSE client)
const OrigES = window.EventSource;
if (OrigES) {
	window.EventSource = function(url, ...args) {
		const pathname = (() => { try { return new URL(url, location.href).pathname; } catch(e) { return url; } })();
		window.__dsBrowserLog.push('es:' + pathname.substring(0,60));
		if (window.__dsBrowserLog.length > 200) window.__dsBrowserLog.shift();
		const es = new OrigES(url, ...args);
		if (pathname.includes('/api/v0/')) {
			window.__dsBrowserLog.push('MATCH_ES:' + pathname.substring(0,60));
			es.addEventListener('message', (e) => {
				try {
					const d = JSON.parse(e.data);
					var ptype = d.p || (d.content ? 'direct_content' : (d.thinking ? 'direct_thinking' : (d.v && d.v.response ? 'v_response' : 'unknown')));
					window.__dsBrowserPTypes[ptype] = (window.__dsBrowserPTypes[ptype] || 0) + 1;
					if (!window.__dsBrowserSamples[ptype]) window.__dsBrowserSamples[ptype] = JSON.stringify(d).substring(0, 200);
					if (d.p === 'response/fragments' && d.o === 'APPEND' && Array.isArray(d.v)) {
						for (const f of d.v) {
							if (f && f.content) {
								if (f.type === 'RESPONSE') {
									window.__dsBrowserCapture = (window.__dsBrowserCapture || '') + f.content;
								} else if (f.type === 'THINKING') {
									window.__dsBrowserThinking = (window.__dsBrowserThinking || '') + f.content;
								} else {
									window.__dsBrowserCapture = (window.__dsBrowserCapture || '') + f.content;
								}
							}
						}
						var lastFrag = d.v[d.v.length - 1];
						if (lastFrag && lastFrag.type) {
							window.__dsCurrentFragmentType = lastFrag.type;
						}
					} else if (d.p && d.p.indexOf('response/fragments/') === 0 && typeof d.v === 'string' && (!d.o || d.o === 'APPEND')) {
						if (window.__dsCurrentFragmentType === 'THINK') {
							window.__dsBrowserThinking = (window.__dsBrowserThinking || '') + d.v;
						} else {
							window.__dsBrowserCapture = (window.__dsBrowserCapture || '') + d.v;
						}
					} else if (d.v && d.v.response) {
						var r = d.v.response;
						if (Array.isArray(r.fragments) && r.fragments.length > 0) {
							var lastFrag = r.fragments[r.fragments.length - 1];
							if (lastFrag && lastFrag.type) {
								window.__dsCurrentFragmentType = lastFrag.type;
							}
						}
					} else if (d.p === 'response/content' && typeof d.v === 'string' && (!d.o || d.o === 'APPEND')) {
						window.__dsBrowserCapture = (window.__dsBrowserCapture || '') + d.v;
					}
					if (d.p === 'response/status' && d.v === 'FINISHED') {
						window.__dsBrowserDone = true;
						window.__dsBrowserLog.push('DONE_ES');
						checkFlags();
					}
				} catch(e) {}
			});
		}
		return es;
	};
}
})();
`
