// Package downloader выполняет загрузку файлов через wget с поддержкой
// докачки (continue) и ретраями при временных ошибках.
package downloader

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// ProgressFunc вызывается периодически в процессе загрузки с текущим
// количеством загруженных байт и (если известно) общим размером файла.
// totalBytes может быть 0, если общий размер пока неизвестен.
type ProgressFunc func(downloadedBytes, totalBytes int64)

type Options struct {
	MaxRetries     int
	RetryBaseDelay time.Duration
	StallTimeout   time.Duration // максимальная пауза без роста файла
	ProgressEvery  time.Duration // как часто отдавать прогресс
	Logger         *slog.Logger
}

type Downloader struct {
	probeClient *http.Client
	opts        Options
	log         *slog.Logger
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

	probeClient := &http.Client{Timeout: 30 * time.Second}
	if client != nil {
		clone := *client
		if clone.Timeout <= 0 {
			clone.Timeout = 30 * time.Second
		}
		probeClient = &clone
	}

	return &Downloader{probeClient: probeClient, opts: opts, log: opts.Logger}
}

// Result — итог успешной загрузки.
type Result struct {
	TotalBytes int64
	SHA256     string
}

// Download скачивает url в destPath через wget --continue.
// При временных ошибках выполняет повторные попытки с backoff.
func (d *Downloader) Download(ctx context.Context, url, destPath string, onProgress ProgressFunc) (Result, error) {
	var lastErr error
	consecutiveNoProgressFailures := 0

	for {
		if consecutiveNoProgressFailures > d.opts.MaxRetries {
			return Result{}, fmt.Errorf("download failed after %d attempts without progress: %w", d.opts.MaxRetries+1, lastErr)
		}

		if consecutiveNoProgressFailures > 0 {
			delay := backoff(d.opts.RetryBaseDelay, consecutiveNoProgressFailures)
			select {
			case <-ctx.Done():
				return Result{}, ctx.Err()
			case <-time.After(delay):
			}
		}

		beforeSize := fileSize(destPath)
		res, err := d.attempt(ctx, url, destPath, onProgress)
		if err == nil {
			return res, nil
		}
		if !isRetryable(err) {
			return Result{}, err
		}
		lastErr = err

		afterSize := fileSize(destPath)
		if afterSize > beforeSize {
			consecutiveNoProgressFailures = 0
			if d.log != nil {
				d.log.Warn("download attempt failed after progress, retrying", "error", err, "bytes_before", beforeSize, "bytes_after", afterSize)
			}
			continue
		}

		consecutiveNoProgressFailures++
		if d.log != nil {
			d.log.Warn("download attempt failed without progress", "error", err, "bytes", afterSize, "consecutive_no_progress_failures", consecutiveNoProgressFailures)
		}
	}
}

