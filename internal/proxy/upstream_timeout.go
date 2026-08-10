package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

const (
	defaultUpstreamResponseHeaderTimeout = 3 * time.Minute
	defaultUpstreamStreamIdleTimeout     = 3 * time.Minute
)

var errUpstreamStreamIdleTimeout = errors.New("upstream SSE stream idle timeout")

type idleTimeoutReadCloser struct {
	body       io.ReadCloser
	timeout    time.Duration
	mu         sync.Mutex
	generation uint64
	closed     bool
	timedOut   bool
}

func withStreamIdleTimeout(body io.ReadCloser, timeout time.Duration) io.ReadCloser {
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
			return 0, errUpstreamStreamIdleTimeout
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
		return 0, errUpstreamStreamIdleTimeout
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
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var networkError net.Error
	return errors.As(err, &networkError) && networkError.Timeout()
}

func upstreamStreamFailureMessage(protocol string, err error) string {
	if errors.Is(err, errUpstreamStreamIdleTimeout) {
		return "upstream " + protocol + " stream timed out waiting for data"
	}
	return "upstream " + protocol + " stream failed"
}
