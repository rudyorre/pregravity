package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateFailClosedSecurity(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config should be valid: %v", err)
	}

	// Telegram enabled without allowlist must fail closed.
	cfg.Telegram.Enabled = true
	cfg.Telegram.AllowedChatIDs = nil
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error when telegram is enabled without allowed_chat_ids")
	}
	cfg.Telegram.AllowedChatIDs = []int64{123456}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected valid telegram config with allowlist: %v", err)
	}

	// HTTP binding to non-loopback must be rejected.
	cfg.HTTP.Enabled = true
	cfg.HTTP.Listen = "0.0.0.0:8787"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error when http.listen is non-loopback")
	}
	cfg.HTTP.Listen = "127.0.0.1:8787"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected loopback http.listen to pass: %v", err)
	}
}

func TestSaveLoadAndEnvOverlay(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	envPath := filepath.Join(dir, "env")

	orig := Default()
	orig.DataDir = filepath.Join(dir, "data")
	orig.Model = "gemini-3.5-flash-lite"
	if err := Save(cfgPath, orig); err != nil {
		t.Fatalf("save: %v", err)
	}

	if err := os.WriteFile(envPath, []byte("PREGRAVITY_GEMINI_API_KEY=test-key-from-env\n"), 0o600); err != nil {
		t.Fatalf("write env: %v", err)
	}
	t.Setenv("PREGRAVITY_GEMINI_API_KEY", "")
	os.Unsetenv("PREGRAVITY_GEMINI_API_KEY")
	t.Cleanup(func() { os.Unsetenv("PREGRAVITY_GEMINI_API_KEY") })

	loaded, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Model != "gemini-3.5-flash-lite" {
		t.Fatalf("got model %q, want gemini-3.5-flash-lite", loaded.Model)
	}
	if loaded.GeminiAPIKey != "test-key-from-env" {
		t.Fatalf("got api key %q, want test-key-from-env", loaded.GeminiAPIKey)
	}
}
