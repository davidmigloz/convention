package lifecycle

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	convAuth "github.com/sofmon/convention/lib/auth"
	convCtx "github.com/sofmon/convention/lib/ctx"
)

func TestShutdown(t *testing.T) {
	sentinel := errors.New("sentinel stage failure")

	tests := []struct {
		name       string
		stages     []Stage
		wantErr    error
		wantParts  []string
		wantNoPart string
	}{
		{
			name: "all stages succeed",
			stages: []Stage{
				{Name: "first", Fn: func(convCtx.Context) error { return nil }},
				{Name: "second", Fn: func(convCtx.Context) error { return nil }},
			},
		},
		{
			name: "stage errors retain declaration order",
			stages: []Stage{
				{Name: "first", Fn: func(convCtx.Context) error { return sentinel }},
				{Name: "second", Fn: func(convCtx.Context) error { return errors.New("second failure") }},
			},
			wantErr:   sentinel,
			wantParts: []string{"first: sentinel stage failure", "second: second failure"},
		},
		{
			name:       "no stages succeeds",
			wantNoPart: "deadline",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := shutdown(testContext(), time.Second, tt.stages)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("shutdown() error = %v, want errors.Is(_, %v)", err, tt.wantErr)
			}
			lastIndex := -1
			for _, part := range tt.wantParts {
				index := strings.Index(err.Error(), part)
				if index < 0 {
					t.Fatalf("shutdown() error = %q, want part %q", err, part)
				}
				if index <= lastIndex {
					t.Fatalf("shutdown() error = %q, want parts in declaration order", err)
				}
				lastIndex = index
			}
			if tt.wantNoPart != "" && err != nil && strings.Contains(err.Error(), tt.wantNoPart) {
				t.Fatalf("shutdown() error = %q, do not want %q", err, tt.wantNoPart)
			}
		})
	}
}

func TestShutdownStopsContextAwareStageAtDeadline(t *testing.T) {
	started := time.Now()
	err := shutdown(testContext(), 25*time.Millisecond, []Stage{{
		Name: "context aware",
		Fn: func(ctx convCtx.Context) error {
			<-ctx.Done()
			return ctx.Err()
		},
	}})

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown() error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed < 15*time.Millisecond || elapsed >= time.Second {
		t.Fatalf("shutdown() elapsed = %v, want bounded deadline", elapsed)
	}
}

