package downloader

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestDownload_Resume проверяет, что при наличии частично загруженного файла
// повторный вызов Download докачивает недостающий хвост через Range, а не
// перезагружает файл целиком.
func TestDownload_Resume(t *testing.T) {
	content := strings.Repeat("0123456789", 100_000) // 1,000,000 байт
	want := sha256.Sum256([]byte(content))

	var rangeRequests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rng := r.Header.Get("Range")
		if rng == "" {
			w.Header().Set("Content-Length", strconv.Itoa(len(content)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(content))
			return
		}
		rangeRequests++
		var offset int
		_, err := parseRange(rng, &offset)
		if err != nil {
			t.Fatalf("bad range header %q: %v", rng, err)
		}
		remaining := content[offset:]
		w.Header().Set("Content-Range", "bytes "+strconv.Itoa(offset)+"-"+strconv.Itoa(len(content)-1)+"/"+strconv.Itoa(len(content)))
		w.Header().Set("Content-Length", strconv.Itoa(len(remaining)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte(remaining))
	}))
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "file.bin")

	// Имитируем ранее прерванную загрузку: на диске уже лежит первая половина файла.
	if err := os.WriteFile(dest, []byte(content[:400_000]), 0o644); err != nil {
		t.Fatal(err)
	}

	d := New(srv.Client(), Options{MaxRetries: 1, RetryBaseDelay: time.Millisecond, StallTimeout: 5 * time.Second, ProgressEvery: time.Millisecond})

	var lastDownloaded, lastTotal int64
	res, err := d.Download(context.Background(), srv.URL, dest, func(downloaded, total int64) {
		lastDownloaded, lastTotal = downloaded, total
	})
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}

	if rangeRequests != 1 {
		t.Fatalf("expected exactly 1 range request, got %d", rangeRequests)
	}
	if res.TotalBytes != int64(len(content)) {
		t.Fatalf("expected total bytes %d, got %d", len(content), res.TotalBytes)
	}
	if got := hex.EncodeToString(want[:]); res.SHA256 != got {
		t.Fatalf("checksum mismatch: got %s want %s", res.SHA256, got)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Fatalf("resulting file content mismatch (len got=%d want=%d)", len(got), len(content))
	}
	if lastDownloaded != int64(len(content)) || lastTotal != int64(len(content)) {
		t.Fatalf("unexpected final progress: downloaded=%d total=%d", lastDownloaded, lastTotal)
	}
}

// TestDownload_ServerIgnoresRange проверяет фолбэк на полную перезагрузку,
// когда сервер не поддерживает Range и всегда отвечает 200 с полным телом.
func TestDownload_ServerIgnoresRange(t *testing.T) {
	content := "hello world, no range support here"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, content)
	}))
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "file.bin")
	if err := os.WriteFile(dest, []byte("STALE-PARTIAL-DATA"), 0o644); err != nil {
		t.Fatal(err)
	}

	d := New(srv.Client(), Options{MaxRetries: 1, RetryBaseDelay: time.Millisecond, StallTimeout: 5 * time.Second, ProgressEvery: time.Millisecond})
	res, err := d.Download(context.Background(), srv.URL, dest, nil)
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	if res.TotalBytes != int64(len(content)) {
		t.Fatalf("expected %d bytes, got %d", len(content), res.TotalBytes)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != content {
		t.Fatalf("file was not overwritten correctly, got %q", string(got))
	}
}

// parseRange парсит заголовок вида "bytes=NNN-" и возвращает смещение.
func parseRange(header string, offset *int) (int, error) {
	const prefix = "bytes="
	s := strings.TrimPrefix(header, prefix)
	s = strings.TrimSuffix(s, "-")
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, err
	}
	*offset = n
	return n, nil
}
