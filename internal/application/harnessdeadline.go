package application

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/FastR-D/FastTask/internal/agent/protocol"
	"github.com/FastR-D/FastTask/internal/persistence"
)

var errHeaderTimeout = errors.New("MODEL_HEADER_TIMEOUT")
var errIdleTimeout = errors.New("MODEL_IDLE_TIMEOUT")
var errActiveTimeout = errors.New("RUN_TIMEOUT")

// OpenModelProxy reserves one durable attempt and bounds both HTTP headers and
// blocking body reads. Cancellation polling never depends on provider bytes.
func (s *harnessProxy) OpenModelProxy(parent context.Context, p HarnessPrincipal, body []byte) (*ModelProxyStream, error) {
	run, err := s.lifecycle().activeRun(parent, p)
	if err != nil {
		return nil, s.proxyError(err)
	}
	if run.CancelRequested {
		return nil, &ProxyError{Code: "CANCELLED", Status: 409, Detail: "run cancelled", Fatal: true}
	}
	if limit := s.svc.limits.MaxTurns; limit > 0 && run.ModelCalls >= limit {
		if err := s.emitTurnLimitNotice(parent, run); err != nil {
			return nil, err
		}
		return nil, &ProxyError{Code: "MAX_TURNS", Status: 409, Detail: "turn budget exhausted", Fatal: true}
	}
	maxAttempts := s.svc.limits.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 10
	}
	if run.ModelAttempts >= maxAttempts {
		_ = s.lifecycle().finishRun(parent, run, persistence.RunFailed, "MAX_ATTEMPTS", "model attempt budget exhausted", protocol.ReasonMaxTurns)
		return nil, &ProxyError{Code: "MAX_ATTEMPTS", Status: 409, Detail: "model attempt budget exhausted", Fatal: true}
	}
	lease, err := s.repo().AcquireModelAttempt(parent, p.UserID, p.RunID, maxAttempts, s.svc.limits.MaxTurns, p.TokenID)
	if err != nil {
		return nil, &ProxyError{Code: "MODEL_ATTEMPT_ACTIVE", Status: 409, Detail: err.Error()}
	}
	ctx, cancel := context.WithCancelCause(parent)
	started := run.CreatedAt
	if run.StartedAt != nil {
		started = *run.StartedAt
	} else {
		started = s.clock()
	}
	remaining := s.lifecycle().runWallClock() - (s.clock().Sub(started) - time.Duration(run.ApprovalWaitMs)*time.Millisecond)
	activeTimer := time.AfterFunc(max(time.Millisecond, remaining), func() { cancel(errActiveTimeout) })
	header := s.svc.limits.HeaderTimeout
	if header <= 0 {
		header = 30 * time.Second
	}
	headerTimer := time.AfterFunc(header, func() { cancel(errHeaderTimeout) })
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				current, e := s.repo().GetRun(ctx, p.UserID, p.RunID)
				if e == nil && (current.CancelRequested || IsTerminalRunStatus(current.Status)) {
					cancel(errRunCancelled)
					return
				}
				if p.TokenID != "" {
					token, e := s.repo().GetHarnessToken(ctx, p.TokenID)
					if e == nil && !s.svc.harnessTokens.validTokenRow(token) {
						cancel(ErrHarnessToken)
						return
					}
				}
			}
		}
	}()
	var once sync.Once
	finish := func() {
		once.Do(func() {
			cause := context.Cause(ctx)
			headerTimer.Stop()
			activeTimer.Stop()
			cancel(context.Canceled)
			cleanup, done := context.WithTimeout(context.WithoutCancel(parent), 3*time.Second)
			defer done()
			_ = s.repo().ReleaseModelAttempt(cleanup, p.UserID, p.RunID, lease)
			if errors.Is(cause, errHeaderTimeout) || errors.Is(cause, errIdleTimeout) || errors.Is(cause, errActiveTimeout) {
				_ = s.lifecycle().finishRun(cleanup, run, persistence.RunFailed, cause.Error(), "model execution deadline exceeded", protocol.ReasonRunTimeout)
			}
			if errors.Is(cause, errRunCancelled) {
				current, e := s.repo().GetRun(cleanup, p.UserID, p.RunID)
				if e == nil && current.CancelRequested {
					_ = s.lifecycle().finishRun(cleanup, current, persistence.RunCancelled, "CANCELLED", "cancelled by user", protocol.ReasonCancelled)
				}
			}
		})
	}
	stream, err := s.openModelProxy(ctx, p, body)
	headerTimer.Stop()
	if err != nil {
		cause := context.Cause(ctx)
		finish()
		if cause != nil {
			return nil, &ProxyError{Code: cause.Error(), Status: http.StatusGatewayTimeout, Detail: "model request interrupted"}
		}
		return nil, err
	}
	idle := s.svc.limits.IdleTimeout
	if idle <= 0 {
		idle = 30 * time.Second
	}
	reader := &deadlineReader{inner: stream.Body, finish: finish}
	reader.timer = time.AfterFunc(idle, func() { cancel(errIdleTimeout) })
	reader.idle = idle
	stream.Body = reader
	return stream, nil
}

type deadlineReader struct {
	inner  io.ReadCloser
	timer  *time.Timer
	idle   time.Duration
	finish func()
	once   sync.Once
}

func (r *deadlineReader) Read(p []byte) (int, error) {
	n, err := r.inner.Read(p)
	if n > 0 {
		r.timer.Reset(r.idle)
	}
	if err != nil {
		_ = r.Close()
	}
	return n, err
}
func (r *deadlineReader) Close() error {
	var err error
	r.once.Do(func() { r.timer.Stop(); err = r.inner.Close(); r.finish() })
	return err
}
