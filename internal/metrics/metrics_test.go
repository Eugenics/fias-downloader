package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestMetricsExposeDownloadProgressAndCompletedFiles(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New(reg)

	done := m.DownloadStarted("full")
	m.SetDownloadProgress("full", 123, 40, 100)
	m.RecordCompletedDownload("delta", 124, 75)

	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"fias_download_completed_bytes":           true,
		"fias_download_in_progress":               true,
		"fias_download_progress_downloaded_bytes": true,
		"fias_download_progress_total_bytes":      true,
	}
	if len(families) != len(want) {
		t.Fatalf("got %d metric families, want %d", len(families), len(want))
	}
	for _, family := range families {
		if !want[family.GetName()] {
			t.Errorf("unexpected metric family %q", family.GetName())
		}
	}

	m.ClearDownloadProgress("full", 123)
	done()
	families, err = reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	if len(families) != 2 {
		t.Fatalf("got %d metric families after cleanup, want 2", len(families))
	}
	got := map[string]bool{}
	for _, family := range families {
		got[family.GetName()] = true
	}
	if !got["fias_download_in_progress"] || !got["fias_download_completed_bytes"] {
		t.Fatalf("unexpected metrics after cleanup: %v", got)
	}
}
