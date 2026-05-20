package main

import (
	"os"
	"testing"
	"time"
)

func TestLoadConfigDefaults(t *testing.T) {
	setRequiredEnv(t)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	if cfg.DatabasePort != "5432" {
		t.Fatalf("DatabasePort = %q, want 5432", cfg.DatabasePort)
	}
	if cfg.R2Prefix != "postgres" {
		t.Fatalf("R2Prefix = %q, want postgres", cfg.R2Prefix)
	}
	if cfg.BackupRetentionDays != 14 {
		t.Fatalf("BackupRetentionDays = %d, want 14", cfg.BackupRetentionDays)
	}
	if cfg.BackupCompression != compressionGzip {
		t.Fatalf("BackupCompression = %q, want gzip", cfg.BackupCompression)
	}
}

func TestLoadConfigRejectsRetentionOver14Days(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("BACKUP_RETENTION_DAYS", "15")

	if _, err := LoadConfig(); err == nil {
		t.Fatal("LoadConfig() error = nil, want retention validation error")
	}
}

func TestLoadConfigRejectsInvalidCompression(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("BACKUP_COMPRESSION", "brotli")

	if _, err := LoadConfig(); err == nil {
		t.Fatal("LoadConfig() error = nil, want compression validation error")
	}
}

func TestObjectKeyUsesUTCFoldersAndSanitizedDatabase(t *testing.T) {
	cfg := Config{
		DatabaseName:      "my db",
		R2Prefix:          "postgres",
		BackupCompression: compressionGzip,
	}
	now := time.Date(2026, 5, 20, 2, 0, 0, 0, time.UTC)

	got := cfg.ObjectKey(now)
	want := "postgres/2026/05/20/my_db-20260520T020000Z.dump.gz"
	if got != want {
		t.Fatalf("ObjectKey() = %q, want %q", got, want)
	}
}

func TestObjectKeySupportsZstd(t *testing.T) {
	cfg := Config{
		DatabaseName:      "mydb",
		R2Prefix:          "custom",
		BackupCompression: compressionZstd,
	}
	now := time.Date(2026, 5, 20, 2, 0, 0, 0, time.UTC)

	got := cfg.ObjectKey(now)
	want := "custom/2026/05/20/mydb-20260520T020000Z.dump.zst"
	if got != want {
		t.Fatalf("ObjectKey() = %q, want %q", got, want)
	}
}

func TestLoadDotEnvDoesNotOverrideExistingEnv(t *testing.T) {
	tempDir := t.TempDir()
	originalDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd() error = %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(originalDir); err != nil {
			t.Fatalf("os.Chdir(%q) cleanup error = %v", originalDir, err)
		}
	})

	if err := os.WriteFile(tempDir+"/.env", []byte("DATABASE_HOST=from-file\nR2_BUCKET=from-file\n"), 0600); err != nil {
		t.Fatalf("os.WriteFile(.env) error = %v", err)
	}
	if err := os.Chdir(tempDir); err != nil {
		t.Fatalf("os.Chdir(%q) error = %v", tempDir, err)
	}

	t.Setenv("DATABASE_HOST", "from-env")
	t.Setenv("R2_BUCKET", "")
	if err := os.Unsetenv("R2_BUCKET"); err != nil {
		t.Fatalf("os.Unsetenv(R2_BUCKET) error = %v", err)
	}

	if err := loadDotEnv(); err != nil {
		t.Fatalf("loadDotEnv() error = %v", err)
	}

	if got := os.Getenv("DATABASE_HOST"); got != "from-env" {
		t.Fatalf("DATABASE_HOST = %q, want from-env", got)
	}
	if got := os.Getenv("R2_BUCKET"); got != "from-file" {
		t.Fatalf("R2_BUCKET = %q, want from-file", got)
	}
}

func setRequiredEnv(t *testing.T) {
	t.Helper()

	t.Setenv("DATABASE_HOST", "localhost")
	t.Setenv("DATABASE_USER", "postgres")
	t.Setenv("DATABASE_PASSWORD", "secret")
	t.Setenv("DATABASE_NAME", "app")
	t.Setenv("R2_ACCOUNT_ID", "account")
	t.Setenv("R2_ACCESS_KEY_ID", "access-key")
	t.Setenv("R2_SECRET_ACCESS_KEY", "secret-key")
	t.Setenv("R2_BUCKET", "backups")
}
