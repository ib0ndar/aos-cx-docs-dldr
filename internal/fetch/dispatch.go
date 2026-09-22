package fetch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"sync"
)

// DispatchRoundTripper defers the shared start-rate check until any backend
// admission wait has completed, without moving policy into the backend.
type DispatchRoundTripper interface {
	RoundTripWithDispatch(*http.Request, func() error) (*http.Response, error)
}

func DispatchRoundTrip(backend http.RoundTripper, request *http.Request, start func() error) (*http.Response, error) {
	if backend, ok := backend.(DispatchRoundTripper); ok {
		return backend.RoundTripWithDispatch(request, start)
	}
	if err := start(); err != nil {
		return nil, err
	}
	return backend.RoundTrip(request)
}

type dispatchKey struct{}

type scheduledBackend struct {
	backend  http.RoundTripper
	attempts *AttemptBudget
}

func (s scheduledBackend) RoundTrip(r *http.Request) (*http.Response, error) {
	schedule, ok := r.Context().Value(dispatchKey{}).(func() error)
	if !ok {
		return nil, errors.New("missing request dispatch policy")
	}
	diagnostic := traceFromContext(r.Context())
	diagnostic.update(StageLease, r.URL.String())
	var once sync.Once
	var err error
	var permit *attemptPermit
	start := func() error {
		once.Do(func() {
			permit, err = s.attempts.begin()
			if err == nil {
				diagnostic.update(StagePacing, r.URL.String())
				err = schedule()
			}
			if err != nil {
				permit.finish()
				permit = nil
			} else {
				diagnostic.update(StageHeaders, r.URL.String())
			}
		})
		return err
	}
	requestContext, cancel := context.WithCancelCause(r.Context())
	trace := &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) {
			if permit != nil {
				if err := permit.startAttempt(); err != nil {
					cancel(err)
				}
			}
		},
		WroteHeaders: func() {
			if permit != nil {
				permit.wroteHeaders()
			}
		},
	}
	r = r.Clone(httptrace.WithClientTrace(requestContext, trace))
	response, roundTripErr := DispatchRoundTrip(s.backend, r, start)
	if roundTripErr == nil && response != nil {
		if err := permit.requireObservedAttempt(); err != nil {
			cancel(err)
		}
	}
	if cause := context.Cause(requestContext); cause != nil && roundTripErr == nil {
		if response != nil && response.Body != nil {
			roundTripErr = errors.Join(cause, response.Body.Close())
			response = nil
		} else {
			roundTripErr = cause
		}
	}
	if roundTripErr != nil || response == nil || response.Body == nil {
		cancel(roundTripErr)
		permit.finish()
		return response, roundTripErr
	}
	response.Body = &attemptBudgetBody{ReadCloser: response.Body, permit: permit, cancel: cancel}
	return response, roundTripErr
}

type attemptBudgetBody struct {
	io.ReadCloser
	permit *attemptPermit
	cancel context.CancelCauseFunc
}

func (b *attemptBudgetBody) Close() error {
	err := b.ReadCloser.Close()
	b.permit.finish()
	b.cancel(err)
	return err
}

func (s scheduledBackend) CloseIdleConnections() {
	if closer, ok := s.backend.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}
