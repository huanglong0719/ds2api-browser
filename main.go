package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"ds2api-browser/api"
	"ds2api-browser/browser"
	"ds2api-browser/config"
)

func main() {
	configPath := flag.String("config", "browser_config.json", "config file path")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Printf("[main] config load warning: %v", err)
	}
	if len(cfg.Accounts) == 0 {
		log.Println("[main] no accounts in config, add them via browser_config.json")
	}

	session := browser.NewSession(cfg)
	if err := session.Start(); err != nil {
		log.Fatalf("[main] browser start failed: %v", err)
	}
	log.Println("[main] browser launched")

	ctx := context.Background()
	if len(cfg.Accounts) > 0 {
		acc := cfg.Accounts[0]
		if err := session.Login(ctx, acc.Email, acc.Password); err != nil {
			log.Printf("[main] initial login failed: %v", err)
		} else {
			log.Println("[main] initial login success")
		}
	}

	chatHandler := browser.NewChatHandler(session)
	apiHandler := api.NewHandler(cfg, chatHandler)

	addr := fmt.Sprintf(":%d", cfg.Port)
	server := &http.Server{Addr: addr, Handler: apiHandler.Router()}

	go func() {
		log.Printf("[main] listening on http://127.0.0.1%s", addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[main] server error: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("[main] shutting down...")
	server.Shutdown(context.Background())
	session.Close()
}
