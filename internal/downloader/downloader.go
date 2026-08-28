// Package downloader выполняет потоковую загрузку файлов с поддержкой
// докачки (HTTP Range) и ретраями при временных сетевых ошибках.
package downloader

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"os"
	"time"
)

// ProgressFunc вызывается периодически в процессе загрузки с текущим
// количеством загруженных байт и (если известно) общим размером файла.
// totalBytes может быть 0, если сервер не сообщил Content-Length.
type ProgressFunc func(downloadedBytes, totalBytes int64)

type Options struct {
	MaxRetries     int
	RetryBaseDelay time.Duration
	StallTimeout   time.Duration // максимальная пауза без новых данных
	ProgressEvery  time.Duration // не чаще, чем раз в этот интервал, дёргать ProgressFunc
}

type Downloader struct {
	client *http.Client
	opts   Options
}

func New(client *http.Client, opts Options) *Downloader {
	if opts.MaxRetries <= 0 {
		opts.MaxRetries = 5
	}
	if opts.RetryBaseDelay <= 0 {
		opts.RetryBaseDelay = 5 * time.Second
	}
	if opts.StallTimeout <= 0 {
		opts.StallTimeout = 2 * time.Minute
	}
	if opts.ProgressEvery <= 0 {
		opts.ProgressEvery = 5 * time.Second
	}
	return &Downloader{client: client, opts: opts}
}

// Result — итог успешной загрузки.
type Result struct {
	TotalBytes int64
	SHA256     string
}

// Download скачивает url в destPath. Если в destPath уже есть частично
// загруженный файл, выполняется докачка через заголовок Range. Если сервер
// не поддерживает Range (отвечает 200 вместо 206 на запрос с Range),
// загрузка перезапускается с нуля. Функция сама делает повторные попытки
// при временных сетевых ошибках (с экспоненциальным backoff), поэтому
// вызывающему коду достаточно вызвать её один раз за цикл загрузки.
func (d *Downloader) Download(ctx context.Context, url, destPath string, onProgress ProgressFunc) (Result, error) {
	var lastErr error
	for attempt := 0; attempt <= d.opts.MaxRetries; attempt++ {
		if attempt > 0 {
			delay := backoff(d.opts.RetryBaseDelay, attempt)
			select {
			case <-ctx.Done():
				return Result{}, ctx.Err()
			case <-time.After(delay):
			}
		}

		res, err := d.attempt(ctx, url, destPath, onProgress)
		if err == nil {
			return res, nil
		}
		lastErr = err
		if !isRetryable(err) {
			return Result{}, err
		}
	}
	return Result{}, fmt.Errorf("download failed after %d attempts: %w", d.opts.MaxRetries+1, lastErr)
}

