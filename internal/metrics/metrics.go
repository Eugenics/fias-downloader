// Package metrics содержит все Prometheus-метрики сервиса и вспомогательные
// методы для их обновления, чтобы остальной код не работал с prometheus API
// напрямую.
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type Metrics struct {
	reg *prometheus.Registry

	fetchTotal    *prometheus.CounterVec
	fetchDuration prometheus.Histogram

	cycleTotal    *prometheus.CounterVec
	cycleDuration prometheus.Histogram

	downloadsTotal      *prometheus.CounterVec
	downloadBytesTotal  *prometheus.CounterVec
	downloadDuration    *prometheus.HistogramVec
	downloadsInProgress *prometheus.GaugeVec

	lastVersion        *prometheus.GaugeVec
	lastCycleTimestamp prometheus.Gauge
}

// New создаёт и регистрирует все метрики в переданном реестре.
func New(reg *prometheus.Registry) *Metrics {
	const ns = "fias"

	m := &Metrics{
		reg: reg,

		fetchTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Subsystem: "source", Name: "fetch_total",
			Help: "Количество запросов перечня версий к источнику, по результату.",
		}, []string{"result"}),

		fetchDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: ns, Subsystem: "source", Name: "fetch_duration_seconds",
			Help:    "Длительность запроса перечня версий к источнику.",
			Buckets: prometheus.DefBuckets,
		}),

		cycleTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Name: "cycle_total",
			Help: "Количество завершённых циклов проверки/загрузки версий, по результату.",
		}, []string{"result"}),

		cycleDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: ns, Name: "cycle_duration_seconds",
			Help:    "Длительность цикла проверки/загрузки версий.",
			Buckets: []float64{1, 5, 15, 30, 60, 120, 300, 600, 1800, 3600},
		}),

		downloadsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Subsystem: "download", Name: "total",
			Help: "Количество загрузок файлов версий, по типу (full/delta) и результату.",
		}, []string{"kind", "result"}),

		downloadBytesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Subsystem: "download", Name: "bytes_total",
			Help: "Суммарное количество загруженных байт, по типу файла.",
		}, []string{"kind"}),

		downloadDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: ns, Subsystem: "download", Name: "duration_seconds",
			Help:    "Длительность загрузки одного файла версии.",
			Buckets: []float64{1, 5, 15, 30, 60, 300, 600, 1800, 3600, 7200},
		}, []string{"kind"}),

		downloadsInProgress: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: ns, Subsystem: "download", Name: "in_progress",
			Help: "Количество загрузок файлов, выполняющихся в данный момент, по типу.",
		}, []string{"kind"}),

		lastVersion: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: ns, Name: "last_completed_version_id",
			Help: "VersionId последней успешно загруженной версии, по типу файла (full/delta).",
		}, []string{"kind"}),

		lastCycleTimestamp: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: ns, Name: "last_cycle_timestamp_seconds",
			Help: "Unix-время завершения последнего цикла проверки/загрузки версий.",
		}),
	}

	reg.MustRegister(
		m.fetchTotal, m.fetchDuration,
		m.cycleTotal, m.cycleDuration,
		m.downloadsTotal, m.downloadBytesTotal, m.downloadDuration, m.downloadsInProgress,
		m.lastVersion, m.lastCycleTimestamp,
		prometheus.NewGoCollector(),
		prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}),
	)

	return m
}

func (m *Metrics) ObserveFetch(d time.Duration, err error) {
	m.fetchDuration.Observe(d.Seconds())
	if err != nil {
		m.fetchTotal.WithLabelValues("failure").Inc()
		return
	}
	m.fetchTotal.WithLabelValues("success").Inc()
}

func (m *Metrics) ObserveCycle(d time.Duration, err error) {
	m.cycleDuration.Observe(d.Seconds())
	m.lastCycleTimestamp.Set(float64(time.Now().Unix()))
	if err != nil {
		m.cycleTotal.WithLabelValues("failure").Inc()
		return
	}
	m.cycleTotal.WithLabelValues("success").Inc()
}

// DownloadStarted должен вызываться перед началом загрузки файла; возвращённая
// функция — перед выходом из области видимости (снижает gauge in_progress).
func (m *Metrics) DownloadStarted(kind string) func() {
	m.downloadsInProgress.WithLabelValues(kind).Inc()
	return func() {
		m.downloadsInProgress.WithLabelValues(kind).Dec()
	}
}

func (m *Metrics) ObserveDownload(kind string, d time.Duration, bytes int64, err error) {
	m.downloadDuration.WithLabelValues(kind).Observe(d.Seconds())
	if err != nil {
		m.downloadsTotal.WithLabelValues(kind, "failure").Inc()
		return
	}
	m.downloadsTotal.WithLabelValues(kind, "success").Inc()
	m.downloadBytesTotal.WithLabelValues(kind).Add(float64(bytes))
}

func (m *Metrics) SetLastCompletedVersion(kind string, versionID int64) {
	m.lastVersion.WithLabelValues(kind).Set(float64(versionID))
}
