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
	Port       int       `json:"port"`
	Accounts   []Account `json:"accounts"`
	APIKey     string    `json:"api_key"`
	ChromePath string    `json:"chrome_path,omitempty"`
}

func Load(path string) (*Config, error) {
	cfg := &Config{Port: 8766}
	if path == "" {
		path = "browser_config.json"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return cfg, err
	}
	if cfg.Port == 0 {
		cfg.Port = 8766
	}
	return cfg, nil
}
