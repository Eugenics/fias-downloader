// Package config загружает конфигурацию сервиса из YAML-файла.
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration — обёртка над time.Duration с поддержкой человекочитаемых строк
// в YAML (например, "30s", "6h"), т.к. штатный time.Duration умеет
// маршалиться только в наносекунды.
type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalYAML(unmarshal func(any) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}
	if s == "" {
		d.Duration = 0
		return nil
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	d.Duration = parsed
	return nil
}

func (d Duration) MarshalYAML() (any, error) {
	return d.Duration.String(), nil
}

// Config — конфигурация сервиса, полностью описывается YAML-файлом.
// Структура и yaml-теги соответствуют config.example.yaml в корне проекта.
type Config struct {
	Postgres  PostgresConfig  `yaml:"postgres"`
	Source    SourceConfig    `yaml:"source"`
	Download  DownloadConfig  `yaml:"download"`
	HTTP      HTTPConfig      `yaml:"http"`
	Scheduler SchedulerConfig `yaml:"scheduler"`
}

type PostgresConfig struct {
	// DSN — строка подключения к PostgreSQL,
	// напр. "postgres://user:pass@host:5432/dbname?sslmode=disable".
	DSN string `yaml:"dsn"`
}

type SourceConfig struct {
	// URL — адрес перечня версий ФИАС/ГАР.
	URL string `yaml:"url"`
	// HTTPTimeout — таймаут на запрос перечня версий.
	HTTPTimeout Duration `yaml:"http_timeout"`
}

type DownloadConfig struct {
	// Dir — каталог для сохранения (в т.ч. частично) загруженных архивов.
	Dir string `yaml:"dir"`
	// StallTimeout — таймаут неактивности при потоковой загрузке файла
	// (если за это время не пришло ни одного байта — считаем загрузку оборванной).
	StallTimeout Duration `yaml:"stall_timeout"`
	// MaxRetries — количество повторных попыток при временных сетевых ошибках.
	MaxRetries int `yaml:"max_retries"`
	// RetryBaseDelay — базовая задержка для экспоненциального backoff между попытками.
	RetryBaseDelay Duration `yaml:"retry_base_delay"`
	// ProgressSaveInterval — как часто сохранять прогресс докачки в БД.
	ProgressSaveInterval Duration `yaml:"progress_save_interval"`
}

type HTTPConfig struct {
	// ListenAddr — адрес, на котором поднимается HTTP API (ручное
	// управление + /metrics).
	ListenAddr string `yaml:"listen_addr"`
}

type SchedulerConfig struct {
	// PollInterval — периодичность автоматической проверки обновлений.
	PollInterval Duration `yaml:"poll_interval"`
}

func defaults() Config {
	return Config{
		Source: SourceConfig{
			URL:         "https://fias.nalog.ru/WebServices/Public/GetAllDownloadFileInfo",
			HTTPTimeout: Duration{30 * time.Second},
		},
		Download: DownloadConfig{
			Dir:                  "./data/downloads",
			StallTimeout:         Duration{10 * time.Minute},
			MaxRetries:           15,
			RetryBaseDelay:       Duration{5 * time.Second},
			ProgressSaveInterval: Duration{30 * time.Second},
		},
		HTTP: HTTPConfig{
			ListenAddr: ":8080",
		},
		Scheduler: SchedulerConfig{
			PollInterval: Duration{6 * time.Hour},
		},
	}
}

// Load читает и разбирает YAML-файл конфигурации по указанному пути,
// накладывая значения поверх значений по умолчанию (см. defaults()).
func Load(path string) (Config, error) {
	cfg := defaults()

	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config file %q: %w", path, err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config file %q: %w", path, err)
	}

	if err := cfg.validate(); err != nil {
		return Config{}, fmt.Errorf("invalid config %q: %w", path, err)
	}
	return cfg, nil
}

func (c Config) validate() error {
	if c.Postgres.DSN == "" {
		return fmt.Errorf("postgres.dsn is required")
	}
	if c.Source.URL == "" {
		return fmt.Errorf("source.url is required")
	}
	if c.Download.Dir == "" {
		return fmt.Errorf("download.dir is required")
	}
	if c.HTTP.ListenAddr == "" {
		return fmt.Errorf("http.listen_addr is required")
	}
	if c.Scheduler.PollInterval.Duration <= 0 {
		return fmt.Errorf("scheduler.poll_interval must be a positive duration")
	}
	return nil
}
