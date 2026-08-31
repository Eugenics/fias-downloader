// Package downloader downloads files over HTTP with resume support and retries.
package downloader

import (
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
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type ProgressFunc func(downloadedBytes, totalBytes int64)

type Options struct {
	MaxRetries     int
	RetryBaseDelay time.Duration
	StallTimeout   time.Duration
	ProgressEvery  time.Duration
	ConsoleEvery   time.Duration
	ProgressOutput io.Writer
	ConsoleLines   bool
	Logger         *slog.Logger
}

type Downloader struct {
	client *http.Client
	opts   Options
	log    *slog.Logger
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
	if opts.ConsoleEvery <= 0 {
		opts.ConsoleEvery = 5 * time.Second
	}
	if client == nil {
		client = &http.Client{}
	}
	return &Downloader{client: client, opts: opts, log: opts.Logger}
}

type Result struct {
	TotalBytes int64
	SHA256     string
}

// Download stores incomplete data in destPath.part and atomically renames it
// after the complete response has been received and synced.
func (d *Downloader) Download(ctx context.Context, url, destPath string, onProgress ProgressFunc) (Result, error) {
	partPath := destPath + ".part"
	var lastErr error
	noProgressFailures := 0
	for {
		if noProgressFailures > d.opts.MaxRetries {
			return Result{}, fmt.Errorf("download failed after %d attempts without progress: %w", d.opts.MaxRetries+1, lastErr)
		}
		if noProgressFailures > 0 {
			select {
			case <-ctx.Done():
				return Result{}, ctx.Err()
			case <-time.After(backoff(d.opts.RetryBaseDelay, noProgressFailures)):
			}
		}

		beforeSize := fileSize(partPath)
		res, err := d.attempt(ctx, url, destPath, onProgress)
		if err == nil {
			return res, nil
		}
		if !isRetryable(err) {
			return Result{}, err
		}
		lastErr = err
		afterSize := fileSize(partPath)
		if afterSize > beforeSize {
			noProgressFailures = 0
			if d.log != nil {
				d.log.Warn("download attempt failed after progress, retrying", "error", err, "bytes_before", beforeSize, "bytes_after", afterSize)
			}
			continue
		}
		noProgressFailures++
		if d.log != nil {
			d.log.Warn("download attempt failed without progress", "error", err, "bytes", afterSize, "consecutive_no_progress_failures", noProgressFailures)
		}
	}
}

func (d *Downloader) attempt(ctx context.Context, url, destPath string, onProgress ProgressFunc) (Result, error) {
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return Result{}, fmt.Errorf("create download directory: %w", err)
	}
	partPath := destPath + ".part"
	f, err := os.OpenFile(partPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return Result{}, fmt.Errorf("open partial file: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return Result{}, fmt.Errorf("stat partial file: %w", err)
	}
	offset := info.Size()

	attemptCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(attemptCtx, http.MethodGet, url, nil)
	if err != nil {
		return Result{}, fmt.Errorf("create download request: %w", err)
	}
	req.Header.Set("Accept-Encoding", "identity")
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	if d.log != nil {
		d.log.Info("starting HTTP download", "url", url, "dest", destPath, "offset", offset)
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("execute download request: %w", err)
	}
	defer resp.Body.Close()

	expectedTotal := int64(0)
	switch resp.StatusCode {
	case http.StatusPartialContent:
		start, total, parseErr := parseContentRange(resp.Header.Get("Content-Range"))
		if parseErr != nil || start != offset {
			return Result{}, fmt.Errorf("invalid Content-Range %q for offset %d", resp.Header.Get("Content-Range"), offset)
		}
		expectedTotal = total
	case http.StatusOK:
		if offset > 0 {
			if d.log != nil {
				d.log.Warn("source ignored Range request, restarting download from zero", "url", url, "offset", offset)
			}
			if d.opts.ProgressOutput != nil {
				fmt.Fprintln(d.opts.ProgressOutput, "сервер не поддержал докачку; начинаю заново")
			}
		}
		offset = 0
		if err := f.Truncate(0); err != nil {
			return Result{}, fmt.Errorf("truncate partial file: %w", err)
		}
		expectedTotal = resp.ContentLength
		if expectedTotal < 0 {
			expectedTotal = 0
		}
	case http.StatusRequestedRangeNotSatisfiable:
		total, ok := rangeTotal(resp.Header.Get("Content-Range"))
		if ok && total == offset {
			return finishDownload(f, partPath, destPath, total, onProgress, d.opts.ProgressOutput)
		}
		return Result{}, fmt.Errorf("server rejected download range: %s", resp.Status)
	default:
		return Result{}, fmt.Errorf("server returned %s", resp.Status)
	}

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return Result{}, fmt.Errorf("seek partial file: %w", err)
	}
	if onProgress != nil {
		onProgress(offset, expectedTotal)
	}
	if d.opts.ProgressOutput != nil {
		fmt.Fprintf(d.opts.ProgressOutput, "загрузка с позиции %s", formatBytes(offset))
		if expectedTotal > 0 {
			fmt.Fprintf(d.opts.ProgressOutput, " из %s", formatBytes(expectedTotal))
		}
		fmt.Fprintln(d.opts.ProgressOutput)
	}
	w := &progressWriter{
		dst:           f,
		output:        d.opts.ProgressOutput,
		downloaded:    offset,
		total:         expectedTotal,
		callbackEvery: d.opts.ProgressEvery,
		outputEvery:   d.opts.ConsoleEvery,
		outputLines:   d.opts.ConsoleLines,
		onProgress:    onProgress,
		lastCallback:  time.Now(),
		lastOutput:    time.Now(),
		activity:      make(chan struct{}, 1),
	}
	copyErr := make(chan error, 1)
	go func() {
		_, copyError := io.Copy(w, resp.Body)
		copyErr <- copyError
	}()

	stallTimer := time.NewTimer(d.opts.StallTimeout)
	defer stallTimer.Stop()
	for {
		select {
		case <-ctx.Done():
			cancel()
			<-copyErr
			return Result{}, ctx.Err()
		case err := <-copyErr:
			if err != nil {
				return Result{}, fmt.Errorf("download data: %w", err)
			}
			downloaded := w.Downloaded()
			if expectedTotal > 0 && downloaded != expectedTotal {
				return Result{}, fmt.Errorf("download interrupted: received %d of %d bytes", downloaded, expectedTotal)
			}
			return finishDownload(f, partPath, destPath, downloaded, onProgress, d.opts.ProgressOutput)
		case <-w.activity:
			if !stallTimer.Stop() {
				select {
				case <-stallTimer.C:
				default:
				}
			}
			stallTimer.Reset(d.opts.StallTimeout)
		case <-stallTimer.C:
			cancel()
			<-copyErr
			return Result{}, fmt.Errorf("stalled: no data for %s", d.opts.StallTimeout)
		}
	}
}

