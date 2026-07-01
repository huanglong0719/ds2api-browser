package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_DefaultPort(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "config.json")
	err := os.WriteFile(path, []byte(`{}`), 0644)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Port != 8766 {
		t.Errorf("Port = %d, want 8766", cfg.Port)
	}
}

func TestLoad_ZeroPortDefaults(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "config.json")
	err := os.WriteFile(path, []byte(`{"port": 0}`), 0644)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Port != 8766 {
		t.Errorf("Port = %d, want 8766", cfg.Port)
	}
}

func TestLoad_ValidConfig(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "config.json")
	data := `{
		"port": 9999,
		"api_key": "test-key",
		"accounts": [{"email": "a@b.com", "password": "pass"}],
		"auto_new_conversation": true,
		"response_timeout_sec": 60
	}`
	err := os.WriteFile(path, []byte(data), 0644)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Port != 9999 {
		t.Errorf("Port = %d, want 9999", cfg.Port)
	}
	if cfg.APIKey != "test-key" {
		t.Errorf("APIKey = %q, want %q", cfg.APIKey, "test-key")
	}
	if len(cfg.Accounts) != 1 {
		t.Fatalf("Accounts len = %d, want 1", len(cfg.Accounts))
	}
	if cfg.Accounts[0].Email != "a@b.com" {
		t.Errorf("Email = %q, want %q", cfg.Accounts[0].Email, "a@b.com")
	}
	if !cfg.AutoNewConversation {
		t.Error("AutoNewConversation = false, want true")
	}
	if cfg.ResponseTimeoutSec != 60 {
		t.Errorf("ResponseTimeoutSec = %d, want 60", cfg.ResponseTimeoutSec)
	}
}

func TestLoad_FileNotFound(t *testing.T) {
	_, err := Load("/nonexistent/path/config.json")
	if err == nil {
		t.Error("Load() should return error for nonexistent file")
	}
}

func TestLoad_InvalidJSON(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "config.json")
	err := os.WriteFile(path, []byte(`{invalid json`), 0644)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Load(path)
	if err == nil {
		t.Error("Load() should return error for invalid JSON")
	}
}

func TestLoad_EmptyFilePathUsesDefault(t *testing.T) {
	// When path is empty, Load falls back to "browser_config.json"
	// This test verifies the default path behavior by temporarily changing cwd
	tmpDir := t.TempDir()
	origWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(origWd)

	// Create a minimal config
	err := os.WriteFile("browser_config.json", []byte(`{}`), 0644)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\") error: %v", err)
	}
	if cfg.Port != 8766 {
		t.Errorf("Port = %d, want 8766", cfg.Port)
	}
}
