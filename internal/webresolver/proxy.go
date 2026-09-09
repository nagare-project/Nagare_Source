package webresolver

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type outboundProxy struct {
	policy   *urlPolicy
	maxBytes int64
	listener net.Listener
	server   *http.Server

	mu          sync.Mutex
	connections map[net.Conn]bool
	violation   error
	requests    atomic.Uint64
}

func startOutboundProxy(policy *urlPolicy, maxBytes int64) (*outboundProxy, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	proxy := &outboundProxy{policy: policy, maxBytes: maxBytes, listener: listener, connections: map[net.Conn]bool{}}
	proxy.server = &http.Server{
		Handler:           proxy,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       30 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	go func() {
		_ = proxy.server.Serve(listener)
	}()
	return proxy, nil
}

func (proxy *outboundProxy) URL() string {
	return "http://" + proxy.listener.Addr().String()
}

func (proxy *outboundProxy) Close() {
	if proxy == nil {
		return
	}
	context, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = proxy.server.Shutdown(context)
	proxy.mu.Lock()
	for connection := range proxy.connections {
		_ = connection.Close()
	}
	proxy.connections = map[net.Conn]bool{}
	proxy.mu.Unlock()
}

func (proxy *outboundProxy) Violation() error {
	proxy.mu.Lock()
	defer proxy.mu.Unlock()
	return proxy.violation
}

func (proxy *outboundProxy) recordViolation(err error) {
	proxy.mu.Lock()
	if proxy.violation == nil {
		proxy.violation = err
	}
	proxy.mu.Unlock()
}

func (proxy *outboundProxy) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	proxy.requests.Add(1)
	if request.Method == http.MethodConnect {
		proxy.serveConnect(writer, request)
		return
	}
	proxy.serveHTTP(writer, request)
}

func (proxy *outboundProxy) serveConnect(writer http.ResponseWriter, request *http.Request) {
	addresses, port, err := proxy.policy.resolveAddress(request.Context(), request.Host, "443")
	if err != nil {
		proxy.recordViolation(err)
		http.Error(writer, "destination blocked by browser policy", http.StatusForbidden)
		return
	}
	upstream, err := dialResolved(request.Context(), "tcp", addresses, port)
	if err != nil {
		http.Error(writer, "destination unavailable", http.StatusBadGateway)
		return
	}
	hijacker, ok := writer.(http.Hijacker)
	if !ok {
		_ = upstream.Close()
		http.Error(writer, "proxy tunnel unavailable", http.StatusInternalServerError)
		return
	}
	client, buffered, err := hijacker.Hijack()
	if err != nil {
		_ = upstream.Close()
		return
	}
	if _, err := buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		_ = client.Close()
		_ = upstream.Close()
		return
	}
	if err := buffered.Flush(); err != nil {
		_ = client.Close()
		_ = upstream.Close()
		return
	}
	proxy.track(client, true)
	proxy.track(upstream, true)
	go func() {
		var once sync.Once
		closeBoth := func() {
			once.Do(func() {
				_ = client.Close()
				_ = upstream.Close()
				proxy.track(client, false)
				proxy.track(upstream, false)
			})
		}
		go func() {
			// A client may pipeline tunnel bytes after CONNECT. Preserve bytes
			// already buffered by net/http when the connection is hijacked.
			_, _ = io.Copy(upstream, buffered)
			closeBoth()
		}()
		_, _ = io.Copy(client, upstream)
		closeBoth()
	}()
}

func (proxy *outboundProxy) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.URL == nil || !request.URL.IsAbs() {
		http.Error(writer, "absolute proxy URL required", http.StatusBadRequest)
		return
	}
	parsed, addresses, err := proxy.policy.validateURL(request.Context(), request.URL.String())
	if err != nil {
		proxy.recordViolation(err)
		http.Error(writer, "destination blocked by browser policy", http.StatusForbidden)
		return
	}
	port := parsed.Port()
	if port == "" {
		if parsed.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	transport := &http.Transport{
		Proxy:                 nil,
		DisableCompression:    true,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12, ServerName: parsed.Hostname()},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialResolved(ctx, network, addresses, port)
		},
	}
	defer transport.CloseIdleConnections()
	outgoing := request.Clone(request.Context())
	outgoing.RequestURI = ""
	outgoing.URL = cloneURL(parsed)
	removeHopHeaders(outgoing.Header)
	response, err := transport.RoundTrip(outgoing)
	if err != nil {
		http.Error(writer, "destination unavailable", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	removeHopHeaders(response.Header)
	for key, values := range response.Header {
		for _, value := range values {
			writer.Header().Add(key, value)
		}
	}
	writer.WriteHeader(response.StatusCode)
	limit := proxy.maxBytes
	if limit <= 0 {
		limit = 5 * 1024 * 1024
	}
	_, _ = io.Copy(writer, io.LimitReader(response.Body, limit))
}

func (proxy *outboundProxy) track(connection net.Conn, add bool) {
	proxy.mu.Lock()
	if add {
		proxy.connections[connection] = true
	} else {
		delete(proxy.connections, connection)
	}
	proxy.mu.Unlock()
}

func dialResolved(ctx context.Context, network string, addresses []net.IP, port string) (net.Conn, error) {
	var last error
	for _, address := range addresses {
		connection, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, net.JoinHostPort(address.String(), port))
		if err == nil {
			return connection, nil
		}
		last = err
	}
	if last == nil {
		last = errors.New("no destination address")
	}
	return nil, last
}

func removeHopHeaders(headers http.Header) {
	connectionValues := append([]string(nil), headers.Values("Connection")...)
	for _, key := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"} {
		headers.Del(key)
	}
	if len(connectionValues) > 0 {
		for _, value := range connectionValues {
			for _, key := range strings.Split(value, ",") {
				headers.Del(strings.TrimSpace(key))
			}
		}
	}
}

func cloneURL(value *url.URL) *url.URL {
	copy := *value
	return &copy
}
