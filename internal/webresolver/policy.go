package webresolver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

type urlPolicy struct {
	allowed      []string
	resolver     ipResolver
	allowPrivate bool
	allowAnyHost bool
}

func newURLPolicy(allowed []string, resolver ipResolver, allowPrivate bool) (*urlPolicy, error) {
	if len(allowed) == 0 {
		return nil, errors.New("allowed_hosts is required")
	}
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	patterns := make([]string, 0, len(allowed))
	seen := map[string]bool{}
	for _, pattern := range allowed {
		pattern = strings.ToLower(strings.TrimSpace(pattern))
		if pattern == "" || strings.ContainsAny(pattern, "/:@?#") {
			return nil, fmt.Errorf("invalid allowed host %q", pattern)
		}
		if strings.HasPrefix(pattern, "*.") {
			if strings.Count(pattern, "*") != 1 || len(pattern) < 4 {
				return nil, fmt.Errorf("invalid wildcard host %q", pattern)
			}
		} else if strings.Contains(pattern, "*") {
			return nil, fmt.Errorf("invalid wildcard host %q", pattern)
		}
		if !seen[pattern] {
			seen[pattern] = true
			patterns = append(patterns, pattern)
		}
	}
	return &urlPolicy{allowed: patterns, resolver: resolver, allowPrivate: allowPrivate}, nil
}

// newPublicURLPolicy 供隔离浏览器加载页面依赖与媒体 CDN。它不限制公网 host，
// 但继续执行协议、凭据、DNS 固定与私网地址检查。页面导航仍使用来源声明的严格策略。
func newPublicURLPolicy(resolver ipResolver, allowPrivate bool) *urlPolicy {
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	return &urlPolicy{resolver: resolver, allowPrivate: allowPrivate, allowAnyHost: true}
}

func (policy *urlPolicy) validateURL(ctx context.Context, raw string) (*url.URL, []net.IP, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, nil, fmt.Errorf("URL scheme %q is forbidden", parsed.Scheme)
	}
	if parsed.User != nil {
		return nil, nil, errors.New("URL credentials are forbidden")
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return nil, nil, errors.New("URL has no host")
	}
	if !policy.hostAllowed(host) {
		return nil, nil, fmt.Errorf("host %q is outside allowed_hosts", host)
	}
	addresses, err := policy.resolveHost(ctx, host)
	if err != nil {
		return nil, nil, err
	}
	return parsed, addresses, nil
}

func (policy *urlPolicy) resolveAddress(ctx context.Context, hostPort, defaultPort string) ([]net.IP, string, error) {
	host := hostPort
	port := defaultPort
	if parsedHost, parsedPort, err := net.SplitHostPort(hostPort); err == nil {
		host = parsedHost
		port = parsedPort
	} else if strings.Contains(hostPort, ":") && net.ParseIP(hostPort) == nil {
		return nil, "", fmt.Errorf("invalid host and port")
	}
	host = strings.Trim(host, "[]")
	if !policy.hostAllowed(strings.ToLower(host)) {
		return nil, "", fmt.Errorf("host %q is outside allowed_hosts", strings.ToLower(host))
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return nil, "", fmt.Errorf("invalid destination port")
	}
	addresses, err := policy.resolveHost(ctx, host)
	if err != nil {
		return nil, "", err
	}
	return addresses, port, nil
}

func (policy *urlPolicy) resolveHost(ctx context.Context, host string) ([]net.IP, error) {
	if parsed := net.ParseIP(host); parsed != nil {
		if !policy.allowPrivate && unsafeIP(parsed) {
			return nil, fmt.Errorf("non-public destination %q is forbidden", host)
		}
		return []net.IP{parsed}, nil
	}
	addresses, err := policy.resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve host %q: %w", host, err)
	}
	var safe []net.IP
	for _, address := range addresses {
		if address.IP == nil || (!policy.allowPrivate && unsafeIP(address.IP)) {
			continue
		}
		safe = append(safe, address.IP)
	}
	if len(safe) == 0 {
		return nil, fmt.Errorf("host %q has no permitted address", host)
	}
	return safe, nil
}

func (policy *urlPolicy) hostAllowed(host string) bool {
	if policy.allowAnyHost {
		return true
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, pattern := range policy.allowed {
		if host == pattern {
			return true
		}
		if strings.HasPrefix(pattern, "*.") {
			suffix := strings.TrimPrefix(pattern, "*")
			if strings.HasSuffix(host, suffix) && host != strings.TrimPrefix(suffix, ".") {
				return true
			}
		}
	}
	return false
}

func unsafeIP(ip net.IP) bool {
	if !ip.IsGlobalUnicast() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return true
	}
	if ipv4 := ip.To4(); ipv4 != nil {
		// Shared address space (RFC 6598) is not classified as private by net.IP.
		return ipv4[0] == 100 && ipv4[1]&0xc0 == 0x40
	}
	return false
}

func RedactURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return "[redacted-url]"
	}
	parsed.User = nil
	parsed.Fragment = ""
	if parsed.RawQuery != "" {
		parsed.RawQuery = "redacted"
	}
	return parsed.String()
}
