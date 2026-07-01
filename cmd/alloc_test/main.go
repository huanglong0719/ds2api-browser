package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/chromedp/chromedp"
)

func main() {
	port := 9222

	opts := []chromedp.ExecAllocatorOption{
		chromedp.Flag("no-first-run", true),
		chromedp.Flag("no-default-browser-check", true),
		chromedp.Flag("disable-popup-blocking", true),
		chromedp.Flag("disable-extensions", true),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("remote-debugging-port", fmt.Sprintf("%d", port)),
		chromedp.Flag("user-data-dir", `C:\Users\long\ds2api-browser-profile`),
		chromedp.WindowSize(1280, 900),
	}

	log.Println("starting Chrome via ExecAllocator...")
	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer allocCancel()

	log.Println("creating browser context...")
	browserCtx, browserCancel := chromedp.NewContext(allocCtx)
	defer browserCancel()

	for i := 0; i < 12; i++ {
		time.Sleep(10 * time.Second)
		var title string
		err := chromedp.Run(browserCtx, chromedp.Title(&title))
		if err != nil {
			log.Printf("[%ds] FAIL: %v | alloc.Err=%v | browser.Err=%v",
				(i+1)*10, err, allocCtx.Err(), browserCtx.Err())
		} else {
			log.Printf("[%ds] OK: title=%q | alloc.Err=%v | browser.Err=%v",
				(i+1)*10, title, allocCtx.Err(), browserCtx.Err())
		}
	}

	log.Println("test complete")
}