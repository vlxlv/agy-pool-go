package proxy

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"time"
)

var (
	// BaseTransport is configured with DisableKeepAlives to eliminate hidden connection-reuse retries.
	BaseTransport = &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 0,
		}).DialContext,
		ForceAttemptHTTP2:     false,
		DisableKeepAlives:     true, // Project invariant: no hidden connection-reuse retries
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 300 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	HTTPClient = &http.Client{
		Transport: BaseTransport,
		Timeout:   300 * time.Second,
	}
)

// IsTimeoutError reports whether an error was caused by an upstream timeout.
func IsTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		if errors.Is(urlErr.Err, context.DeadlineExceeded) {
			return true
		}
		if nErr, ok := urlErr.Err.(net.Error); ok && nErr.Timeout() {
			return true
		}
	}
	return false
}
