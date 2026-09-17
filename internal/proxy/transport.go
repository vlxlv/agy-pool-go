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

	defaultHTTPClient = &http.Client{
		Transport: BaseTransport,
		Timeout:   300 * time.Second,
	}
	HTTPClient = defaultHTTPClient

	// GenerationTransport is a dedicated transport for generation requests.
	// Configured with DisableKeepAlives to guarantee at-most-once delivery.
	GenerationTransport = &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 0,
		}).DialContext,
		ForceAttemptHTTP2:     false,
		DisableKeepAlives:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 300 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	defaultGenerationClient = &http.Client{
		Transport: &GenerationRoundTripper{Base: GenerationTransport},
		Timeout:   300 * time.Second,
	}
	GenerationClient = defaultGenerationClient

	// RoundTripHook is an optional callback for tests to monitor physical upstream round trips.
	RoundTripHook func(req *http.Request)
)

// GenerationRoundTripper wraps an http.RoundTripper for generation endpoints.
// It enforces no-replay semantics and invokes RoundTripHook if configured.
type GenerationRoundTripper struct {
	Base http.RoundTripper
}

func (rt *GenerationRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	// Structural invariant: generation requests must never have GetBody or idempotency headers
	req.GetBody = nil
	req.Header.Del("Idempotency-Key")
	req.Header.Del("X-Idempotency-Key")

	if RoundTripHook != nil {
		RoundTripHook(req)
	}

	base := rt.Base
	if base == nil {
		base = BaseTransport
	}
	return base.RoundTrip(req)
}

// GetClient returns the appropriate http.Client for the request type.
func GetClient(isGeneration bool) *http.Client {
	if isGeneration {
		if GenerationClient != nil && GenerationClient != defaultGenerationClient {
			return GenerationClient
		}
		if HTTPClient != nil && HTTPClient != defaultHTTPClient {
			return HTTPClient
		}
		return defaultGenerationClient
	}
	if HTTPClient != nil {
		return HTTPClient
	}
	return defaultHTTPClient
}

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