func (d *Downloader) attempt(ctx context.Context, url, destPath string, onProgress ProgressFunc) (Result, error) {
	var offset int64
	if fi, err := os.Stat(destPath); err == nil {
		offset = fi.Size()
	} else if !os.IsNotExist(err) {
		return Result{}, fmt.Errorf("stat existing file: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Result{}, fmt.Errorf("build request: %w", err)
	}
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	var (
		file       *os.File
		flags      = os.O_CREATE | os.O_WRONLY
		startBytes int64
	)

	switch resp.StatusCode {
	case http.StatusOK:
		// Сервер отдал файл целиком: если мы просили Range, но получили 200,
		// значит докачка не поддерживается — начинаем заново.
		flags |= os.O_TRUNC
		startBytes = 0
	case http.StatusPartialContent:
		flags |= os.O_APPEND
		startBytes = offset
	case http.StatusRequestedRangeNotSatisfiable:
		// Файл на диске уже не меньше файла на сервере (например, версия
		// перевыпущена с меньшим размером) — начинаем заново.
		flags |= os.O_TRUNC
		startBytes = 0
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return Result{}, fmt.Errorf("unexpected status %d for %s: %s", resp.StatusCode, url, string(body))
	}

	if err := os.MkdirAll(dirOf(destPath), 0o755); err != nil {
		return Result{}, fmt.Errorf("mkdir: %w", err)
	}
	file, err = os.OpenFile(destPath, flags, 0o644)
	if err != nil {
		return Result{}, fmt.Errorf("open dest file: %w", err)
	}
	defer file.Close()

	hasher := sha256.New()
	if startBytes > 0 {
		// Для честной контрольной суммы всего файла при докачке нужно
		// пересчитать хэш уже имеющейся части.
		if err := rehashExisting(destPath, startBytes, hasher); err != nil {
			return Result{}, fmt.Errorf("rehash existing part: %w", err)
		}
	}

	// Content-Length в ответе на Range-запрос — это размер ОСТАВШЕЙСЯ части,
	// поэтому для отображения прогресса складываем его со смещением.
	expectedTotal := int64(0)
	if resp.ContentLength > 0 {
		expectedTotal = startBytes + resp.ContentLength
	}

	progress := func(written int64) {
		if onProgress != nil {
			onProgress(startBytes+written, expectedTotal)
		}
	}

	written, err := copyWithStallGuard(ctx, io.MultiWriter(file, hasher), resp.Body, d.opts.StallTimeout, d.opts.ProgressEvery, progress)
	total := startBytes + written
	if err != nil {
		return Result{}, fmt.Errorf("copy body (written=%d): %w", total, err)
	}

	return Result{
		TotalBytes: total,
		SHA256:     hex.EncodeToString(hasher.Sum(nil)),
	}, nil
}

func rehashExisting(path string, n int64, w io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.CopyN(w, f, n)
	if err == io.EOF {
		return nil
	}
	return err
}

// copyWithStallGuard копирует данные, прерывая загрузку, если за
// stallTimeout не пришло ни одного байта (защита от "зависших" соединений,
// которые формально не рвутся, но и не передают данные). onProgress
// вызывается не чаще, чем раз в progressEvery.
func copyWithStallGuard(ctx context.Context, dst io.Writer, src io.Reader, stallTimeout, progressEvery time.Duration, onProgress func(written int64)) (int64, error) {
	buf := make([]byte, 256*1024)
	var total int64
	var lastProgress time.Time
	type readResult struct {
		n   int
		err error
	}

	for {
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		default:
		}

		resCh := make(chan readResult, 1)
		go func() {
			n, err := src.Read(buf)
			resCh <- readResult{n, err}
		}()

		select {
		case <-ctx.Done():
			return total, ctx.Err()
		case <-time.After(stallTimeout):
			return total, fmt.Errorf("stalled: no data for %s", stallTimeout)
		case r := <-resCh:
			if r.n > 0 {
				if _, werr := dst.Write(buf[:r.n]); werr != nil {
					return total, werr
				}
				total += int64(r.n)
				if onProgress != nil && time.Since(lastProgress) >= progressEvery {
					lastProgress = time.Now()
					onProgress(total)
				}
			}
			if r.err != nil {
				if r.err == io.EOF {
					if onProgress != nil {
						onProgress(total)
					}
					return total, nil
				}
				return total, r.err
			}
		}
	}
}

func dirOf(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[:i]
		}
	}
	return "."
}

func backoff(base time.Duration, attempt int) time.Duration {
	d := time.Duration(float64(base) * math.Pow(2, float64(attempt-1)))
	jitter := time.Duration(rand.Int63n(int64(base)))
	return d + jitter
}

func isRetryable(err error) bool {
	// Упрощённая политика: считаем повторяемыми все ошибки, кроме отмены
	// контекста — сетевые обрывы, таймауты, временная недоступность сервера
	// (5xx попадают сюда же через unexpected status, что тоже уместно
	// повторить). При необходимости здесь можно исключить 4xx (кроме 416).
	return err != context.Canceled
}
