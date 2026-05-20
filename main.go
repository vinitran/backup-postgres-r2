package main

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/joho/godotenv"
	"github.com/klauspost/compress/zstd"
	"github.com/robfig/cron/v3"
)

const (
	compressionGzip = "gzip"
	compressionZstd = "zstd"
)

type Config struct {
	DatabaseHost     string
	DatabasePort     string
	DatabaseUser     string
	DatabasePassword string
	DatabaseName     string

	R2AccountID       string
	R2AccessKeyID     string
	R2SecretAccessKey string
	R2Bucket          string
	R2Prefix          string

	BackupCron          string
	TZ                  string
	BackupRetentionDays int
	BackupCompression   string
}

type S3Uploader interface {
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

type BackupService struct {
	cfg Config
	s3  S3Uploader
}

func main() {
	if err := run(); err != nil {
		logError("%v", err)
		os.Exit(1)
	}
}

func run() error {
	if err := loadDotEnv(); err != nil {
		return err
	}

	mode := "serve"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch mode {
	case "backup":
		cfg, err := LoadConfig()
		if err != nil {
			return err
		}
		service, err := NewBackupService(ctx, cfg)
		if err != nil {
			return err
		}
		return service.Run(ctx, time.Now().UTC())
	case "serve":
		cfg, err := LoadConfig()
		if err != nil {
			return err
		}
		return serve(ctx, cfg)
	case "help", "-h", "--help":
		printUsage()
		return nil
	default:
		printUsage()
		return fmt.Errorf("unknown command %q", mode)
	}
}

func loadDotEnv() error {
	if err := godotenv.Load(); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("load .env: %w", err)
	}
	return nil
}

func LoadConfig() (Config, error) {
	cfg := Config{
		DatabaseHost:        envFirst("DATABASE_HOST", "POSTGRES_HOST"),
		DatabasePort:        envFirstDefault("5432", "DATABASE_PORT", "POSTGRES_PORT"),
		DatabaseUser:        envFirst("DATABASE_USER", "POSTGRES_USER"),
		DatabasePassword:    envFirst("DATABASE_PASSWORD", "POSTGRES_PASSWORD"),
		DatabaseName:        envFirst("DATABASE_NAME", "POSTGRES_DB"),
		R2AccountID:         os.Getenv("R2_ACCOUNT_ID"),
		R2AccessKeyID:       os.Getenv("R2_ACCESS_KEY_ID"),
		R2SecretAccessKey:   os.Getenv("R2_SECRET_ACCESS_KEY"),
		R2Bucket:            os.Getenv("R2_BUCKET"),
		R2Prefix:            normalizePrefix(envDefault("R2_PREFIX", "postgres")),
		BackupCron:          envDefault("BACKUP_CRON", "0 2 * * *"),
		TZ:                  envDefault("TZ", "Asia/Ho_Chi_Minh"),
		BackupCompression:   envDefault("BACKUP_COMPRESSION", compressionGzip),
		BackupRetentionDays: 14,
	}

	required := []struct {
		name  string
		value string
	}{
		{"DATABASE_HOST", cfg.DatabaseHost},
		{"DATABASE_USER", cfg.DatabaseUser},
		{"DATABASE_PASSWORD", cfg.DatabasePassword},
		{"DATABASE_NAME", cfg.DatabaseName},
		{"R2_ACCOUNT_ID", cfg.R2AccountID},
		{"R2_ACCESS_KEY_ID", cfg.R2AccessKeyID},
		{"R2_SECRET_ACCESS_KEY", cfg.R2SecretAccessKey},
		{"R2_BUCKET", cfg.R2Bucket},
	}
	for _, item := range required {
		if strings.TrimSpace(item.value) == "" {
			return Config{}, fmt.Errorf("missing required environment variable: %s", item.name)
		}
	}

	if _, err := parsePort(cfg.DatabasePort); err != nil {
		return Config{}, err
	}

	retentionDays, err := parseRetention(envDefault("BACKUP_RETENTION_DAYS", "14"))
	if err != nil {
		return Config{}, err
	}
	cfg.BackupRetentionDays = retentionDays

	switch cfg.BackupCompression {
	case compressionGzip, compressionZstd:
	default:
		return Config{}, fmt.Errorf("BACKUP_COMPRESSION must be either %q or %q", compressionGzip, compressionZstd)
	}

	if hasLineBreak(cfg.BackupCron) {
		return Config{}, errors.New("BACKUP_CRON must be a single-line cron expression")
	}
	if hasLineBreak(cfg.TZ) {
		return Config{}, errors.New("TZ must be a single-line timezone value")
	}

	return cfg, nil
}

