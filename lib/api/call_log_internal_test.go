package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	convAuth "github.com/sofmon/convention/lib/auth"
	convCtx "github.com/sofmon/convention/lib/ctx"
)

type callLogEndpoints struct {
	Slow Raw `api:"GET /test/v1/slow"`
}

type slowResponseWriter struct {
	http.ResponseWriter
	delay time.Duration
}

func (w slowResponseWriter) Write(p []byte) (int, error) {
	time.Sleep(w.delay)
	return w.ResponseWriter.Write(p)
}

func Test_ServeHTTP_logs_duration_ms_on_the_API_call_line(t *testing.T) {
	const body = "ok"

	tests := []struct {
		name       string
		checkPause time.Duration
		pause      time.Duration
		copyPause  time.Duration
		frozenAt   time.Time
		wantExact  bool
		want       time.Duration
	}{
		{name: "an instant endpoint"},
		{name: "a slow endpoint", pause: 40 * time.Millisecond, want: 40 * time.Millisecond},
		{name: "a slower endpoint", pause: 120 * time.Millisecond, want: 120 * time.Millisecond},
		{name: "a slow access check", checkPause: 40 * time.Millisecond, want: 40 * time.Millisecond},
		{name: "a slow copy to the client", copyPause: 40 * time.Millisecond, want: 40 * time.Millisecond},
		{
			name:      "a frozen base clock",
			pause:     30 * time.Millisecond,
			frozenAt:  time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC),
			wantExact: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			ctx := convCtx.New(convAuth.Claims{User: "svc-test"}).
				WithLogger(slog.New(slog.NewJSONHandler(&logs, nil)))
			if !tt.frozenAt.IsZero() {
				ctx = ctx.WithNow(tt.frozenAt)
			}

			api := &callLogEndpoints{
				Slow: NewRaw(func(_ convCtx.Context, w http.ResponseWriter, _ *http.Request) {
					time.Sleep(tt.pause)
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, body)
				}),
			}
			check := func(*http.Request) (convAuth.Target, error) {
				time.Sleep(tt.checkPause)
				var target convAuth.Target
				return target, nil
			}
			srv := &server{httpServer: &http.Server{Handler: NewHandler(ctx, "localhost", 0, check, api)}}
			srv.EnableCallsLogging()

			rec := httptest.NewRecorder()
			srv.httpServer.Handler.ServeHTTP(
				slowResponseWriter{ResponseWriter: rec, delay: tt.copyPause},
				httptest.NewRequest(http.MethodGet, "https://svc/test/v1/slow", nil),
			)

			if rec.Code != http.StatusOK || rec.Body.String() != body {
				t.Fatalf("client got %d %q, want 200 %q", rec.Code, rec.Body.String(), body)
			}

			lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
			if len(lines) != 1 {
				t.Fatalf("logged %d lines, want 1:\n%s", len(lines), logs.String())
			}
			var line map[string]any
			if err := json.Unmarshal([]byte(lines[0]), &line); err != nil {
				t.Fatalf("decoding the log line: %v\n%s", err, lines[0])
			}

			if line["msg"] != "API call" {
				t.Errorf("msg = %v, want \"API call\"", line["msg"])
			}
			for _, group := range []string{"request", "response"} {
				if _, ok := line[group].(map[string]any); !ok {
					t.Errorf("the %q group is missing or not an object: %v", group, line[group])
				}
			}

			ms, ok := line["duration_ms"].(float64)
			if !ok {
				t.Fatalf("duration_ms = %#v, want a JSON number", line["duration_ms"])
			}
			if ms != float64(int64(ms)) {
				t.Errorf("duration_ms = %v, want a whole number of milliseconds", ms)
			}
			got := time.Duration(ms) * time.Millisecond
			if tt.wantExact && got != tt.want {
				t.Errorf("duration_ms = %v, want exactly %v", got, tt.want)
			}
			if !tt.wantExact && got < tt.want {
				t.Errorf("duration_ms = %v, want at least %v", got, tt.want)
			}
		})
	}
}
