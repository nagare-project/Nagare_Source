// Package sourceruntime executes normalized Source Spec v1 documents and
// produces Candidate v1 values for the Plugin API coordinator.
package sourceruntime

import (
	"context"
	"fmt"
	"time"
)

type ResolveRequest struct {
	Schema      string      `json:"schema"`
	Subject     Subject     `json:"subject"`
	Episode     Episode     `json:"episode"`
	Preferences Preferences `json:"preferences,omitempty"`
}

type Subject struct {
	IDs    map[string]string `json:"ids"`
	Titles []string          `json:"titles"`
	Season *int              `json:"season,omitempty"`
	Year   *int              `json:"year,omitempty"`
}

type Episode struct {
	ID       string   `json:"id,omitempty"`
	Number   string   `json:"number"`
	Absolute *float64 `json:"absolute,omitempty"`
	AirDate  string   `json:"airDate,omitempty"`
	Title    string   `json:"title,omitempty"`
}

type Preferences struct {
	SubtitleLanguages   []string `json:"subtitleLanguages,omitempty"`
	MaxResolution       string   `json:"maxResolution,omitempty"`
	PreferredTransports []string `json:"preferredTransports,omitempty"`
	// Transports 是允许列表：非空时只运行能产出这些 transport 的来源。
	// 客户端拿它做「只要 BT」这类快路径，跳过整队浏览器嗅探。
	Transports []string `json:"transports,omitempty"`
}

type Candidate struct {
	Schema          string    `json:"schema"`
	ID              string    `json:"id"`
	SourceID        string    `json:"sourceId"`
	Tier            int       `json:"tier"`
	MatchConfidence float64   `json:"matchConfidence"`
	Match           Match     `json:"match"`
	Transport       Transport `json:"transport"`
	Metadata        Metadata  `json:"metadata"`
}

type Match struct {
	Basis         []string `json:"basis"`
	SubjectTitle  string   `json:"subjectTitle,omitempty"`
	EpisodeNumber *float64 `json:"episodeNumber,omitempty"`
	Evidence      string   `json:"evidence,omitempty"`
}

type Transport struct {
	Type       string            `json:"type"`
	URL        string            `json:"url,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
	ExpiresAt  *int64            `json:"expiresAt,omitempty"`
	Magnet     string            `json:"magnet,omitempty"`
	InfoHash   string            `json:"infoHash,omitempty"`
	TorrentURL string            `json:"torrentUrl,omitempty"`
	FileIndex  *int              `json:"fileIndex,omitempty"`
	Trackers   []string          `json:"trackers,omitempty"`
}

type Metadata struct {
	Resolution        string   `json:"resolution,omitempty"`
	SubtitleLanguages []string `json:"subtitleLanguages,omitempty"`
	Channel           string   `json:"channel,omitempty"`
	ChannelTier       *int     `json:"channelTier,omitempty"`
	Episode           *float64 `json:"episode,omitempty"`
	Fansub            string   `json:"fansub,omitempty"`
	SizeBytes         *int64   `json:"sizeBytes,omitempty"`
	Seeders           *int64   `json:"seeders,omitempty"`
	PublishedAt       string   `json:"publishedAt,omitempty"`
}

type Source struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Kind         string   `json:"kind"`
	Tier         int      `json:"tier"`
	Version      string   `json:"version"`
	Enabled      bool     `json:"enabled"`
	Status       string   `json:"status"`
	Capabilities []string `json:"capabilities"`
}

type SelfCheckResult struct {
	SourceID   string           `json:"sourceId"`
	Status     string           `json:"status"`
	DurationMS int64            `json:"durationMs"`
	Stages     map[string]int64 `json:"stages,omitempty"`
	Category   string           `json:"category,omitempty"`
	Message    string           `json:"message,omitempty"`
}

type Error struct {
	Category  string
	Retryable bool
	Message   string
	Cause     error
}

func (err *Error) Error() string {
	if err.Message != "" {
		return err.Message
	}
	return err.Category
}

func (err *Error) Unwrap() error { return err.Cause }

func NewError(category string, retryable bool, message string, cause error) error {
	return &Error{Category: category, Retryable: retryable, Message: message, Cause: cause}
}

type Runner interface {
	Source() Source
	Candidates(context.Context, ResolveRequest, func(Candidate) error) error
	SelfCheck(context.Context, string) SelfCheckResult
}

func durationMilliseconds(started time.Time) int64 {
	value := time.Since(started).Milliseconds()
	if value < 0 {
		return 0
	}
	return value
}

func pointer[T any](value T) *T { return &value }

func sourceError(category, message string, retryable bool, cause error) error {
	if message == "" {
		message = fmt.Sprintf("source failed with %s", category)
	}
	return NewError(category, retryable, message, cause)
}
