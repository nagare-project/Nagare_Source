package webresolver

import (
	"context"
	"fmt"
	"net"
	"time"
)

const (
	CategoryCancelled      = "cancelled"
	CategoryResolveTimeout = "resolve_timeout"
	CategoryResolveFailed  = "resolve_failed"
	CategoryBrowserBlocked = "browser_blocked"
	CategoryUnsafeRedirect = "unsafe_redirect"
)

type Error struct {
	Category  string
	Retryable bool
	Message   string
	Cause     error
}

func (e *Error) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return e.Category
}

func (e *Error) Unwrap() error { return e.Cause }

type NetworkEventKind string

const (
	EventRequest  NetworkEventKind = "request"
	EventResponse NetworkEventKind = "response"
	EventFailed   NetworkEventKind = "failed"
)

type NetworkEvent struct {
	Kind            NetworkEventKind
	RequestID       string
	URL             string
	Method          string
	ResourceType    string
	Status          int
	MIMEType        string
	Headers         map[string]string
	Redirect        bool
	TopLevel        bool
	VerifiedMedia   bool
	ErrorText       string
	Navigate        func(context.Context, string) error
	SnapshotHeaders func(context.Context, string) (map[string]string, error)
}

type BrowseRequest struct {
	URL          string
	Headers      map[string]string
	Cookies      []string
	CookiePolicy string
	AllowedHosts []string
	MaxRedirects int
	MaxBytes     int64

	policy       *urlPolicy
	egressPolicy *urlPolicy
}

type Browser interface {
	Browse(ctx context.Context, request BrowseRequest, emit func(NetworkEvent)) error
}

type Media struct {
	URL       string            `json:"url"`
	Transport string            `json:"transport"`
	Headers   map[string]string `json:"headers,omitempty"`
	MIMEType  string            `json:"mimeType,omitempty"`
	Status    int               `json:"status"`
	Duration  time.Duration     `json:"-"`
}

type Logger interface {
	Printf(format string, arguments ...any)
}

type LoggerFunc func(format string, arguments ...any)

func (function LoggerFunc) Printf(format string, arguments ...any) {
	function(format, arguments...)
}

type ipResolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

type Runtime struct {
	Browser Browser
	Logger  Logger

	resolver     ipResolver
	allowPrivate bool
}

func New(browser Browser) *Runtime {
	return &Runtime{Browser: browser, resolver: net.DefaultResolver}
}

func (runtime *Runtime) logf(format string, arguments ...any) {
	if runtime.Logger != nil {
		runtime.Logger.Printf(format, arguments...)
	}
}

func wrapError(category string, retryable bool, message string, cause error) error {
	return &Error{Category: category, Retryable: retryable, Message: message, Cause: cause}
}

func asError(err error, target **Error) bool {
	for err != nil {
		if typed, ok := err.(*Error); ok {
			*target = typed
			return true
		}
		type unwrapper interface{ Unwrap() error }
		unwrapped, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = unwrapped.Unwrap()
	}
	return false
}

func browserFailure(message string, cause error) error {
	return wrapError(CategoryBrowserBlocked, true, fmt.Sprintf("browser session failed: %s", message), cause)
}
