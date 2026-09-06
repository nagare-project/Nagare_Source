package btcrawler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// StaticFetcher feeds a saved response through the same extraction path as a
// network request. It is intended for offline fixtures and CI self-tests.
type StaticFetcher struct {
	Response Response
}

func (fetcher StaticFetcher) Fetch(_ context.Context, request Request) (Response, error) {
	response := fetcher.Response
	if int64(len(response.Body)) > request.MaxBytes {
		return Response{}, fmt.Errorf("response exceeds %d bytes", request.MaxBytes)
	}
	if response.URL == "" {
		response.URL = request.URL
	}
	return response, nil
}

// HTTPFetcher executes a Source Spec request with redirect, DNS, timeout, and
// response-size enforcement. It intentionally does not use environment proxy
// variables because they would bypass the target-address checks.
type HTTPFetcher struct{}

func (HTTPFetcher) Fetch(ctx context.Context, request Request) (Response, error) {
	if err := validateRequestTarget(request.URL, request.AllowedHosts); err != nil {
		return Response{}, err
	}
	timeout := time.Duration(request.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	requestContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	dialer := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy:                 nil,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			if !hostAllowed(strings.ToLower(strings.TrimSuffix(host, ".")), request.AllowedHosts) {
				return nil, fmt.Errorf("host %q is outside allowed_hosts", host)
			}
			addresses, err := resolvePublic(ctx, host)
			if err != nil {
				return nil, err
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(addresses[0].String(), port))
		},
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(next *http.Request, previous []*http.Request) error {
			if len(previous) > request.MaxRedirects {
				return fmt.Errorf("redirect limit %d exceeded", request.MaxRedirects)
			}
			return validateRequestTarget(next.URL.String(), request.AllowedHosts)
		},
	}
	httpRequest, err := http.NewRequestWithContext(requestContext, request.Method, request.URL, bytes.NewReader(request.Body))
	if err != nil {
		return Response{}, err
	}
	httpRequest.Header = request.Headers.Clone()
	if httpRequest.Header.Get("User-Agent") == "" {
		httpRequest.Header.Set("User-Agent", "Nagare-Source/1 (+https://github.com/nagare-project/Nagare_Source)")
	}
	for key := range httpRequest.Header {
		if strings.EqualFold(key, "Authorization") || strings.EqualFold(key, "Cookie") {
			return Response{}, fmt.Errorf("fixed %s header is forbidden", key)
		}
	}
	response, err := client.Do(httpRequest)
	if err != nil {
		return Response{}, sanitizeRequestError(err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return Response{}, fmt.Errorf("HTTP status %d", response.StatusCode)
	}
	if response.ContentLength > request.MaxBytes {
		return Response{}, fmt.Errorf("response content length %d exceeds %d bytes", response.ContentLength, request.MaxBytes)
	}
	reader := io.LimitReader(response.Body, request.MaxBytes+1)
	body, err := io.ReadAll(reader)
	if err != nil {
		return Response{}, err
	}
	if int64(len(body)) > request.MaxBytes {
		return Response{}, fmt.Errorf("response exceeds %d bytes", request.MaxBytes)
	}
	return Response{URL: response.Request.URL.String(), Body: body}, nil
}

func sanitizeRequestError(err error) error {
	operation := "HTTP"
	for range 8 {
		urlError, ok := err.(*url.Error)
		if !ok {
			break
		}
		if urlError.Op != "" {
			operation = urlError.Op
		}
		if urlError.Err == nil {
			break
		}
		err = urlError.Err
	}
	return fmt.Errorf("%s request failed: %w", operation, err)
}

func validateRequestTarget(rawURL string, allowedHosts []string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("scheme %q is not allowed", parsed.Scheme)
	}
	if parsed.User != nil {
		return errors.New("URL credentials are forbidden")
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if host == "" {
		return errors.New("URL has no host")
	}
	if !hostAllowed(host, allowedHosts) {
		return fmt.Errorf("host %q is outside allowed_hosts", host)
	}
	if ip := net.ParseIP(host); ip != nil && !isPublicIP(ip) {
		return fmt.Errorf("non-public IP %q is forbidden", host)
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return fmt.Errorf("local host %q is forbidden", host)
	}
	port := parsed.Port()
	if port != "" {
		value, err := strconv.Atoi(port)
		if err != nil || value < 1 || value > 65535 {
			return errors.New("URL port is invalid")
		}
	}
	return nil
}

func resolvePublic(ctx context.Context, host string) ([]net.IP, error) {
	if parsed := net.ParseIP(host); parsed != nil {
		if !isPublicIP(parsed) {
			return nil, fmt.Errorf("non-public IP %q is forbidden", host)
		}
		return []net.IP{parsed}, nil
	}
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve %q: %w", host, err)
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("host %q resolved to no addresses", host)
	}
	result := make([]net.IP, 0, len(addresses))
	for _, address := range addresses {
		if !isPublicIP(address.IP) {
			return nil, fmt.Errorf("host %q resolved to forbidden address %s", host, address.IP)
		}
		result = append(result, address.IP)
	}
	return result, nil
}

func isPublicIP(ip net.IP) bool {
	return ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() &&
		!ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() && !ip.IsUnspecified()
}

func hostAllowed(host string, allowedHosts []string) bool {
	for _, allowed := range allowedHosts {
		allowed = strings.ToLower(strings.TrimSuffix(allowed, "."))
		if host == allowed {
			return true
		}
		if strings.HasPrefix(allowed, "*.") {
			suffix := strings.TrimPrefix(allowed, "*")
			if strings.HasSuffix(host, suffix) && host != strings.TrimPrefix(suffix, ".") {
				return true
			}
		}
	}
	return false
}