type progressWriter struct {
	dst           io.Writer
	output        io.Writer
	outputLines   bool
	downloaded    int64
	total         int64
	callbackEvery time.Duration
	outputEvery   time.Duration
	onProgress    ProgressFunc
	lastCallback  time.Time
	lastOutput    time.Time
	activity      chan struct{}
	mu            sync.Mutex
}

func (w *progressWriter) Write(p []byte) (int, error) {
	n, err := w.dst.Write(p)
	w.mu.Lock()
	w.downloaded += int64(n)
	downloaded := w.downloaded
	now := time.Now()
	reportCallback := w.onProgress != nil && now.Sub(w.lastCallback) >= w.callbackEvery
	reportOutput := w.output != nil && now.Sub(w.lastOutput) >= w.outputEvery
	if reportCallback {
		w.lastCallback = now
	}
	if reportOutput {
		w.lastOutput = now
	}
	w.mu.Unlock()
	if n > 0 {
		select {
		case w.activity <- struct{}{}:
		default:
		}
	}
	if reportCallback {
		w.onProgress(downloaded, w.total)
	}
	if reportOutput {
		writeConsoleProgress(w.output, downloaded, w.total, w.outputLines)
	}
	return n, err
}

func (w *progressWriter) Downloaded() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.downloaded
}

func finishDownload(f *os.File, partPath, destPath string, total int64, onProgress ProgressFunc, output io.Writer) (Result, error) {
	if err := f.Sync(); err != nil {
		return Result{}, fmt.Errorf("sync downloaded file: %w", err)
	}
	_, sha, err := fileSHA256(partPath)
	if err != nil {
		return Result{}, fmt.Errorf("checksum downloaded file: %w", err)
	}
	if err := f.Close(); err != nil {
		return Result{}, fmt.Errorf("close downloaded file: %w", err)
	}
	if err := os.Rename(partPath, destPath); err != nil {
		return Result{}, fmt.Errorf("rename downloaded file: %w", err)
	}
	if onProgress != nil {
		onProgress(total, total)
	}
	if output != nil {
		fmt.Fprintf(output, "\rзагружено %s\n", formatBytes(total))
		fmt.Fprintln(output, "готово:", destPath)
	}
	return Result{TotalBytes: total, SHA256: sha}, nil
}

func writeConsoleProgress(output io.Writer, downloaded, total int64, lines bool) {
	if output == nil {
		return
	}
	prefix, suffix := "\r", ""
	if lines {
		prefix, suffix = "", "\n"
	}
	if total > 0 {
		fmt.Fprintf(output, "%sзагружено %s / %s (%.1f%%)%s", prefix, formatBytes(downloaded), formatBytes(total), 100*float64(downloaded)/float64(total), suffix)
		return
	}
	fmt.Fprintf(output, "%sзагружено %s%s", prefix, formatBytes(downloaded), suffix)
}

func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for q := n / unit; q >= unit && exp < 5; q /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func parseContentRange(value string) (start, total int64, err error) {
	if !strings.HasPrefix(value, "bytes ") {
		return 0, 0, errors.New("missing bytes prefix")
	}
	parts := strings.Split(strings.TrimPrefix(value, "bytes "), "/")
	if len(parts) != 2 {
		return 0, 0, errors.New("invalid content range")
	}
	bounds := strings.Split(parts[0], "-")
	if len(bounds) != 2 {
		return 0, 0, errors.New("invalid content range bounds")
	}
	start, err = strconv.ParseInt(bounds[0], 10, 64)
	if err != nil {
		return 0, 0, err
	}
	total, err = strconv.ParseInt(parts[1], 10, 64)
	if err != nil || total < 0 {
		return 0, 0, errors.New("invalid content range total")
	}
	return start, total, nil
}

func rangeTotal(value string) (int64, bool) {
	if !strings.HasPrefix(value, "bytes */") {
		return 0, false
	}
	total, err := strconv.ParseInt(strings.TrimPrefix(value, "bytes */"), 10, 64)
	return total, err == nil && total >= 0
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

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func backoff(base time.Duration, attempt int) time.Duration {
	delay := time.Duration(float64(base) * math.Pow(2, float64(attempt-1)))
	return delay + time.Duration(rand.Int63n(int64(base)))
}

func isRetryable(err error) bool {
	return !(errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded))
}
