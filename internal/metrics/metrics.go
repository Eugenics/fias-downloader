// Package metrics exposes Prometheus metrics for the currently active download.
package metrics

import (
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
)

type Metrics struct {
	downloadsInProgress             *prometheus.GaugeVec
	downloadProgressDownloadedBytes *prometheus.GaugeVec
	downloadProgressTotalBytes      *prometheus.GaugeVec
	downloadCompletedBytes          *prometheus.GaugeVec
}

// New registers metrics describing current and completed downloads.
func New(reg *prometheus.Registry) *Metrics {
	m := &Metrics{
		downloadsInProgress: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "fias", Subsystem: "download", Name: "in_progress",
			Help: "Количество загрузок, выполняющихся в данный момент, по типу файла.",
		}, []string{"kind"}),
		downloadProgressDownloadedBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "fias", Subsystem: "download", Name: "progress_downloaded_bytes",
			Help: "Количество уже загруженных байт текущего файла.",
		}, []string{"kind", "version_id"}),
		downloadProgressTotalBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "fias", Subsystem: "download", Name: "progress_total_bytes",
			Help: "Ожидаемый размер текущего файла; 0, если размер неизвестен.",
		}, []string{"kind", "version_id"}),
		downloadCompletedBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "fias", Subsystem: "download", Name: "completed_bytes",
			Help: "Итоговый размер успешно завершённой загрузки.",
		}, []string{"kind", "version_id"}),
	}

	reg.MustRegister(
		m.downloadsInProgress,
		m.downloadProgressDownloadedBytes,
		m.downloadProgressTotalBytes,
		m.downloadCompletedBytes,
	)
	return m
}

// RecordCompletedDownload keeps the final size for the lifetime of the process.
func (m *Metrics) RecordCompletedDownload(kind string, versionID, total int64) {
	m.downloadCompletedBytes.WithLabelValues(kind, strconv.FormatInt(versionID, 10)).Set(float64(total))
}

// DownloadStarted marks a download as active and returns its cleanup function.
func (m *Metrics) DownloadStarted(kind string) func() {
	m.downloadsInProgress.WithLabelValues(kind).Inc()
	return func() {
		m.downloadsInProgress.WithLabelValues(kind).Dec()
	}
}

// SetDownloadProgress updates the byte counters for an active download.
func (m *Metrics) SetDownloadProgress(kind string, versionID int64, downloaded, total int64) {
	vid := strconv.FormatInt(versionID, 10)
	m.downloadProgressDownloadedBytes.WithLabelValues(kind, vid).Set(float64(downloaded))
	m.downloadProgressTotalBytes.WithLabelValues(kind, vid).Set(float64(total))
}

// ClearDownloadProgress removes the completed or failed download from metrics.
func (m *Metrics) ClearDownloadProgress(kind string, versionID int64) {
	vid := strconv.FormatInt(versionID, 10)
	m.downloadProgressDownloadedBytes.DeleteLabelValues(kind, vid)
	m.downloadProgressTotalBytes.DeleteLabelValues(kind, vid)
}
