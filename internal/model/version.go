package model

import "time"

// Kind различает, какой из двух файлов версии загружается: полный снимок
// или дельта относительно предыдущей версии. Это НЕ тип записи в перечне
// источника — каждая версия из GetAllDownloadFileInfo содержит оба URL
// одновременно, а Kind определяет, какой из них был скачан в рамках
// конкретной записи в таблице version_info.
type Kind string

const (
	KindFull  Kind = "full"
	KindDelta Kind = "delta"
)

// Status — статус загрузки конкретной пары (VersionID, Kind).
type Status string

const (
	StatusPending     Status = "pending"
	StatusDownloading Status = "downloading"
	StatusCompleted   Status = "completed"
	StatusFailed      Status = "failed"
)

// SourceVersion — одна запись из ответа
// https://fias.nalog.ru/WebServices/Public/GetAllDownloadFileInfo.
//
// Поля FiasComplete*/FiasDelta*/Kladr* — легаси-поля старого формата,
// в наблюдаемом ответе сервиса всегда пустые; сознательно не маппятся,
// т.к. не используются бизнес-логикой.
type SourceVersion struct {
	VersionID      int64  `json:"VersionId"`
	TextVersion    string `json:"TextVersion"`
	GarXMLFullURL  string `json:"GarXMLFullURL"`
	GarXMLDeltaURL string `json:"GarXMLDeltaURL"`
	ExpDate        string `json:"ExpDate"` // справочно, в бизнес-логике не используется
	Date           string `json:"Date"`    // dd.mm.yyyy
}

// URLFor возвращает ссылку на файл нужного вида для данной версии.
// Пустая строка означает, что источник не предоставил файл этого вида.
func (v SourceVersion) URLFor(k Kind) string {
	switch k {
	case KindFull:
		return v.GarXMLFullURL
	case KindDelta:
		return v.GarXMLDeltaURL
	default:
		return ""
	}
}

// ParsedDate возвращает дату версии, распарсенную из поля Date (dd.mm.yyyy).
func (v SourceVersion) ParsedDate() (time.Time, error) {
	return time.Parse("02.01.2006", v.Date)
}

// DownloadRecord — строка таблицы version_info: состояние загрузки одной
// пары (VersionID, Kind).
type DownloadRecord struct {
	ID              int64
	VersionID       int64
	VersionDate     time.Time
	TextVersion     string
	Kind            Kind
	SourceURL       string
	Status          Status
	FilePath        string
	TotalBytes      int64
	DownloadedBytes int64
	Checksum        string
	IsManual        bool
	StartedAt       *time.Time
	CompletedAt     *time.Time
	LastError       string
}