func TestRunReturnsWhenStageIgnoresContextAndGoroutinesCanFinish(t *testing.T) {
	baseline := runtime.NumGoroutine()
	stageStarted := make(chan struct{})
	releaseStage := make(chan struct{})
	listenerStarted := make(chan struct{})
	releaseListener := make(chan struct{})
	signals := make(chan os.Signal, 1)
	result := make(chan error, 1)

	go func() {
		result <- run(testContext(), Config{
			ListenAndServe: func(convCtx.Context) error {
				close(listenerStarted)
				<-releaseListener
				return nil
			},
			ShutdownTimeout: 25 * time.Millisecond,
			Stages: []Stage{{
				Name: "context ignoring",
				Fn: func(convCtx.Context) error {
					close(stageStarted)
					<-releaseStage
					return nil
				},
			}},
			OnSignalShutdown: func(convCtx.Context, error) {
				close(releaseListener)
			},
		}, signals)
	}()

	<-listenerStarted
	signals <- syscall.SIGTERM
	<-stageStarted
	err := <-result
	if err != nil {
		t.Fatalf("run() error = %v", err)
	}
	close(releaseStage)

	deadline := time.Now().Add(time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > baseline {
		t.Fatalf("goroutines after release = %d, want at most baseline %d", got, baseline)
	}
}

func TestRunSignalCallbacksAreOrderedAndExactlyOnce(t *testing.T) {
	listenerStarted := make(chan struct{})
	releaseListener := make(chan struct{})
	signals := make(chan os.Signal, 1)
	var callbacks []string

	result := make(chan error, 1)
	go func() {
		result <- run(testContext(), Config{
			ListenAndServe: func(convCtx.Context) error {
				close(listenerStarted)
				<-releaseListener
				return nil
			},
			ShutdownTimeout: time.Second,
			Stages: []Stage{{Name: "cleanup", Fn: func(convCtx.Context) error {
				callbacks = append(callbacks, "stage")
				return nil
			}}},
			OnSignal: func(convCtx.Context) {
				callbacks = append(callbacks, "signal")
			},
			OnSignalShutdown: func(_ convCtx.Context, err error) {
				if err != nil {
					t.Errorf("OnSignalShutdown() error = %v", err)
				}
				callbacks = append(callbacks, "shutdown")
				close(releaseListener)
			},
		}, signals)
	}()

	<-listenerStarted
	signals <- syscall.SIGTERM
	if err := <-result; err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if got, want := strings.Join(callbacks, ","), "signal,stage,shutdown"; got != want {
		t.Fatalf("callback order = %q, want %q", got, want)
	}
}

func TestRunListenerCompletionJoinsErrorsWithoutSignalCallbacks(t *testing.T) {
	listenerErr := errors.New("listener failed")
	stageErr := errors.New("cleanup failed")
	var callbackCalls atomic.Int32

	err := run(testContext(), Config{
		ListenAndServe:  func(convCtx.Context) error { return listenerErr },
		ShutdownTimeout: time.Second,
		Stages: []Stage{{
			Name: "cleanup",
			Fn:   func(convCtx.Context) error { return stageErr },
		}},
		OnSignal: func(convCtx.Context) { callbackCalls.Add(1) },
		OnSignalShutdown: func(convCtx.Context, error) {
			callbackCalls.Add(1)
		},
	}, make(chan os.Signal))

	if !errors.Is(err, listenerErr) || !errors.Is(err, stageErr) {
		t.Fatalf("run() error = %v, want listener and stage errors", err)
	}
	if got := callbackCalls.Load(); got != 0 {
		t.Fatalf("signal callback calls = %d, want 0", got)
	}
}

func TestRunValidatesConfigBeforeStartingListener(t *testing.T) {
	tests := []struct {
		name   string
		config Config
		want   string
	}{
		{name: "missing listener", config: Config{ShutdownTimeout: time.Second}, want: "ListenAndServe is required"},
		{name: "zero timeout", config: Config{ListenAndServe: func(convCtx.Context) error { return nil }}, want: "ShutdownTimeout must be positive"},
		{name: "unnamed stage", config: Config{ListenAndServe: func(convCtx.Context) error { return nil }, ShutdownTimeout: time.Second, Stages: []Stage{{Fn: func(convCtx.Context) error { return nil }}}}, want: "stage 0 has no name"},
		{name: "missing stage function", config: Config{ListenAndServe: func(convCtx.Context) error { return nil }, ShutdownTimeout: time.Second, Stages: []Stage{{Name: "cleanup"}}}, want: "stage \"cleanup\" has no function"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Run(testContext(), tt.config)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Run() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestRunDrainsHTTPServerBeforeReturning(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	handlerStarted := make(chan struct{})
	releaseHandler := make(chan struct{})
	server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		close(handlerStarted)
		<-releaseHandler
		writer.WriteHeader(http.StatusNoContent)
	})}
	signals := make(chan os.Signal, 1)
	runResult := make(chan error, 1)
	go func() {
		runResult <- run(testContext(), Config{
			ListenAndServe: func(convCtx.Context) error {
				err := server.Serve(listener)
				if errors.Is(err, http.ErrServerClosed) {
					return nil
				}
				return err
			},
			ShutdownTimeout: time.Second,
			Stages: []Stage{{Name: "drain http server", Fn: func(ctx convCtx.Context) error {
				return server.Shutdown(ctx.Context)
			}}},
		}, signals)
	}()

	requestResult := make(chan error, 1)
	go func() {
		response, err := http.Get("http://" + listener.Addr().String())
		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			err = response.Body.Close()
		}
		requestResult <- err
	}()

	<-handlerStarted
	signals <- syscall.SIGTERM
	select {
	case err := <-runResult:
		t.Fatalf("run() returned before held request completed: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(releaseHandler)

	if err := <-requestResult; err != nil {
		t.Fatalf("held request error = %v", err)
	}
	if err := <-runResult; err != nil {
		t.Fatalf("run() error = %v", err)
	}
}

func testContext() convCtx.Context {
	return convCtx.New(convAuth.Claims{User: "lifecycle-test"})
}
