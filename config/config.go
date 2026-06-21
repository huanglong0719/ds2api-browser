package config

import (
	"encoding/json"
	"os"
)

type Account struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type Config struct {
	Port               int       `json:"port"`
	Accounts           []Account `json:"accounts"`
	APIKey             string    `json:"api_key"`
	ChromePath         string    `json:"chrome_path,omitempty"`
	AutoNewConversation bool     `json:"auto_new_conversation,omitempty"`
	ResponseTimeoutSec int       `json:"response_timeout_sec,omitempty"` // 响应等待超时(秒),默认120
}

func Load(path string) (*Config, error) {
	if path == "" {
		path = "browser_config.json"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := &Config{Port: 8766}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	if cfg.Port == 0 {
		cfg.Port = 8766
	}
	return cfg, nil
}
