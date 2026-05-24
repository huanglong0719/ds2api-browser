package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/chromedp/chromedp"
)

func main() {
	log.Println("=== MINIMAL CHROME TEST ===")

	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(),
		chromedp.Flag("no-first-run", true),
		chromedp.Flag("disable-extensions", true),
		chromedp.Flag("remote-debugging-port", "9222"),
		chromedp.WindowSize(1280, 900),
	)
	defer allocCancel()

	log.Println("allocator created, creating context...")
	ctx, cancel := chromedp.NewContext(allocCtx)
	defer cancel()

	log.Println("context created, running first action (this launches browser)...")

	tctx, tcancel := context.WithTimeout(ctx, 30*time.Second)
	defer tcancel()

	err := chromedp.Run(tctx,
		chromedp.Navigate("https://www.google.com"),
	)
	if err != nil {
		log.Fatalf("FIRST ACTION FAILED: %v", err)
	}

	var title string
	if err := chromedp.Run(ctx, chromedp.Title(&title)); err != nil {
		log.Fatalf("GET TITLE FAILED: %v", err)
	}

	log.Printf("SUCCESS! Title: %q", title)
	fmt.Printf("OK: %s\n", title)
}
