package downloader

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func requireWget(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("wget"); err != nil {
		t.Skip("wget not found in PATH")
	}
}

// TestDownload_Resume проверяет, что при наличии частично загруженного файла
// повторный вызов Download докачивает недостающий хвост.
func TestDownload_Resume(t *testing.T) {
	requireWget(t)

	content := strings.Repeat("0123456789", 100_000) // 1,000,000 байт
	want := sha256.Sum256([]byte(content))

	var rangeSeen bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rng := r.Header.Get("Range")
		if rng == "" {
			w.Header().Set("Content-Length", strconv.Itoa(len(content)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(content))
			return
		}
		if rng == "bytes=400000-" {
			rangeSeen = true
		}
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

	// Имитируем ранее прерванную загрузку: на диске уже лежит первая часть файла.
	if err := os.WriteFile(dest, []byte(content[:400_000]), 0o644); err != nil {
		t.Fatal(err)
	}

	d := New(nil, Options{MaxRetries: 1, RetryBaseDelay: time.Millisecond, StallTimeout: 5 * time.Second, ProgressEvery: 20 * time.Millisecond})

	var lastDownloaded, lastTotal int64
	var totalSeenDuringDownload bool
	res, err := d.Download(context.Background(), srv.URL, dest, func(downloaded, total int64) {
		lastDownloaded, lastTotal = downloaded, total
		if downloaded < int64(len(content)) && total == int64(len(content)) {
			totalSeenDuringDownload = true
		}
	})
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	if !rangeSeen {
		t.Fatalf("expected to see resume Range request bytes=400000-")
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
	if !totalSeenDuringDownload {
		t.Fatal("expected full file size in progress before download completed")
	}
}

// TestDownload_ServerIgnoresRange проверяет фолбэк на перезагрузку,
// когда сервер не поддерживает Range и всегда отвечает 200 с полным телом.
func TestDownload_ServerIgnoresRange(t *testing.T) {
	requireWget(t)

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

	d := New(nil, Options{MaxRetries: 1, RetryBaseDelay: time.Millisecond, StallTimeout: 5 * time.Second, ProgressEvery: 20 * time.Millisecond})
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

// TestDownload_RetryBudgetResetsOnProgress проверяет, что лимит ретраев
// применяется к подряд идущим ошибкам без прогресса. Если прогресс есть
// (файл на диске растёт), бюджет ошибок должен сбрасываться.
func TestDownload_RetryBudgetResetsOnProgress(t *testing.T) {
	requireWget(t)

	content := strings.Repeat("abcdefghij", 200) // 2000 байт

	var rangeCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rng := r.Header.Get("Range")
		if rng == "" {
			w.Header().Set("Content-Length", strconv.Itoa(len(content)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(content))
			return
		}

		rangeCalls++
		var offset int
		_, err := parseRange(rng, &offset)
		if err != nil {
			t.Fatalf("bad range header %q: %v", rng, err)
		}

		remaining := content[offset:]
		w.Header().Set("Content-Range", "bytes "+strconv.Itoa(offset)+"-"+strconv.Itoa(len(content)-1)+"/"+strconv.Itoa(len(content)))
		w.Header().Set("Content-Length", strconv.Itoa(len(remaining)))
		w.WriteHeader(http.StatusPartialContent)

		if rangeCalls <= 2 {
			chunk := 300
			if chunk > len(remaining) {
				chunk = len(remaining)
			}
			_, _ = w.Write([]byte(remaining[:chunk]))
			return
		}

		_, _ = w.Write([]byte(remaining))
	}))
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "file.bin")
	if err := os.WriteFile(dest, []byte(content[:100]), 0o644); err != nil {
		t.Fatal(err)
	}

	d := New(nil, Options{MaxRetries: 1, RetryBaseDelay: time.Millisecond, StallTimeout: 5 * time.Second, ProgressEvery: 20 * time.Millisecond})
	res, err := d.Download(context.Background(), srv.URL, dest, nil)
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	if rangeCalls < 3 {
		t.Fatalf("expected at least 3 range calls (2 failures + success path), got %d", rangeCalls)
	}
	if res.TotalBytes != int64(len(content)) {
		t.Fatalf("expected %d bytes, got %d", len(content), res.TotalBytes)
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
