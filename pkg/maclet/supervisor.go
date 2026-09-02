package maclet

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const (
	joinRetryInitial    = time.Second
	joinRetryMax        = time.Minute
	joinRetryResetAfter = 30 * time.Second
)

// runJoin supervises normal daemon sessions. A session owns local runtime
// resources until it exits; transient control-plane failures are retried with
// backoff, while --once remains a single deterministic session.
func runJoin(cfg JoinConfig) error {
	lock, err := acquireDaemonLock(cfg.StateDir)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := lock.Close(); closeErr != nil {
			log.Printf("warning: release daemon lock: %v", closeErr)
		}
	}()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if cfg.Once {
		return runJoinSession(ctx, cfg)
	}
	return superviseJoin(ctx, cfg, runJoinSession, joinRetryDelay)
}

func superviseJoin(ctx context.Context, cfg JoinConfig, session func(context.Context, JoinConfig) error, retryDelay func(int) time.Duration) error {
	return superviseJoinWithResetAfter(ctx, cfg, session, retryDelay, joinRetryResetAfter)
}

func superviseJoinWithResetAfter(ctx context.Context, cfg JoinConfig, session func(context.Context, JoinConfig) error, retryDelay func(int) time.Duration, resetAfter time.Duration) error {
	attempt := 0
	for {
		started := time.Now()
		err := session(ctx, cfg)
		if err == nil || ctx.Err() != nil {
			return nil
		}
		if !retryableJoinError(err) {
			return err
		}
		if resetAfter > 0 && time.Since(started) >= resetAfter {
			attempt = 0
		}
		delay := retryDelay(attempt)
		log.Printf("warning: maclet session unavailable: %v; retrying in %s", err, delay)
		attempt++
		if err := waitForJoinRetry(ctx, delay); err != nil {
			return nil
		}
	}
}

// joinRetryDelay is deliberately capped and deterministic. The retry loop is
// already serialized per state directory, so jitter is not needed to prevent
// a thundering herd on a single Mac and deterministic delays make recovery
// behavior easy to reason about.
func joinRetryDelay(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	delay := joinRetryInitial
	for i := 0; i < attempt && delay < joinRetryMax; i++ {
		delay *= 2
	}
	if delay > joinRetryMax {
		return joinRetryMax
	}
	return delay
}

func waitForJoinRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func retryableJoinError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, errPodCIDRUnavailable) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var apiErr *HTTPError
	if errors.As(err, &apiErr) {
		return apiErr.Code == 408 || apiErr.Code == 425 || apiErr.Code == 429 || apiErr.Code >= 500
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	return errors.Is(err, io.EOF)
}
