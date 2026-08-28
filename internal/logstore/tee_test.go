package logstore

import (
	"context"
	"log/slog"
	"testing"
)

// recordingHandler — простой slog.Handler для тестов, запоминающий сообщения
// и итоговый набор атрибутов (включая унаследованные через WithAttrs) для
// каждой записи.
type recordingHandler struct {
	messages *[]string
	seenAttr *[]bool // seenAttr[i] == true, если у i-й записи был атрибут request_id
	attrs    []slog.Attr
}

func newRecordingHandler() *recordingHandler {
	return &recordingHandler{messages: &[]string{}, seenAttr: &[]bool{}}
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	*h.messages = append(*h.messages, r.Message)

	has := false
	for _, a := range h.attrs {
		if a.Key == "request_id" {
			has = true
		}
	}
	*h.seenAttr = append(*h.seenAttr, has)
	return nil
}

func (h *recordingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	nh := &recordingHandler{
		messages: h.messages,
		seenAttr: h.seenAttr,
		attrs:    append(append([]slog.Attr{}, h.attrs...), attrs...),
	}
	return nh
}

func (h *recordingHandler) WithGroup(string) slog.Handler { return h }

func TestTeeHandler_FansOutToAllHandlers(t *testing.T) {
	a := newRecordingHandler()
	b := newRecordingHandler()

	logger := slog.New(NewTeeHandler(a, b))
	logger.Info("hello")
	logger.Warn("world")

	for name, h := range map[string]*recordingHandler{"a": a, "b": b} {
		if len(*h.messages) != 2 {
			t.Fatalf("handler %s: expected 2 messages, got %d: %v", name, len(*h.messages), *h.messages)
		}
		if (*h.messages)[0] != "hello" || (*h.messages)[1] != "world" {
			t.Fatalf("handler %s: unexpected messages: %v", name, *h.messages)
		}
	}
}

func TestTeeHandler_WithAttrsPropagatesToAllHandlers(t *testing.T) {
	a := newRecordingHandler()
	b := newRecordingHandler()

	logger := slog.New(NewTeeHandler(a, b)).With("request_id", "abc123")
	logger.Info("did something")

	for name, h := range map[string]*recordingHandler{"a": a, "b": b} {
		if len(*h.seenAttr) != 1 || !(*h.seenAttr)[0] {
			t.Fatalf("handler %s: expected propagated attr request_id on the record, got %v", name, *h.seenAttr)
		}
	}
}
