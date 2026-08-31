// Package scheduler реализует бизнес-логику выбора версий для загрузки
// (full/delta) и оркестрирует сам процесс скачивания.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"fias-downloader/internal/downloader"
	"fias-downloader/internal/fetcher"
	"fias-downloader/internal/metrics"
	"fias-downloader/internal/model"
	"fias-downloader/internal/repo"
)

type Scheduler struct {
	log         *slog.Logger
	fetcher     *fetcher.Fetcher
	repo        *repo.Repo
	downloader  *downloader.Downloader
	metrics     *metrics.Metrics
	downloadDir string

	// runMu гарантирует, что одновременно выполняется не более одного цикла
	// загрузки в рамках этого процесса (защита от параллельного запуска
	// планового и ручного триггера). Для нескольких экземпляров сервиса
	// нужна распределённая блокировка (напр. Postgres advisory lock) —
	// см. открытые вопросы в ТЗ.
	runMu sync.Mutex
}

func New(log *slog.Logger, f *fetcher.Fetcher, r *repo.Repo, d *downloader.Downloader, mt *metrics.Metrics, downloadDir string) *Scheduler {
	return &Scheduler{log: log, fetcher: f, repo: r, downloader: d, metrics: mt, downloadDir: downloadDir}
}

// RunCycle выполняет один цикл: получает перечень версий и догружает всё,
// что должно быть загружено согласно правилам из ТЗ.
func (s *Scheduler) RunCycle(ctx context.Context) (err error) {
	if !s.runMu.TryLock() {
		return fmt.Errorf("cycle already running, skipped")
	}
	defer s.runMu.Unlock()

	versions, err := s.fetchVersions(ctx)
	if err != nil {
		return fmt.Errorf("fetch version list: %w", err)
	}

	latestFull, err := s.repo.GetLatestCompleted(ctx, model.KindFull)
	switch {
	case errors.Is(err, repo.ErrNotFound):
		// Полной версии ещё нет — грузим последнюю доступную полную версию.
		latest := versions[len(versions)-1]
		s.log.Info("no completed full version yet, downloading latest full",
			"version_id", latest.VersionID)
		err = s.downloadOne(ctx, latest, model.KindFull, false)
		return err

	case err != nil:
		return fmt.Errorf("get latest completed full version: %w", err)
	}

	// Полная версия уже загружена — догружаем недостающие delta-версии
	// строго по возрастанию, без пропусков. Останавливаемся на первой
	// ошибке, чтобы не нарушать последовательность применения дельт.
	s.log.Info("latest completed full version", "version_id", latestFull.VersionID)

	for _, v := range versions {
		if v.VersionID <= latestFull.VersionID {
			continue
		}
		existing, gerr := s.repo.GetByVersionKind(ctx, v.VersionID, model.KindDelta)
		if gerr == nil && existing.Status == model.StatusCompleted {
			continue // уже загружено
		}
		if gerr != nil && !errors.Is(gerr, repo.ErrNotFound) {
			err = fmt.Errorf("check existing delta record: %w", gerr)
			return err
		}

		if v.URLFor(model.KindDelta) == "" {
			s.log.Warn("delta URL is empty for version, skipping", "version_id", v.VersionID)
			continue
		}

		if derr := s.downloadOne(ctx, v, model.KindDelta, false); derr != nil {
			err = fmt.Errorf("download delta for version %d: %w", v.VersionID, derr)
			return err
		}
	}

	return nil
}

// ForceDownloadLatestFull инициирует загрузку последней доступной полной
// версии вне зависимости от того, есть ли уже загруженная полная версия.
// Используется ручным триггером (см. ТЗ, п. 4.2.3).
func (s *Scheduler) ForceDownloadLatestFull(ctx context.Context) (err error) {
	if !s.runMu.TryLock() {
		return fmt.Errorf("cycle already running, skipped")
	}
	defer s.runMu.Unlock()

	versions, err := s.fetchVersions(ctx)
	if err != nil {
		return fmt.Errorf("fetch version list: %w", err)
	}
	latest := versions[len(versions)-1]
	if latest.URLFor(model.KindFull) == "" {
		err = fmt.Errorf("latest version %d has no full URL", latest.VersionID)
		return err
	}

	s.log.Info("manual forced download of latest full version", "version_id", latest.VersionID)
	err = s.downloadOne(ctx, latest, model.KindFull, true)
	return err
}

func (s *Scheduler) fetchVersions(ctx context.Context) ([]model.SourceVersion, error) {
	return s.fetcher.Fetch(ctx)
}

func (s *Scheduler) downloadOne(ctx context.Context, sv model.SourceVersion, kind model.Kind, isManual bool) error {
	url := sv.URLFor(kind)
	if url == "" {
		return fmt.Errorf("version %d has no %s URL", sv.VersionID, kind)
	}

	rec, err := s.repo.GetOrCreatePending(ctx, sv, kind, isManual)
	if err != nil {
		return fmt.Errorf("get or create record: %w", err)
	}
	if rec.Status == model.StatusCompleted && !isManual {
		s.log.Info("already completed, skipping", "version_id", sv.VersionID, "kind", kind)
		return nil
	}

	destPath := filepath.Join(s.downloadDir, fmt.Sprintf("%d_%s.zip", sv.VersionID, kind))
	if err := s.repo.MarkDownloading(ctx, rec.ID, destPath); err != nil {
		return fmt.Errorf("mark downloading: %w", err)
	}

	log := s.log.With("version_id", sv.VersionID, "kind", kind, "url", url, "dest", destPath)
	log.Info("starting download")

	onProgress := func(downloaded, total int64) {
		s.metrics.SetDownloadProgress(string(kind), sv.VersionID, downloaded, total)
		progressCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		if err := s.repo.UpdateProgress(progressCtx, rec.ID, downloaded, total); err != nil {
			log.Warn("failed to persist progress", "error", err)
		}
	}

	doneFn := s.metrics.DownloadStarted(string(kind))
	defer doneFn()
	// Публикуем начальный прогресс сразу, ещё до получения первого блока
	// данных. Это позволяет dashboard показывать активную загрузку даже при
	// медленном или временно зависшем соединении.
	s.metrics.SetDownloadProgress(string(kind), sv.VersionID, 0, 0)
	defer s.metrics.ClearDownloadProgress(string(kind), sv.VersionID)
	result, dlErr := s.downloader.Download(ctx, url, destPath, onProgress)
	if dlErr != nil {
		log.Error("download failed", "error", dlErr)
		if markErr := s.repo.MarkFailed(ctx, rec.ID, dlErr); markErr != nil {
			log.Error("failed to mark record as failed", "error", markErr)
		}
		return dlErr
	}

	if err := s.repo.MarkCompleted(ctx, rec.ID, result.TotalBytes, result.SHA256); err != nil {
		return fmt.Errorf("mark completed: %w", err)
	}
	log.Info("download completed", "bytes", result.TotalBytes, "sha256", result.SHA256)
	return nil
}
