// Package logstore реализует slog.Handler, который асинхронно и батчами
// сохраняет записи логов в таблицу logs в PostgreSQL — в дополнение к
// обычному выводу в stdout (см. TeeHandler).
package logstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

// EnsureSchema создаёт таблицу logs, если она ещё не существует.
func EnsureSchema(ctx context.Context, db *sql.DB) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS logs (
    id         BIGSERIAL PRIMARY KEY,
    ts         TIMESTAMPTZ NOT NULL,
    level      TEXT        NOT NULL,
    message    TEXT        NOT NULL,
    attrs      JSONB       NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_logs_ts ON logs (ts);
CREATE INDEX IF NOT EXISTS idx_logs_level ON logs (level);
`
	_, err := db.ExecContext(ctx, ddl)
	if err != nil {
		return fmt.Errorf("ensure logs schema: %w", err)
	}
	return nil
}

type entry struct {
	ts      time.Time
	level   string
	message string
	attrs   map[string]any
}

// Options — настройки батчинга записи логов в БД.
type Options struct {
	// QueueSize — размер буфера в памяти. При переполнении новые записи
	// отбрасываются (лучше потерять часть отладочных логов, чем заблокировать
	// основную работу сервиса из-за медленной БД).
	QueueSize int
	// FlushInterval — периодичность записи накопленных записей в БД.
	FlushInterval time.Duration
	// FlushBatchSize — при достижении этого размера буфер сбрасывается
	// немедленно, не дожидаясь FlushInterval.
	FlushBatchSize int
	// MinLevel — минимальный уровень записей, которые попадают в БД
	// (в stdout, через TeeHandler, может выводиться более подробный уровень).
	MinLevel slog.Level
}

func defaultOptions() Options {
	return Options{
		QueueSize:      2000,
		FlushInterval:  2 * time.Second,
		FlushBatchSize: 200,
		MinLevel:       slog.LevelInfo,
	}
}

// core — общее для всех производных хендлеров (после WithAttrs/WithGroup)
// состояние: соединение с БД, очередь и фоновая горутина записи. core
// никогда не копируется — только используется через указатель, поэтому
// содержащийся в нём sync.WaitGroup безопасен.
type core struct {
	db   *sql.DB
	opts Options

	queue chan entry
	wg    sync.WaitGroup
	stop  chan struct{}
}

// DBHandler — slog.Handler, складывающий записи в очередь и асинхронно
// сохраняющий их в БД. Ошибки записи в БД не приводят к панике и не
// блокируют приложение — они выводятся в stderr напрямую (не через slog,
// чтобы не зациклиться).
//
// DBHandler сам по себе — лёгкое значение (указатель на общее ядро +
// собственные attrs/groups), поэтому WithAttrs/WithGroup могут безопасно
// возвращать его копию с изменённым набором атрибутов, не затрагивая
// общую очередь и фоновую горутину.
type DBHandler struct {
	c      *core
	groups []string
	attrs  []slog.Attr
}

func NewDBHandler(db *sql.DB, opts Options) *DBHandler {
	def := defaultOptions()
	if opts.QueueSize <= 0 {
		opts.QueueSize = def.QueueSize
	}
	if opts.FlushInterval <= 0 {
		opts.FlushInterval = def.FlushInterval
	}
	if opts.FlushBatchSize <= 0 {
		opts.FlushBatchSize = def.FlushBatchSize
	}
	if opts.MinLevel == 0 {
		opts.MinLevel = def.MinLevel
	}

	c := &core{
		db:    db,
		opts:  opts,
		queue: make(chan entry, opts.QueueSize),
		stop:  make(chan struct{}),
	}
	c.wg.Add(1)
	go c.run()
	return &DBHandler{c: c}
}

func (h *DBHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.c.opts.MinLevel
}

func (h *DBHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := make(map[string]any, r.NumAttrs()+len(h.attrs))
	for _, a := range h.attrs {
		attrs[a.Key] = a.Value.Any()
	}
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.Any()
		return true
	})

	e := entry{ts: r.Time, level: r.Level.String(), message: r.Message, attrs: attrs}

	select {
	case h.c.queue <- e:
	default:
		// Очередь переполнена — жертвуем этой записью лога, чтобы не
		// блокировать вызывающий код. Само событие переполнения не
		// логируем через slog во избежание рекурсии/шторма.
	}
	return nil
}

func (h *DBHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &DBHandler{
		c:      h.c,
		groups: h.groups,
		attrs:  append(append([]slog.Attr{}, h.attrs...), attrs...),
	}
}

func (h *DBHandler) WithGroup(name string) slog.Handler {
	return &DBHandler{
		c:      h.c,
		groups: append(append([]string{}, h.groups...), name),
		attrs:  h.attrs,
	}
}

// Close останавливает фоновую запись, сбрасывая всё, что осталось в очереди
// (в пределах переданного контекста/таймаута).
func (h *DBHandler) Close(ctx context.Context) error {
	close(h.c.stop)
	done := make(chan struct{})
	go func() {
		h.c.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *core) run() {
	defer c.wg.Done()
	ticker := time.NewTicker(c.opts.FlushInterval)
	defer ticker.Stop()

	batch := make([]entry, 0, c.opts.FlushBatchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := c.insertBatch(batch); err != nil {
			fmt.Fprintf(os.Stderr, "logstore: failed to flush %d log entries: %v\n", len(batch), err)
		}
		batch = batch[:0]
	}

	for {
		select {
		case e := <-c.queue:
			batch = append(batch, e)
			if len(batch) >= c.opts.FlushBatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-c.stop:
			// Дочитываем всё, что успело накопиться в канале, затем финальный flush.
			for {
				select {
				case e := <-c.queue:
					batch = append(batch, e)
				default:
					flush()
					return
				}
			}
		}
	}
}

func (c *core) insertBatch(batch []entry) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `INSERT INTO logs (ts, level, message, attrs) VALUES ($1, $2, $3, $4)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, e := range batch {
		attrsJSON, err := json.Marshal(e.attrs)
		if err != nil {
			attrsJSON = []byte("{}")
		}
		if _, err := stmt.ExecContext(ctx, e.ts, e.level, e.message, attrsJSON); err != nil {
			return err
		}
	}

	return tx.Commit()
}
