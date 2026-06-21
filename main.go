package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"ds2api-browser/api"
	"ds2api-browser/browser"
	"ds2api-browser/config"
)

func main() {
	configPath := flag.String("config", "browser_config.json", "config file path")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("[main] config load failed: %v", err)
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

	chatHandler := browser.NewChatHandler(session, cfg.ResponseTimeoutSec)
	apiHandler := api.NewHandler(cfg, chatHandler)

	addr := fmt.Sprintf(":%d", cfg.Port)
	server := &http.Server{
		Addr:         addr,
		Handler:      apiHandler.Router(),
		ReadTimeout:  60 * time.Second,
		WriteTimeout: 180 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		log.Printf("[main] listening on http://127.0.0.1%s", addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-serverErr:
		log.Printf("[main] server error: %v", err)
	case sig := <-sigCh:
		log.Printf("[main] received signal: %v", sig)
	}

	log.Println("[main] shutting down...")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("[main] server shutdown error: %v", err)
	}
	session.Close()
}