func NewBackupService(ctx context.Context, cfg Config) (*BackupService, error) {
	awsCfg, err := config.LoadDefaultConfig(
		ctx,
		config.WithRegion("auto"),
		config.WithRequestChecksumCalculation(aws.RequestChecksumCalculationWhenRequired),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			cfg.R2AccessKeyID,
			cfg.R2SecretAccessKey,
			"",
		)),
	)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}

	endpoint := fmt.Sprintf("https://%s.r2.cloudflarestorage.com", cfg.R2AccountID)
	client := s3.NewFromConfig(awsCfg, func(options *s3.Options) {
		options.BaseEndpoint = aws.String(endpoint)
		options.UsePathStyle = true
	})

	return &BackupService{cfg: cfg, s3: client}, nil
}

func (b *BackupService) Run(ctx context.Context, now time.Time) error {
	objectKey := b.cfg.ObjectKey(now)
	endpoint := fmt.Sprintf("https://%s.r2.cloudflarestorage.com", b.cfg.R2AccountID)
	logInfo("Starting PostgreSQL backup for database %q on %s:%s", b.cfg.DatabaseName, b.cfg.DatabaseHost, b.cfg.DatabasePort)

	backupFile, size, cleanup, err := b.createCompressedDumpFile(ctx)
	if err != nil {
		return fmt.Errorf("backup failed: %w", err)
	}
	defer cleanup()

	logInfo("Created compressed dump %q (%d bytes)", backupFile, size)
	logInfo("Uploading to R2 bucket %q with key %q", b.cfg.R2Bucket, objectKey)

	file, err := os.Open(backupFile)
	if err != nil {
		return fmt.Errorf("open compressed dump for upload: %w", err)
	}
	defer file.Close()

	_, uploadErr := b.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(b.cfg.R2Bucket),
		Key:           aws.String(objectKey),
		Body:          file,
		ContentLength: aws.Int64(size),
	})
	if uploadErr != nil {
		return fmt.Errorf("upload to %s failed: %w", endpoint, uploadErr)
	}

	logInfo("Uploaded backup to s3://%s/%s (%d compressed bytes)", b.cfg.R2Bucket, objectKey, size)
	logInfo("Retention is expected to be enforced by an R2 Lifecycle rule: prefix %q, delete objects after %d days", b.cfg.R2Prefix+"/", b.cfg.BackupRetentionDays)
	return nil
}

func (b *BackupService) createCompressedDumpFile(ctx context.Context) (string, int64, func(), error) {
	tempDir, err := os.MkdirTemp("", "db-backup-r2-*")
	if err != nil {
		return "", 0, nil, fmt.Errorf("create temp dir: %w", err)
	}

	cleanup := func() {
		if err := os.RemoveAll(tempDir); err != nil {
			logError("remove temp dir %q: %v", tempDir, err)
		}
	}

	extension := "dump.gz"
	if b.cfg.BackupCompression == compressionZstd {
		extension = "dump.zst"
	}
	backupFile := fmt.Sprintf("%s/%s-%s.%s", tempDir, sanitizeName(b.cfg.DatabaseName), time.Now().UTC().Format("20060102T150405Z"), extension)

	file, err := os.Create(backupFile)
	if err != nil {
		cleanup()
		return "", 0, nil, fmt.Errorf("create compressed dump file: %w", err)
	}

	if err := b.streamCompressedDump(ctx, file); err != nil {
		_ = file.Close()
		cleanup()
		return "", 0, nil, err
	}

	if err := file.Close(); err != nil {
		cleanup()
		return "", 0, nil, fmt.Errorf("close compressed dump file: %w", err)
	}

	info, err := os.Stat(backupFile)
	if err != nil {
		cleanup()
		return "", 0, nil, fmt.Errorf("stat compressed dump file: %w", err)
	}
	if info.Size() == 0 {
		cleanup()
		return "", 0, nil, errors.New("compressed dump file is empty")
	}

	return backupFile, info.Size(), cleanup, nil
}

func (b *BackupService) streamCompressedDump(ctx context.Context, output io.Writer) error {
	cmd := exec.CommandContext(
		ctx,
		"pg_dump",
		"--host", b.cfg.DatabaseHost,
		"--port", b.cfg.DatabasePort,
		"--username", b.cfg.DatabaseUser,
		"--dbname", b.cfg.DatabaseName,
		"--format=custom",
		"--no-owner",
		"--no-privileges",
	)
	cmd.Env = append(os.Environ(), "PGPASSWORD="+b.cfg.DatabasePassword)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open pg_dump stdout: %w", err)
	}

	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start pg_dump: %w", err)
	}

	compressor, err := newCompressor(b.cfg.BackupCompression, output)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return err
	}

	if _, err := io.Copy(compressor, stdout); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return fmt.Errorf("read pg_dump output: %w", err)
	}

	if err := cmd.Wait(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return fmt.Errorf("pg_dump failed: %s", message)
	}

	if err := compressor.Close(); err != nil {
		return fmt.Errorf("finish %s compression: %w", b.cfg.BackupCompression, err)
	}

	return nil
}

