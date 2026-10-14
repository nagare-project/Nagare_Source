package btcrawler

import (
	"context"
	"errors"
	"net"
	"time"
)

// happyEyeballsDelay 是上一个地址还没连上时起下一个地址的间隔（RFC 8305 建议 250ms 左右）。
const happyEyeballsDelay = 300 * time.Millisecond

type dialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// dialFastest 对已校验过的地址做 Happy Eyeballs：IPv6/IPv4 交错排序，上一个地址 delay 内没连上
// 就并发起下一个，失败立即起下一个，先连上的胜出。串行逐个拨号时，一条坏掉的 IPv6 路由就能把
// 整个请求超时耗光（Go 的解析结果 IPv6 在前），而 curl、浏览器早就回落到 IPv4 了。
func dialFastest(ctx context.Context, dial dialFunc, network string, addresses []net.IP, port string, delay time.Duration) (net.Conn, error) {
	if len(addresses) == 0 {
		return nil, errors.New("no address to dial")
	}
	ordered := interleaveFamilies(addresses)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type attempt struct {
		conn net.Conn
		err  error
	}
	results := make(chan attempt, len(ordered))
	next, pending := 0, 0
	var tick <-chan time.Time
	start := func() {
		address := net.JoinHostPort(ordered[next].String(), port)
		next++
		pending++
		go func() {
			conn, err := dial(ctx, network, address)
			results <- attempt{conn: conn, err: err}
		}()
		tick = nil
		if next < len(ordered) {
			tick = time.After(delay)
		}
	}
	// 胜出或放弃之后，还在路上的尝试随 cancel 中止；晚到的连接必须关掉，否则会泄漏。
	closeLate := func(remaining int) {
		for range remaining {
			if late := <-results; late.conn != nil {
				late.conn.Close()
			}
		}
	}

	start()
	var lastError error
	for {
		select {
		case result := <-results:
			pending--
			if result.err == nil {
				go closeLate(pending)
				return result.conn, nil
			}
			lastError = result.err
			if next < len(ordered) {
				start()
			} else if pending == 0 {
				return nil, lastError
			}
		case <-tick:
			start()
		case <-ctx.Done():
			go closeLate(pending)
			return nil, ctx.Err()
		}
	}
}

// interleaveFamilies 保持解析顺序决定首选地址族，再与另一族交替排列。
func interleaveFamilies(addresses []net.IP) []net.IP {
	var primary, secondary []net.IP
	firstIsV4 := addresses[0].To4() != nil
	for _, address := range addresses {
		if (address.To4() != nil) == firstIsV4 {
			primary = append(primary, address)
		} else {
			secondary = append(secondary, address)
		}
	}
	ordered := make([]net.IP, 0, len(addresses))
	for index := 0; index < len(primary) || index < len(secondary); index++ {
		if index < len(primary) {
			ordered = append(ordered, primary[index])
		}
		if index < len(secondary) {
			ordered = append(ordered, secondary[index])
		}
	}
	return ordered
}
