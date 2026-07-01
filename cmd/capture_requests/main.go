package main

import (
	"context"
	"ds2api-browser/browser"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/chromedp/chromedp"
)

func main() {
	allocCtx, allocCancel := chromedp.NewRemoteAllocator(context.Background(), "http://127.0.0.1:9222")
	defer allocCancel()

	ctx, cancel := chromedp.NewContext(allocCtx)
	defer cancel()

	log.Println("=== Step 1: Navigate to DeepSeek ===")

	var pageTitle string
	err := chromedp.Run(ctx,
		chromedp.Navigate("https://chat.deepseek.com"),
		chromedp.Sleep(5*time.Second),
		chromedp.Title(&pageTitle),
	)
	if err != nil {
		log.Fatalf("navigate failed: %v", err)
	}
	log.Printf("Page: %s", pageTitle)

	var pageState string
	_ = chromedp.Run(ctx,
		chromedp.Evaluate(`JSON.stringify({hasTextarea: !!document.querySelector('textarea'), url: window.location.href})`, &pageState),
	)
	log.Printf("State: %s", pageState)

	log.Println("=== Step 2: Inject new interceptor ===")

	var injectResult string
	_ = chromedp.Run(ctx,
		chromedp.Evaluate(browser.InjectScript, &injectResult),
	)
	log.Printf("Inject result: %s", injectResult)

	var stateAfterInject string
	_ = chromedp.Run(ctx,
		chromedp.Evaluate(`JSON.stringify({
			done: window.__dsBrowserDone,
			capture: (window.__dsBrowserCapture||'').length,
			thinking: (window.__dsBrowserThinking||'').length,
			fragType: window.__dsCurrentFragmentType,
			logLen: (window.__dsBrowserLog||[]).length
		})`, &stateAfterInject),
	)
	log.Printf("State after inject: %s", stateAfterInject)

	log.Println("=== Step 3: Send test message ===")

	var sendResult string
	_ = chromedp.Run(ctx,
		chromedp.Evaluate(`(()=>{
			const ta = document.querySelector('textarea');
			if (!ta) return 'no_textarea';
			ta.focus();
			ta.select();
			document.execCommand('insertText', false, '你好，请用一句话回答：1+1等于几？');
			ta.dispatchEvent(new Event('input', {bubbles: true}));
			ta.dispatchEvent(new Event('change', {bubbles: true}));
			return 'typed:' + ta.value.length;
		})()`, &sendResult),
		chromedp.Sleep(1*time.Second),
	)
	log.Printf("Send result: %s", sendResult)

	var clickResult string
	_ = chromedp.Run(ctx,
		chromedp.Evaluate(`(()=>{
			var ta = document.querySelector('textarea');
			if (!ta) return 'no_ta';
			var taRect = ta.getBoundingClientRect();
			var all = document.querySelectorAll('div[role="button"][class*="ds-icon-button"]');
			var best = null, bestX = -1;
			for (var i = 0; i < all.length; i++) {
				var r = all[i].getBoundingClientRect();
				if (r.width > 0 && r.height > 0 && r.x > taRect.x + taRect.width * 0.5) {
					if (all[i].getAttribute('aria-disabled') === 'true') continue;
					if (r.x > bestX) { bestX = r.x; best = all[i]; }
				}
			}
			if (!best) return 'no_btn';
			best.click();
			return 'clicked';
		})()`, &clickResult),
	)
	log.Printf("Click result: %s", clickResult)

	log.Println("=== Step 4: Poll for response (max 60s) ===")

	done := false
	for i := 0; i < 60; i++ {
		time.Sleep(1 * time.Second)

		var result string
		_ = chromedp.Run(ctx,
			chromedp.Evaluate(`JSON.stringify({
				done: window.__dsBrowserDone === true,
				capture: (window.__dsBrowserCapture||'').substring(0, 200),
				thinking: (window.__dsBrowserThinking||'').substring(0, 200),
				fragType: window.__dsCurrentFragmentType,
				log: (window.__dsBrowserLog||[]).slice(-5)
			})`, &result),
		)

		if i%5 == 0 {
			log.Printf("[%ds] %s", i+1, result)
		}

		var state map[string]interface{}
		json.Unmarshal([]byte(result), &state)
		if isDone, ok := state["done"].(bool); ok && isDone {
			log.Printf("[%ds] DONE signal received! %s", i+1, result)
			done = true
			time.Sleep(2 * time.Second)
			break
		}
	}

	if !done {
		log.Println("WARNING: Did not receive DONE signal within 60s")
	}

	log.Println("=== Step 5: Read final results ===")

	var finalResult string
	_ = chromedp.Run(ctx,
		chromedp.Evaluate(`JSON.stringify({
			done: window.__dsBrowserDone,
			capture: window.__dsBrowserCapture || '',
			thinking: window.__dsBrowserThinking || '',
			fragType: window.__dsCurrentFragmentType,
			log: window.__dsBrowserLog || []
		})`, &finalResult),
	)

	var final map[string]interface{}
	json.Unmarshal([]byte(finalResult), &final)

	capture, _ := final["capture"].(string)
	thinking, _ := final["thinking"].(string)
	fragType, _ := final["fragType"].(string)
	logArr, _ := final["log"].([]interface{})

	fmt.Println("\n========== TEST RESULTS ==========")
	fmt.Printf("DONE signal: %v\n", final["done"])
	fmt.Printf("Fragment type at end: %s\n", fragType)
	fmt.Printf("Capture length: %d chars\n", len(capture))
	fmt.Printf("Thinking length: %d chars\n", len(thinking))
	fmt.Printf("Capture preview: %q\n", truncate(capture, 300))
	fmt.Printf("Thinking preview: %q\n", truncate(thinking, 300))
	fmt.Printf("Log entries (%d): %v\n", len(logArr), logArr)

	passed := true
	if len(capture) == 0 {
		fmt.Println("\n❌ FAIL: No capture content received!")
		passed = false
	} else {
		fmt.Println("\n✅ PASS: Capture content received")
	}

	if final["done"] == true {
		fmt.Println("✅ PASS: DONE signal received")
	} else {
		fmt.Println("❌ FAIL: DONE signal NOT received")
		passed = false
	}

	hasBatchDone := false
	for _, l := range logArr {
		if ls, ok := l.(string); ok && strings.Contains(ls, "DONE_BATCH") {
			hasBatchDone = true
		}
	}
	if hasBatchDone {
		fmt.Println("✅ PASS: BATCH quasi_status FINISHED detected")
	} else {
		fmt.Println("⚠️  INFO: BATCH quasi_status not in log (may use response/status instead)")
	}

	if len(thinking) > 0 {
		fmt.Println("✅ PASS: Thinking content captured")
	} else {
		fmt.Println("⚠️  INFO: No thinking content (may not use thinking mode)")
	}

	outputFile := "test_injector_result.json"
	os.WriteFile(outputFile, []byte(finalResult), 0644)
	log.Printf("Full results saved to %s", outputFile)

	if passed {
		fmt.Println("\n========== ALL TESTS PASSED ==========")
	} else {
		fmt.Println("\n========== SOME TESTS FAILED ==========")
		os.Exit(1)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
