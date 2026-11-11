package ctx

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	convAuth "github.com/sofmon/convention/lib/auth"
)

var requestStartClaims = convAuth.Claims{User: "svc-test"}

func Test_RequestElapsed_is_absent_on_a_context_that_was_never_stamped(t *testing.T) {
	d, ok := New(requestStartClaims).RequestElapsed()
	if ok || d != 0 {
		t.Errorf("RequestElapsed() = %v, %v; want 0, false", d, ok)
	}
}

func Test_RequestElapsed_counts_from_the_stamp_through_derived_contexts(t *testing.T) {
	const pause = 30 * time.Millisecond

	req := httptest.NewRequest(http.MethodGet, "https://svc/x", nil)

	tests := []struct {
		name   string
		derive func(Context) Context
	}{
		{"the stamped context itself", func(c Context) Context { return c }},
		{"a scope", func(c Context) Context { return c.WithScope("work", "k", "v") }},
		{"claims", func(c Context) Context { return c.WithClaims(convAuth.Claims{User: "other"}) }},
		{"agent claims", func(c Context) Context { return c.WithAgentClaims() }},
		{"a logger", func(c Context) Context { return c.WithLogger(slog.New(slog.NewJSONHandler(io.Discard, nil))) }},
		{"an attached request", func(c Context) Context { return c.WithRequest(req, false) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := New(requestStartClaims).WithRequestStart()
			time.Sleep(pause)

			d, ok := tt.derive(ctx).RequestElapsed()
			if !ok || d < pause {
				t.Errorf("RequestElapsed() = %v, %v; want at least %v, true", d, ok, pause)
			}
		})
	}
}

func Test_RequestElapsed_survives_attaching_a_request_again(t *testing.T) {
	const pause = 20 * time.Millisecond

	ctx := New(requestStartClaims).WithRequestStart()
	arrival := ctx.Value(contextKeyRequestStart).(requestStamp).start

	ctx = ctx.WithRequest(httptest.NewRequest(http.MethodGet, "https://svc/first", nil), false)
	time.Sleep(pause)
	ctx = ctx.WithRequest(httptest.NewRequest(http.MethodGet, "https://svc/second", nil), false)
	time.Sleep(pause)

	if got := ctx.Value(contextKeyRequestStart).(requestStamp).start; !got.Equal(arrival) {
		t.Errorf("the arrival moved from %v to %v when a request was attached again", arrival, got)
	}
	d, ok := ctx.RequestElapsed()
	if !ok || d < 2*pause {
		t.Errorf("RequestElapsed() = %v, %v; want at least %v (the first arrival), true", d, ok, 2*pause)
	}
}

func Test_RequestElapsed_reads_the_clock_captured_at_the_stamp(t *testing.T) {
	base := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name   string
		derive func(Context) Context
	}{
		{"unchanged", func(c Context) Context { return c }},
		{"a later business clock", func(c Context) Context { return c.WithNow(base.Add(time.Hour)) }},
		{"an earlier business clock", func(c Context) Context { return c.WithNow(base.Add(-time.Hour)) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := New(requestStartClaims).WithNow(base).WithRequestStart()
			time.Sleep(5 * time.Millisecond)

			d, ok := tt.derive(ctx).RequestElapsed()
			if !ok || d != 0 {
				t.Errorf("RequestElapsed() = %v, %v; want exactly 0, true (the stamp's own clock is frozen)", d, ok)
			}
		})
	}
}

func Test_RequestElapsed_ignores_the_Time_Now_header(t *testing.T) {
	base := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	header := base.Add(2 * time.Hour)

	// Without configuration a context is production, which ignores the header.
	ctx := New(requestStartClaims)
	ctx.Context = context.WithValue(ctx.Context, contextKeyEnv, Environment("development"))
	ctx = ctx.WithNow(base).WithRequestStart()

	req := httptest.NewRequest(http.MethodGet, "https://svc/x", nil)
	req.Header.Set(HTTPHeaderTimeNow, header.Format(time.RFC3339))
	ctx = ctx.WithRequest(req, false)

	if got := ctx.Now(); !got.Equal(header) {
		t.Fatalf("the header did not re-clock the request context: Now() = %v, want %v", got, header)
	}

	d, ok := ctx.RequestElapsed()
	if !ok || d != 0 {
		t.Errorf("RequestElapsed() = %v, %v; want exactly 0, true", d, ok)
	}
}
