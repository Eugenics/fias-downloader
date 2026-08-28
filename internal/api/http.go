// Package api содержит минимальный HTTP-интерфейс ручного управления
// сервисом: внеплановая проверка обновлений и принудительная загрузка
// последней полной версии.
package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"fias-downloader/internal/scheduler"
)

type Server struct {
	log       *slog.Logger
	scheduler *scheduler.Scheduler
	mux       *http.ServeMux
}

func New(log *slog.Logger, sch *scheduler.Scheduler, reg *prometheus.Registry) *Server {
	s := &Server{log: log, scheduler: sch, mux: http.NewServeMux()}
	s.mux.HandleFunc("/healthz", s.handleHealthz)
	s.mux.HandleFunc("/trigger/sync", s.handleTriggerSync)
	s.mux.HandleFunc("/trigger/full", s.handleTriggerFull)
	s.mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	return s
}

func (s *Server) Handler() http.Handler {
	return s.mux
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// handleTriggerSync запускает внеплановый цикл проверки/загрузки версий
// (та же логика full/delta, что и по расписанию). Загрузка выполняется
// асинхронно — эндпоинт сразу отвечает 202 Accepted.
func (s *Server) handleTriggerSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Используем context.Background(), а не r.Context(): загрузка выполняется
	// асинхронно и не должна отменяться при завершении HTTP-запроса.
	go func() {
		if err := s.scheduler.RunCycle(context.Background()); err != nil {
			s.log.Error("manual sync cycle failed", "error", err)
		}
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "started"})
}

// handleTriggerFull принудительно запускает загрузку последней полной
// версии, даже если полная версия уже была загружена ранее (см. ТЗ, п. 4.2.3).
func (s *Server) handleTriggerFull(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	go func() {
		if err := s.scheduler.ForceDownloadLatestFull(context.Background()); err != nil {
			s.log.Error("manual full download failed", "error", err)
		}
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "started"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
