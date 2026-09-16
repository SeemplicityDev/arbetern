package httpx

import (
	"context"
	"crypto/rand"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RetryableStatus reports whether a response status is worth retrying: 429
// and the transient 5xx family. Client errors are the caller's to fix.
func RetryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	}
	return false
}

// Backoff returns the delay before the given 1-based retry attempt:
// exponential from 1s, capped at 30s, with up to 250ms of jitter so parallel
// callers do not retry in lockstep. A longer server hint wins, capped at one
// minute.
func Backoff(attempt int, serverHint time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 6 {
		attempt = 6
	}
	d := time.Second << (attempt - 1)
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	if serverHint > d {
		d = serverHint
	}
	if d > time.Minute {
		d = time.Minute
	}
	return d + jitter(250*time.Millisecond)
}

func jitter(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(max)))
	if err != nil {
		return max / 2
	}
	return time.Duration(n.Int64())
}

// RetryAfter parses a Retry-After header in its delay-seconds or HTTP-date
// form. Absent or unparsable values yield 0.
func RetryAfter(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// SleepCtx waits for d or until ctx is done, whichever comes first.
func SleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
