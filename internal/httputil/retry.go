package httputil

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RateLimitError is returned by API functions when the server responds 429.
// The Wait field holds how long to pause before the next attempt.
type RateLimitError struct {
	Wait time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("rate limited (HTTP 429) — retry after %v", e.Wait)
}

// TransientError wraps a retriable 5xx or network-level error.
// Callers that implement retry logic detect this type and back off before
// retrying. Permanent client errors (4xx other than 429) are not wrapped.
type TransientError struct {
	cause error
}

func NewTransientError(err error) *TransientError { return &TransientError{cause: err} }
func (e *TransientError) Error() string           { return e.cause.Error() }
func (e *TransientError) Unwrap() error           { return e.cause }

// ParseRetryAfter extracts the Retry-After delay from a 429 response, falling
// back to defaultWait if the header is absent or unparseable.
func ParseRetryAfter(resp *http.Response, defaultWait time.Duration) time.Duration {
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(strings.TrimSpace(ra)); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second
		}
	}
	return defaultWait
}

// RetryOptions configures the retry loop in RetryFunc.
// Zero values use the defaults shown in the field comments.
type RetryOptions struct {
	MaxRateLimitAttempts int           // max 429 retries; default 3
	MaxTransientAttempts int           // max 5xx/network retries; default 3
	BaseDelay            time.Duration // initial transient delay, doubles each time; default 2s
}

func (o RetryOptions) rateLimitMax() int {
	if o.MaxRateLimitAttempts == 0 {
		return 3
	}
	return o.MaxRateLimitAttempts
}

func (o RetryOptions) transientMax() int {
	if o.MaxTransientAttempts == 0 {
		return 3
	}
	return o.MaxTransientAttempts
}

func (o RetryOptions) baseDelay() time.Duration {
	if o.BaseDelay == 0 {
		return 2 * time.Second
	}
	return o.BaseDelay
}

// RetryFunc calls fn repeatedly until it returns nil or all retry budgets are
// exhausted. fn should return *RateLimitError or *TransientError for retriable
// failures; any other error causes an immediate return without retry.
func RetryFunc(ctx context.Context, fn func() error, opts RetryOptions) error {
	rateLimitRetries := 0
	transientRetries := 0
	for {
		err := fn()
		if err == nil {
			return nil
		}

		var rl *RateLimitError
		if errors.As(err, &rl) {
			if rateLimitRetries >= opts.rateLimitMax() {
				return err
			}
			rateLimitRetries++
			fmt.Printf("\n  rate limited (429) — pausing %v before retry...\n", rl.Wait)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(rl.Wait):
			}
			continue
		}

		var te *TransientError
		if errors.As(err, &te) {
			if transientRetries >= opts.transientMax() {
				return err
			}
			transientRetries++
			delay := opts.baseDelay() * (1 << uint(transientRetries-1))
			fmt.Printf("    transient error — retrying in %v (attempt %d/%d)...\n",
				delay, transientRetries, opts.transientMax())
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
			continue
		}

		return err // permanent error — don't retry
	}
}
