package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/hellowind777/hellogrok/internal/config"
)

const (
	upstreamTimeoutGrace = time.Second

	// Grok Build's shell default is 600 seconds per inference idle interval.
	// Keep the facade one second behind that boundary so Grok Build owns the
	// timeout classification instead of receiving an earlier gateway error.
	defaultUpstreamResponseHeaderTimeout = 10*time.Minute + upstreamTimeoutGrace
	defaultUpstreamBodyIdleTimeout       = 10*time.Minute + upstreamTimeoutGrace
	defaultDeepSeekResponseHeaderTimeout = 11 * time.Minute
	defaultDeepSeekBodyIdleTimeout       = 11 * time.Minute
	minimumInferenceIdleTimeout          = 10 * time.Second
)

var (
	errUpstreamBodyIdleTimeout       = errors.New("upstream response body idle timeout")
	errUpstreamResponseHeaderTimeout = errors.New("upstream response header timeout")
)

// routeUpstreamIdleTimeout keeps hellogrok just behind Grok Build's configured
// per-chunk deadline. The small grace prevents the proxy from winning a timer
// race and replacing Grok Build's native IdleTimeout with a gateway error.
func routeUpstreamIdleTimeout(route config.Route, fallback time.Duration) time.Duration {
	if !route.InferenceIdleTimeoutConfigured {
		return fallback
	}
	const maxDuration = time.Duration(1<<63 - 1)
	seconds := route.InferenceIdleTimeoutSecs
	if seconds < uint64(minimumInferenceIdleTimeout/time.Second) {
		seconds = uint64(minimumInferenceIdleTimeout / time.Second)
	}
	maxSeconds := uint64((maxDuration - upstreamTimeoutGrace) / time.Second)
	if seconds >= maxSeconds {
		return maxDuration
	}
	return time.Duration(seconds)*time.Second + upstreamTimeoutGrace
}

// doUpstreamRequest applies a deadline only until response headers arrive.
// http.Client.Timeout would also cap a healthy long-running stream.
func doUpstreamRequest(client *http.Client, request *http.Request, timeout time.Duration, cancel context.CancelFunc) (*http.Response, error) {
	if timeout <= 0 {
		return client.Do(request)
	}

	var stateMu sync.Mutex
	returned := false
	timedOut := false
	timer := time.AfterFunc(timeout, func() {
		stateMu.Lock()
		defer stateMu.Unlock()
		if returned {
			return
		}
		timedOut = true
		cancel()
	})

	response, err := client.Do(request)
	stateMu.Lock()
	returned = true
	headerTimedOut := timedOut
	stateMu.Unlock()
	_ = timer.Stop()
	if headerTimedOut {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		return nil, errUpstreamResponseHeaderTimeout
	}
	return response, err
}

type idleTimeoutReadCloser struct {
	body       io.ReadCloser
	timeout    time.Duration
	mu         sync.Mutex
	generation uint64
	closed     bool
	timedOut   bool
}

func withBodyIdleTimeout(body io.ReadCloser, timeout time.Duration) io.ReadCloser {
	if body == nil || timeout <= 0 {
		return body
	}
	return &idleTimeoutReadCloser{body: body, timeout: timeout}
}

func (reader *idleTimeoutReadCloser) Read(buffer []byte) (int, error) {
	reader.mu.Lock()
	if reader.closed {
		timedOut := reader.timedOut
		reader.mu.Unlock()
		if timedOut {
			return 0, errUpstreamBodyIdleTimeout
		}
		return 0, io.ErrClosedPipe
	}
	reader.generation++
	generation := reader.generation
	reader.mu.Unlock()

	timer := time.AfterFunc(reader.timeout, func() {
		reader.expire(generation)
	})
	count, err := reader.body.Read(buffer)
	_ = timer.Stop()

	reader.mu.Lock()
	if reader.generation == generation {
		reader.generation++
	}
	timedOut := reader.timedOut
	closed := reader.closed
	reader.mu.Unlock()

	if timedOut {
		if count > 0 {
			return count, nil
		}
		return 0, errUpstreamBodyIdleTimeout
	}
	if closed && count == 0 && err == nil {
		return 0, io.ErrClosedPipe
	}
	return count, err
}

func (reader *idleTimeoutReadCloser) Close() error {
	reader.mu.Lock()
	if reader.closed {
		reader.mu.Unlock()
		return nil
	}
	reader.closed = true
	reader.generation++
	reader.mu.Unlock()
	return reader.body.Close()
}

func (reader *idleTimeoutReadCloser) expire(generation uint64) {
	reader.mu.Lock()
	if reader.closed || reader.generation != generation {
		reader.mu.Unlock()
		return
	}
	reader.closed = true
	reader.timedOut = true
	reader.generation++
	reader.mu.Unlock()
	_ = reader.body.Close()
}

func isUpstreamTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errUpstreamResponseHeaderTimeout) {
		return true
	}
	var networkError net.Error
	return errors.As(err, &networkError) && networkError.Timeout()
}

func upstreamStreamFailureMessage(protocol string, err error) string {
	if errors.Is(err, errUpstreamBodyIdleTimeout) {
		return "upstream " + protocol + " stream timed out waiting for data"
	}
	return "upstream " + protocol + " stream failed"
}
