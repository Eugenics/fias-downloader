// Command fias-downloader — микросервис загрузки справочника адресов
// ФИАС/ГАР: отслеживает версии в PostgreSQL, автоматически догружает
// недостающие delta-версии после первой полной загрузки, поддерживает
// докачку прерванных загрузок, экспортирует Prometheus-метрики и пишет
// логи в БД (в дополнение к stdout).
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"fias-downloader/internal/api"
	"fias-downloader/internal/config"
	"fias-downloader/internal/downloader"
	"fias-downloader/internal/fetcher"
	"fias-downloader/internal/logstore"
	"fias-downloader/internal/metrics"
	"fias-downloader/internal/repo"
	"fias-downloader/internal/scheduler"
)

func main() {
	// Стартовый логгер только в stdout — до подключения к БД писать логи
	// в неё ещё некуда.
	bootLogger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := run(bootLogger); err != nil {
		bootLogger.Error("service exited with error", "error", err)
		os.Exit(1)
	}
}

func run(bootLogger *slog.Logger) error {
	configPath := flag.String("config", envOr("FIAS_CONFIG_PATH", "config.yaml"),
		"путь к YAML-файлу конфигурации (можно также задать через FIAS_CONFIG_PATH)")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	bootLogger.Info("configuration loaded", "path", *configPath)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := repo.Open(cfg.Postgres.DSN)
	if err != nil {
		return err
	}
	defer db.Close()

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := db.Ping(pingCtx); err != nil {
		return err
	}
	if err := db.EnsureSchema(ctx); err != nil {
		return err
	}
	if err := logstore.EnsureSchema(ctx, db.DB()); err != nil {
		return err
	}

	// Логи пишутся одновременно в stdout (JSON, для docker logs / агрегаторов)
	// и в таблицу logs в PostgreSQL (для запросов и корреляции с version_info
	// без похода во внешнюю систему логирования).
	stdoutHandler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})
	dbHandler := logstore.NewDBHandler(db.DB(), logstore.Options{MinLevel: slog.LevelInfo})
	logger := slog.New(logstore.NewTeeHandler(stdoutHandler, dbHandler))
	slog.SetDefault(logger)

	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := dbHandler.Close(closeCtx); err != nil {
			bootLogger.Warn("db log handler did not flush cleanly", "error", err)
		}
	}()

	reg := prometheus.NewRegistry()
	mt := metrics.New(reg)

	f := fetcher.New(cfg.Source.URL, cfg.Source.HTTPTimeout.Duration)

	dl := downloader.New(&http.Client{Timeout: 0}, downloader.Options{
		MaxRetries:     cfg.Download.MaxRetries,
		RetryBaseDelay: cfg.Download.RetryBaseDelay.Duration,
		StallTimeout:   cfg.Download.StallTimeout.Duration,
		ProgressEvery:  cfg.Download.ProgressSaveInterval.Duration,
	})
	// Таймаут на весь запрос намеренно не задаётся (Timeout: 0) — загрузка
	// больших архивов может занимать долго; вместо этого используется
	// защита от "зависания" по неактивности (StallTimeout) и общий
	// контекст сервиса, отменяемый по сигналу остановки.

	sch := scheduler.New(logger, f, db, dl, mt, cfg.Download.Dir)

	srv := &http.Server{
		Addr:    cfg.HTTP.ListenAddr,
		Handler: api.New(logger, sch, reg).Handler(),
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("http api listening", "addr", cfg.HTTP.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	go runLoop(ctx, logger, sch, cfg.Scheduler.PollInterval.Duration)

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	case err := <-errCh:
		logger.Error("http server failed", "error", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// runLoop периодически запускает цикл проверки/загрузки версий. Первый
// прогон выполняется сразу при старте сервиса, не дожидаясь первого тика.
func runLoop(ctx context.Context, logger *slog.Logger, sch *scheduler.Scheduler, interval time.Duration) {
	runOnce := func() {
		if err := sch.RunCycle(ctx); err != nil {
			logger.Error("scheduled cycle failed", "error", err)
		}
	}

	runOnce()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runOnce()
		}
	}
}

// envOr — единственное оставшееся использование переменных окружения:
// позволяет переопределить путь к YAML-файлу конфигурации без изменения
// команды запуска (удобно в docker-compose). Всё остальное содержимое
// конфигурации читается исключительно из самого YAML-файла.
func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}
