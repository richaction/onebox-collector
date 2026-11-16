package resolver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"time"
)

const mobileUA = "Mozilla/5.0 (Linux; Android 13) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0 Mobile Safari/537.36"

var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("2002::/16"),
}

func publicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return false
	}
	for _, block := range nonPublic {
		if block.Contains(ip) {
			return false
		}
	}
	return true
}

// PublicHTTP pins each connection to a validated public IP, including redirects.
func PublicHTTP(timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	dial := &net.Dialer{Timeout: 5 * time.Second}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
		for _, ip := range addrs {
			if !publicIP(ip) {
				continue
			}
			conn, err := dial.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
		}
		return nil, errors.New("no reachable public address")
	}
	return &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if !validHTTP(req.URL) || len(via) > 4 {
			return http.ErrUseLastResponse
		}
		return nil
	}}
}

func validHTTP(u *url.URL) bool {
	return u.Hostname() != "" && u.User == nil && (u.Scheme == "https" || u.Scheme == "http") &&
		(u.Port() == "" || u.Port() == "443" || u.Port() == "80")
}

func request(ctx context.Context, client *http.Client, rawURL, referer, ua string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil || !validHTTP(req.URL) {
		return nil, errors.New("无效的媒体或页面地址")
	}
	if ua == "" {
		ua = mobileUA
	}
	req.Header.Set("User-Agent", ua)
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("请求失败")
	}
	return resp, nil
}

func readPage(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	const pageLimit = 5 << 20
	b, err := io.ReadAll(io.LimitReader(resp.Body, pageLimit+1))
	if err != nil || len(b) > pageLimit {
		return nil, errors.New("页面读取失败或超过大小上限")
	}
	return b, nil
}

type Assets interface {
	StoreAsset(date, kind string, source io.Reader) (path string, size int64, err error)
}

type Cache interface {
	CacheGet(ctx context.Context, key string, target any) (bool, error)
	CachePut(ctx context.Context, key string, value any, ttl time.Duration) error
}

func saveRemote(ctx context.Context, httpClient *http.Client, assets Assets, date, kind, link, referer string) (string, error) {
	resp, err := request(ctx, httpClient, link, referer, "")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	path, _, err := assets.StoreAsset(date, kind, resp.Body)
	return path, err
}