func serve(ctx context.Context, cfg Config) error {
	location, err := time.LoadLocation(cfg.TZ)
	if err != nil {
		return fmt.Errorf("load TZ %q: %w", cfg.TZ, err)
	}

	service, err := NewBackupService(ctx, cfg)
	if err != nil {
		return err
	}

	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
	scheduler := cron.New(
		cron.WithLocation(location),
		cron.WithParser(parser),
		cron.WithChain(cron.SkipIfStillRunning(cronLogger{})),
	)

	if _, err := scheduler.AddFunc(cfg.BackupCron, func() {
		if err := service.Run(ctx, time.Now().UTC()); err != nil {
			logError("%v", err)
		}
	}); err != nil {
		return fmt.Errorf("parse BACKUP_CRON %q: %w", cfg.BackupCron, err)
	}

	logInfo("Starting Go scheduler with BACKUP_CRON=%q TZ=%q", cfg.BackupCron, cfg.TZ)
	scheduler.Start()
	<-ctx.Done()

	logInfo("Stopping scheduler")
	stopCtx := scheduler.Stop()
	select {
	case <-stopCtx.Done():
	case <-time.After(30 * time.Second):
		logError("Timed out waiting for running backup job to stop")
	}
	return nil
}

func newCompressor(name string, output io.Writer) (io.WriteCloser, error) {
	switch name {
	case compressionGzip:
		return gzip.NewWriter(output), nil
	case compressionZstd:
		return zstd.NewWriter(output)
	default:
		return nil, fmt.Errorf("unsupported compression %q", name)
	}
}

func (cfg Config) ObjectKey(now time.Time) string {
	utc := now.UTC()
	extension := "dump.gz"
	if cfg.BackupCompression == compressionZstd {
		extension = "dump.zst"
	}
	return fmt.Sprintf(
		"%s/%s/%s-%s.%s",
		cfg.R2Prefix,
		utc.Format("2006/01/02"),
		sanitizeName(cfg.DatabaseName),
		utc.Format("20060102T150405Z"),
		extension,
	)
}

func envDefault(name string, fallback string) string {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	return value
}

func envFirst(names ...string) string {
	for _, name := range names {
		if value := os.Getenv(name); value != "" {
			return value
		}
	}
	return ""
}

func envFirstDefault(fallback string, names ...string) string {
	if value := envFirst(names...); value != "" {
		return value
	}
	return fallback
}

func normalizePrefix(prefix string) string {
	prefix = strings.Trim(prefix, "/")
	if prefix == "" {
		return "postgres"
	}
	return prefix
}

func sanitizeName(value string) string {
	var builder strings.Builder
	for _, r := range value {
		switch {
		case r >= 'A' && r <= 'Z':
			builder.WriteRune(r)
		case r >= 'a' && r <= 'z':
			builder.WriteRune(r)
		case r >= '0' && r <= '9':
			builder.WriteRune(r)
		case r == '_', r == '.', r == '-':
			builder.WriteRune(r)
		default:
			builder.WriteByte('_')
		}
	}
	if builder.Len() == 0 || !utf8.ValidString(builder.String()) {
		return "database"
	}
	return builder.String()
}

func parsePort(value string) (int, error) {
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("DATABASE_PORT must be a number between 1 and 65535")
	}
	return port, nil
}

func parseRetention(value string) (int, error) {
	days, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("BACKUP_RETENTION_DAYS must be a number")
	}
	if days < 1 || days > 14 {
		return 0, fmt.Errorf("BACKUP_RETENTION_DAYS must be between 1 and 14. Configure R2 Lifecycle deletion for actual retention")
	}
	return days, nil
}

func hasLineBreak(value string) bool {
	return strings.ContainsAny(value, "\r\n")
}

func logInfo(format string, args ...any) {
	logLine(os.Stdout, format, args...)
}

func logError(format string, args ...any) {
	logLine(os.Stderr, "ERROR: "+format, args...)
}

func logLine(writer io.Writer, format string, args ...any) {
	fmt.Fprintf(writer, "[%s] %s\n", time.Now().UTC().Format(time.RFC3339), fmt.Sprintf(format, args...))
}

func printUsage() {
	fmt.Fprintln(os.Stdout, "Usage: db-backup-r2 [serve|backup]")
}

type cronLogger struct{}

func (cronLogger) Info(msg string, keysAndValues ...any) {
	logInfo("%s%s", msg, formatKeyValues(keysAndValues...))
}

func (cronLogger) Error(err error, msg string, keysAndValues ...any) {
	logError("%s: %v%s", msg, err, formatKeyValues(keysAndValues...))
}

func formatKeyValues(keysAndValues ...any) string {
	if len(keysAndValues) == 0 {
		return ""
	}

	parts := make([]string, 0, len(keysAndValues)/2)
	for i := 0; i < len(keysAndValues); i += 2 {
		key := fmt.Sprint(keysAndValues[i])
		value := "<missing>"
		if i+1 < len(keysAndValues) {
			value = fmt.Sprint(keysAndValues[i+1])
		}
		parts = append(parts, fmt.Sprintf("%s=%s", key, value))
	}
	return " " + strings.Join(parts, " ")
}
