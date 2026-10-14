package events

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"fias-downloader/internal/repo"
)

// Run delivers pending events from PostgreSQL. A crash after broker confirmation
// but before MarkEventPublished can cause a duplicate; consumers should dedupe.
func Run(ctx context.Context, log *slog.Logger, records *repo.Repo, url, queue string) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		if err := drain(ctx, records, url, queue); err != nil && !errors.Is(err, context.Canceled) {
			log.Warn("rabbitmq event delivery failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func drain(ctx context.Context, records *repo.Repo, url, queue string) error {
	event, err := records.NextDownloadEvent(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	conn, err := amqp.Dial(url)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close()
	ch, err := conn.Channel()
	if err != nil {
		return err
	}
	defer ch.Close()
	if _, err := ch.QueueDeclare(queue, true, false, false, false, nil); err != nil {
		return err
	}
	if err := ch.Confirm(false); err != nil {
		return err
	}
	confirms := ch.NotifyPublish(make(chan amqp.Confirmation, 1))
	for {
		if err := ch.PublishWithContext(ctx, "", queue, false, false, amqp.Publishing{
			ContentType: "application/json", DeliveryMode: amqp.Persistent,
			MessageId: fmt.Sprintf("download-event-%d", event.ID), Body: event.Payload,
		}); err != nil {
			return err
		}
		select {
		case confirmation, ok := <-confirms:
			if !ok || !confirmation.Ack {
				return fmt.Errorf("broker did not confirm event %d", event.ID)
			}
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
			return fmt.Errorf("broker confirmation timeout for event %d", event.ID)
		}
		if err := records.MarkEventPublished(ctx, event.ID); err != nil {
			return err
		}
		event, err = records.NextDownloadEvent(ctx)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
