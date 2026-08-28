package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoad_AppliesDefaultsForOmittedFields(t *testing.T) {
	path := writeTempConfig(t, `
postgres:
  dsn: "postgres://user:pass@localhost:5432/fias?sslmode=disable"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.Source.URL != "https://fias.nalog.ru/WebServices/Public/GetAllDownloadFileInfo" {
		t.Errorf("unexpected default source.url: %q", cfg.Source.URL)
	}
	if cfg.Download.Dir != "./data/downloads" {
		t.Errorf("unexpected default download.dir: %q", cfg.Download.Dir)
	}
	if cfg.Download.MaxRetries != 5 {
		t.Errorf("unexpected default download.max_retries: %d", cfg.Download.MaxRetries)
	}
	if cfg.HTTP.ListenAddr != ":8080" {
		t.Errorf("unexpected default http.listen_addr: %q", cfg.HTTP.ListenAddr)
	}
	if cfg.Scheduler.PollInterval.Duration != 6*time.Hour {
		t.Errorf("unexpected default scheduler.poll_interval: %v", cfg.Scheduler.PollInterval.Duration)
	}
}

func TestLoad_OverridesDefaultsFromFile(t *testing.T) {
	path := writeTempConfig(t, `
postgres:
  dsn: "postgres://user:pass@localhost:5432/fias?sslmode=disable"
source:
  url: "https://example.org/versions.json"
  http_timeout: "15s"
download:
  dir: "/data/archives"
  stall_timeout: "90s"
  max_retries: 3
  retry_base_delay: "2s"
  progress_save_interval: "1s"
http:
  listen_addr: ":9999"
scheduler:
  poll_interval: "1h"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.Source.URL != "https://example.org/versions.json" {
		t.Errorf("source.url not overridden: %q", cfg.Source.URL)
	}
	if cfg.Source.HTTPTimeout.Duration != 15*time.Second {
		t.Errorf("source.http_timeout not overridden: %v", cfg.Source.HTTPTimeout.Duration)
	}
	if cfg.Download.Dir != "/data/archives" {
		t.Errorf("download.dir not overridden: %q", cfg.Download.Dir)
	}
	if cfg.Download.MaxRetries != 3 {
		t.Errorf("download.max_retries not overridden: %d", cfg.Download.MaxRetries)
	}
	if cfg.HTTP.ListenAddr != ":9999" {
		t.Errorf("http.listen_addr not overridden: %q", cfg.HTTP.ListenAddr)
	}
	if cfg.Scheduler.PollInterval.Duration != time.Hour {
		t.Errorf("scheduler.poll_interval not overridden: %v", cfg.Scheduler.PollInterval.Duration)
	}
}

func TestLoad_MissingPostgresDSN_Fails(t *testing.T) {
	path := writeTempConfig(t, `
source:
  url: "https://example.org/versions.json"
`)

	if _, err := Load(path); err == nil {
		t.Fatal("expected error for missing postgres.dsn, got nil")
	}
}

func TestLoad_InvalidDuration_Fails(t *testing.T) {
	path := writeTempConfig(t, `
postgres:
  dsn: "postgres://user:pass@localhost:5432/fias?sslmode=disable"
scheduler:
  poll_interval: "not-a-duration"
`)

	if _, err := Load(path); err == nil {
		t.Fatal("expected error for invalid duration, got nil")
	}
}

func TestLoad_MissingFile_Fails(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml")); err == nil {
		t.Fatal("expected error for missing config file, got nil")
	}
}
