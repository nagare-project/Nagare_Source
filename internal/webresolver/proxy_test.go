package webresolver

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestOutboundProxyPreservesPipelinedTunnelBytes(t *testing.T) {
	upstream, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	go func() {
		conn, err := upstream.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(2 * time.Second))
		io.Copy(conn, conn)
	}()
	policy, _ := newURLPolicy([]string{"127.0.0.1"}, nil, true)
	proxy, err := startOutboundProxy(policy, 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	client, err := net.Dial("tcp", proxy.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	client.SetDeadline(time.Now().Add(2 * time.Second))
	fmt.Fprintf(client, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\npipelined", upstream.Addr(), upstream.Addr())
	reader := bufio.NewReader(client)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 {
		t.Fatalf("CONNECT status %d", response.StatusCode)
	}
	data := make([]byte, len("pipelined"))
	if _, err := io.ReadFull(reader, data); err != nil {
		t.Fatal(err)
	}
	if string(data) != "pipelined" {
		t.Fatalf("lost buffered tunnel bytes: %q", data)
	}
}

func TestOutboundProxyPinsAllowedDestinationAndBlocksOthers(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("loopback listeners are unavailable: %v", err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(writer, "proxied "+request.URL.Path)
	}))
	server.Listener = listener
	server.Start()
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := newURLPolicy([]string{parsed.Hostname()}, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := startOutboundProxy(policy, 1024)
	if err != nil {
		t.Skipf("loopback proxy listeners are unavailable: %v", err)
	}
	defer proxy.Close()
	proxyURL, _ := url.Parse(proxy.URL())
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	response, err := client.Get(server.URL + "/fixture?token=secret")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || string(body) != "proxied /fixture" {
		t.Fatalf("unexpected proxy response: status=%d body=%q", response.StatusCode, body)
	}

	blocked, err := client.Get("http://localhost:" + parsed.Port() + "/private?token=secret")
	if err != nil {
		t.Fatal(err)
	}
	blocked.Body.Close()
	if blocked.StatusCode != http.StatusForbidden || proxy.Violation() == nil {
		t.Fatalf("disallowed proxy destination was not blocked: status=%d violation=%v", blocked.StatusCode, proxy.Violation())
	}
	if strings.Contains(proxy.Violation().Error(), "token") {
		t.Fatalf("proxy violation leaked query: %v", proxy.Violation())
	}
}
