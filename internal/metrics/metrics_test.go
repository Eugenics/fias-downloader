package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestMetricsExposeOnlyCurrentDownload(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New(reg)

	done := m.DownloadStarted("full")
	m.SetDownloadProgress("full", 123, 40, 100)

	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
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
	if len(families) != 1 {
		t.Fatalf("got %d metric families after cleanup, want 1", len(families))
	}
	if families[0].GetName() != "fias_download_in_progress" {
		t.Fatalf("unexpected metric after cleanup: %q", families[0].GetName())
	}
}