func (d *Downloader) attempt(ctx context.Context, url, destPath string, onProgress ProgressFunc) (Result, error) {
	if err := os.MkdirAll(dirOf(destPath), 0o755); err != nil {
		return Result{}, fmt.Errorf("mkdir: %w", err)
	}

	offset := fileSize(destPath)
	var totalBytes int64
	if offset > 0 {
		canResume, remoteTotal, probeErr := d.canResume(ctx, url, offset)
		totalBytes = remoteTotal
		if probeErr != nil {
			if d.log != nil {
				d.log.Warn("resume probe failed, proceeding with wget --continue", "error", probeErr, "url", url, "offset", offset)
			}
		} else if !canResume {
			if d.log != nil {
				d.log.Warn("source does not support resume for current partial file, restarting download from zero", "url", url, "offset", offset)
			}
			if err := os.Remove(destPath); err != nil && !os.IsNotExist(err) {
				return Result{}, fmt.Errorf("remove stale partial file: %w", err)
			}
		}
	}
	if totalBytes <= 0 {
		totalBytes = d.probeTotal(ctx, url)
	}

	readTimeoutSec := int64(d.opts.StallTimeout.Seconds())
	if readTimeoutSec <= 0 {
		readTimeoutSec = 1
	}

	args := []string{
		"--continue",
		"--output-document=" + destPath,
		"--tries=1",
		"--retry-connrefused",
		"--read-timeout=" + fmt.Sprintf("%d", readTimeoutSec),
		"--timeout=" + fmt.Sprintf("%d", readTimeoutSec),
		"--no-verbose",
		url,
	}

	var output bytes.Buffer
	cmd := exec.CommandContext(ctx, "wget", args...)
	cmd.Stdout = &output
	cmd.Stderr = &output

	if d.log != nil {
		d.log.Info("starting wget", "url", url, "dest", destPath, "args", args)
	}

	errCh := make(chan error, 1)
	if onProgress != nil {
		onProgress(fileSize(destPath), totalBytes)
	}
	go func() {
		errCh <- cmd.Run()
	}()

	ticker := time.NewTicker(d.opts.ProgressEvery)
	defer ticker.Stop()

	lastSize := fileSize(destPath)
	lastGrowthAt := time.Now()

	for {
		select {
		case <-ctx.Done():
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			<-errCh
			return Result{}, ctx.Err()
		case err := <-errCh:
			if err != nil {
				out := strings.TrimSpace(output.String())
				if len(out) > 2000 {
					out = out[:2000] + "..."
				}
				if out != "" {
					return Result{}, fmt.Errorf("wget failed: %w: %s", err, out)
				}
				return Result{}, fmt.Errorf("wget failed: %w", err)
			}

			total, sha, sumErr := fileSHA256(destPath)
			if sumErr != nil {
				return Result{}, fmt.Errorf("checksum file: %w", sumErr)
			}
			if onProgress != nil {
				onProgress(total, total)
			}
			return Result{TotalBytes: total, SHA256: sha}, nil
		case <-ticker.C:
			sz := fileSize(destPath)
			if sz > lastSize {
				lastSize = sz
				lastGrowthAt = time.Now()
			}
			if onProgress != nil {
				onProgress(sz, totalBytes)
			}
			if time.Since(lastGrowthAt) >= d.opts.StallTimeout {
				if cmd.Process != nil {
					_ = cmd.Process.Kill()
				}
				<-errCh
				return Result{}, fmt.Errorf("stalled: no data for %s", d.opts.StallTimeout)
			}
		}
	}
}

func (d *Downloader) canResume(ctx context.Context, url string, offset int64) (bool, int64, error) {
	probeCtx, cancel := context.WithTimeout(ctx, d.probeClient.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, url, nil)
	if err != nil {
		return false, 0, fmt.Errorf("build resume probe request: %w", err)
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))

	resp, err := d.probeClient.Do(req)
	if err != nil {
		return false, 0, fmt.Errorf("execute resume probe request: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.CopyN(io.Discard, resp.Body, 1)

	total := totalFromResponse(resp)
	switch resp.StatusCode {
	case http.StatusPartialContent:
		return true, total, nil
	case http.StatusOK, http.StatusRequestedRangeNotSatisfiable:
		return false, total, nil
	default:
		return false, total, fmt.Errorf("unexpected resume probe status: %d", resp.StatusCode)
	}
}

// probeTotal получает полный размер объекта однобайтовым Range-запросом.
func (d *Downloader) probeTotal(ctx context.Context, url string) int64 {
	probeCtx, cancel := context.WithTimeout(ctx, d.probeClient.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, url, nil)
	if err != nil {
		return 0
	}
	req.Header.Set("Range", "bytes=0-0")
	resp, err := d.probeClient.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	_, _ = io.CopyN(io.Discard, resp.Body, 1)
	return totalFromResponse(resp)
}

func totalFromResponse(resp *http.Response) int64 {
	if contentRange := resp.Header.Get("Content-Range"); contentRange != "" {
		if slash := strings.LastIndexByte(contentRange, '/'); slash >= 0 {
			total, err := strconv.ParseInt(contentRange[slash+1:], 10, 64)
			if err == nil && total >= 0 {
				return total
			}
		}
	}
	if resp.ContentLength >= 0 {
		return resp.ContentLength
	}
	return 0
}

func fileSHA256(path string) (int64, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()

	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return 0, "", err
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

func dirOf(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[:i]
		}
	}
	return "."
}

func fileSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

func backoff(base time.Duration, attempt int) time.Duration {
	d := time.Duration(float64(base) * math.Pow(2, float64(attempt-1)))
	jitter := time.Duration(rand.Int63n(int64(base)))
	return d + jitter
}

func isRetryable(err error) bool {
	return !(errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded))
}
