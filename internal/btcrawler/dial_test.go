package btcrawler

import (
	"context"
	"errors"
	"net"
	"reflect"
	"sync"
	"testing"
	"time"
)

type trackedConn struct {
	net.Conn
	once   sync.Once
	closed chan struct{}
}

func newTrackedConn(t *testing.T) *trackedConn {
	t.Helper()
	local, remote := net.Pipe()
	t.Cleanup(func() { local.Close(); remote.Close() })
	return &trackedConn{Conn: local, closed: make(chan struct{})}
}

func (conn *trackedConn) Close() error {
	conn.once.Do(func() { close(conn.closed) })
	return conn.Conn.Close()
}

func ips(values ...string) []net.IP {
	result := make([]net.IP, len(values))
	for index, value := range values {
		result[index] = net.ParseIP(value)
	}
	return result
}

func TestInterleaveFamiliesKeepsResolvedPreference(t *testing.T) {
	got := interleaveFamilies(ips("2001:db8::1", "2001:db8::2", "192.0.2.1", "192.0.2.2", "192.0.2.3"))
	want := ips("2001:db8::1", "192.0.2.1", "2001:db8::2", "192.0.2.2", "192.0.2.3")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	got = interleaveFamilies(ips("192.0.2.1", "2001:db8::1"))
	want = ips("192.0.2.1", "2001:db8::1")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// IPv6 路由坏掉（连接一直挂着）时，不能把整个请求超时耗在它身上。
func TestDialFastestFallsBackFromHangingAddress(t *testing.T) {
	winner := newTrackedConn(t)
	hangingCancelled := make(chan struct{})
	dial := func(ctx context.Context, _, address string) (net.Conn, error) {
		if address == "[2001:db8::1]:443" {
			<-ctx.Done()
			close(hangingCancelled)
			return nil, ctx.Err()
		}
		return winner, nil
	}
	started := time.Now()
	conn, err := dialFastest(context.Background(), dial, "tcp", ips("2001:db8::1", "192.0.2.1"), "443", 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if conn != winner {
		t.Fatalf("conn = %v, want IPv4 connection", conn)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("fallback took %s", elapsed)
	}
	select {
	case <-hangingCancelled:
	case <-time.After(time.Second):
		t.Fatal("hanging attempt was not cancelled after a winner")
	}
}

func TestDialFastestStartsNextAddressImmediatelyAfterFailure(t *testing.T) {
	winner := newTrackedConn(t)
	dial := func(_ context.Context, _, address string) (net.Conn, error) {
		if address == "192.0.2.1:80" {
			return nil, errors.New("connection refused")
		}
		return winner, nil
	}
	// delay 设成一小时：只有“失败立即起下一个”才能让这个测试按时结束。
	conn, err := dialFastest(context.Background(), dial, "tcp", ips("192.0.2.1", "192.0.2.2"), "80", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if conn != winner {
		t.Fatalf("conn = %v, want second address", conn)
	}
}

func TestDialFastestReturnsLastErrorWhenAllFail(t *testing.T) {
	dial := func(_ context.Context, _, address string) (net.Conn, error) {
		return nil, errors.New("refused " + address)
	}
	_, err := dialFastest(context.Background(), dial, "tcp", ips("2001:db8::1", "192.0.2.1"), "443", time.Hour)
	if err == nil || err.Error() != "refused 192.0.2.1:443" {
		t.Fatalf("err = %v, want last address error", err)
	}
}

func TestDialFastestClosesLateConnections(t *testing.T) {
	winner := newTrackedConn(t)
	late := newTrackedConn(t)
	release := make(chan struct{})
	dial := func(_ context.Context, _, address string) (net.Conn, error) {
		if address == "[2001:db8::1]:443" {
			<-release
			return late, nil
		}
		return winner, nil
	}
	conn, err := dialFastest(context.Background(), dial, "tcp", ips("2001:db8::1", "192.0.2.1"), "443", 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if conn != winner {
		t.Fatalf("conn = %v, want IPv4 connection", conn)
	}
	close(release)
	select {
	case <-late.closed:
	case <-time.After(time.Second):
		t.Fatal("late connection leaked")
	}
	select {
	case <-winner.closed:
		t.Fatal("winning connection must stay open")
	default:
	}
}

func TestDialFastestHonoursContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	_, err := dialFastest(ctx, dial, "tcp", ips("2001:db8::1", "192.0.2.1"), "443", 10*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
}
