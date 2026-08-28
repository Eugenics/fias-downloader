// Package fetcher получает и разбирает перечень версий справочника ФИАС/ГАР
// с публичного сервиса ФНС (GetAllDownloadFileInfo).
package fetcher

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"fias-downloader/internal/model"
)

type Fetcher struct {
	url    string
	client *http.Client
}

func New(url string, timeout time.Duration) *Fetcher {
	return &Fetcher{
		url: url,
		client: &http.Client{
			Timeout: timeout,
		},
	}
}

// Fetch возвращает перечень версий, отсортированный по VersionID по возрастанию.
// Порядок ответа источника не гарантирован, поэтому сортировка выполняется
// явно и не должна зависеть от порядка в JSON.
func (f *Fetcher) Fetch(ctx context.Context) ([]model.SourceVersion, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request %s: %w", f.url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("unexpected status %d from %s: %s", resp.StatusCode, f.url, string(body))
	}

	var versions []model.SourceVersion
	if err := json.NewDecoder(resp.Body).Decode(&versions); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if len(versions) == 0 {
		return nil, fmt.Errorf("source returned empty version list")
	}

	sort.Slice(versions, func(i, j int) bool {
		return versions[i].VersionID < versions[j].VersionID
	})

	return versions, nil
}
