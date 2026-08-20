package lifecycle

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
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
		phases     [][]Stage
		wantErr    error
		wantParts  []string
		wantNoPart string
	}{
		{
			name: "all stages succeed",
			phases: [][]Stage{{
				{Name: "first", Fn: func(convCtx.Context) error { return nil }},
				{Name: "second", Fn: func(convCtx.Context) error { return nil }},
			}},
		},
		{
			name: "stage errors retain declaration order",
			phases: [][]Stage{{
				{Name: "first", Fn: func(convCtx.Context) error { return sentinel }},
				{Name: "second", Fn: func(convCtx.Context) error { return errors.New("second failure") }},
			}},
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
			err := shutdown(testContext(), time.Second, tt.phases)
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
	missingDeadline := errors.New("stage context has no deadline")
	started := time.Now()
	err := shutdown(testContext(), 25*time.Millisecond, [][]Stage{{{
		Name: "context aware",
		Fn: func(ctx convCtx.Context) error {
			if _, ok := ctx.Deadline(); !ok {
				return missingDeadline
			}
			<-ctx.Done()
			return ctx.Err()
		},
	}}})

	if errors.Is(err, missingDeadline) {
		t.Fatalf("shutdown() error = %v, stage context must carry the deadline", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown() error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed < 15*time.Millisecond || elapsed >= time.Second {
		t.Fatalf("shutdown() elapsed = %v, want bounded deadline", elapsed)
	}
}

func TestShutdownIgnoresCancelledParentDuringCleanup(t *testing.T) {
	ctx := testContext()
	parent, cancel := context.WithCancel(ctx.Context)
	cancel()
	ctx.Context = parent

	stageStarted := make(chan struct{})
	stageFinished := make(chan struct{})
	stageContextErr := make(chan error, 1)
	releaseStage := make(chan struct{})
	releaseStageOnce := sync.OnceFunc(func() { close(releaseStage) })
	t.Cleanup(releaseStageOnce)
	result := make(chan error, 1)

	go func() {
		result <- shutdown(ctx, time.Second, [][]Stage{{{
			Name: "cleanup",
			Fn: func(ctx convCtx.Context) error {
				defer close(stageFinished)
				stageContextErr <- ctx.Err()
				close(stageStarted)
				<-releaseStage
				return nil
			},
		}}})
	}()

	select {
	case <-stageStarted:
	case <-time.After(time.Second):
		t.Fatal("shutdown stage did not start")
	}

	var earlyErr error
	returnedEarly := false
	select {
	case earlyErr = <-result:
		returnedEarly = true
	case <-time.After(25 * time.Millisecond):
	}

	ctxErr := <-stageContextErr
	releaseStageOnce()
	<-stageFinished
	if returnedEarly {
		t.Fatalf("shutdown() returned before cleanup was released with error %v", earlyErr)
	}
	if err := <-result; err != nil {
		t.Fatalf("shutdown() error = %v, want nil", err)
	}
	if ctxErr != nil {
		t.Fatalf("shutdown stage context error at entry = %v, want nil", ctxErr)
	}
}

func TestShutdownConvertsStagePanicToNamedError(t *testing.T) {
	const helperEnv = "CONVENTION_LIFECYCLE_PANIC_STAGE_HELPER"
	if os.Getenv(helperEnv) == "1" {
		err := shutdown(testContext(), time.Second, [][]Stage{{{
			Name: "flush write-ahead log",
			Fn: func(convCtx.Context) error {
				panic("corrupt buffer")
			},
		}}})
		if err == nil || !strings.Contains(err.Error(), "flush write-ahead log") || !strings.Contains(err.Error(), "corrupt buffer") {
			t.Fatalf("shutdown() error = %v, want named stage panic", err)
		}
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestShutdownConvertsStagePanicToNamedError$")
	command.Env = append(os.Environ(), helperEnv+"=1")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("panic-stage helper failed: %v\n%s", err, output)
	}
}

func TestShutdownCompletesDrainBeforeClosingDependency(t *testing.T) {
	drainStarted := make(chan struct{})
	drainCompleted := make(chan struct{})
	dependencyChecked := make(chan struct{})
	releaseDrain := make(chan struct{})
	releaseDrainOnce := sync.OnceFunc(func() { close(releaseDrain) })
	t.Cleanup(releaseDrainOnce)

	go func() {
		select {
		case <-dependencyChecked:
		case <-time.After(25 * time.Millisecond):
		}
		releaseDrainOnce()
	}()

	err := shutdown(testContext(), time.Second, [][]Stage{
		{{
			Name: "drain http server",
			Fn: func(convCtx.Context) error {
				close(drainStarted)
				<-releaseDrain
				close(drainCompleted)
				return nil
			},
		}},
		{{
			Name: "close dependency",
			Fn: func(convCtx.Context) error {
				<-drainStarted
				defer close(dependencyChecked)
				select {
				case <-drainCompleted:
					return nil
				default:
					return errors.New("dependency closed before HTTP drain completed")
				}
			},
		}},
	})

	if err != nil {
		t.Fatalf("shutdown() error = %v, want ordered cleanup", err)
	}
}

func TestShutdownRunsStagesWithinPhaseConcurrently(t *testing.T) {
	firstStarted := make(chan struct{})
	secondStarted := make(chan struct{})
	releaseStages := make(chan struct{})
	releaseStagesOnce := sync.OnceFunc(func() { close(releaseStages) })
	t.Cleanup(releaseStagesOnce)
	result := make(chan error, 1)

	go func() {
		result <- shutdown(testContext(), time.Second, [][]Stage{{
			{
				Name: "first",
				Fn: func(convCtx.Context) error {
					close(firstStarted)
					<-releaseStages
					return nil
				},
			},
			{
				Name: "second",
				Fn: func(convCtx.Context) error {
					close(secondStarted)
					<-releaseStages
					return nil
				},
			},
		}})
	}()

	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first stage did not start")
	}
	select {
	case <-secondStarted:
	case <-time.After(time.Second):
		t.Fatal("second stage did not start while first stage was blocked")
	}
	releaseStagesOnce()
	if err := <-result; err != nil {
		t.Fatalf("shutdown() error = %v, want nil", err)
	}
}

func TestShutdownDoesNotStartLaterPhasesAfterDeadline(t *testing.T) {
	releaseStage := make(chan struct{})
	releaseStageOnce := sync.OnceFunc(func() { close(releaseStage) })
	t.Cleanup(releaseStageOnce)
	stageFinished := make(chan struct{})
	var laterStageCalls atomic.Int32

	err := shutdown(testContext(), 25*time.Millisecond, [][]Stage{
		{{
			Name: "drain http server",
			Fn: func(convCtx.Context) error {
				defer close(stageFinished)
				<-releaseStage
				return nil
			},
		}},
		{{
			Name: "close database",
			Fn: func(convCtx.Context) error {
				laterStageCalls.Add(1)
				return nil
			},
		}},
	})

	releaseStageOnce()
	<-stageFinished
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown() error = %v, want context deadline exceeded", err)
	}
	if got := laterStageCalls.Load(); got != 0 {
		t.Fatalf("later phase calls = %d, want 0 after deadline", got)
	}
	first := strings.Index(err.Error(), "drain http server: context deadline exceeded")
	second := strings.Index(err.Error(), "close database: context deadline exceeded")
	if first < 0 || second <= first {
		t.Fatalf("shutdown() error = %q, want skipped stages in declaration order", err)
	}
}

func TestRunReturnsWhenStageIgnoresContextAndGoroutinesCanFinish(t *testing.T) {
	stageStarted := make(chan struct{})
	stageFinished := make(chan struct{})
	releaseStage := make(chan struct{})
	releaseStageOnce := sync.OnceFunc(func() { close(releaseStage) })
	t.Cleanup(releaseStageOnce)
	listenerStarted := make(chan struct{})
	listenerFinished := make(chan struct{})
	releaseListener := make(chan struct{})
	releaseListenerOnce := sync.OnceFunc(func() { close(releaseListener) })
	t.Cleanup(releaseListenerOnce)
	signals := make(chan os.Signal, 1)
	result := make(chan error, 1)

	go func() {
		result <- run(testContext(), Config{
			ListenAndServe: func(convCtx.Context) error {
				defer close(listenerFinished)
				close(listenerStarted)
				<-releaseListener
				return nil
			},
			ShutdownTimeout: 25 * time.Millisecond,
			Stages: [][]Stage{{{
				Name: "context ignoring",
				Fn: func(convCtx.Context) error {
					defer close(stageFinished)
					close(stageStarted)
					<-releaseStage
					return nil
				},
			}}},
			OnSignalShutdown: func(convCtx.Context, error) {
				releaseListenerOnce()
			},
		}, signals)
	}()

	<-listenerStarted
	signals <- syscall.SIGTERM
	<-stageStarted
	err := <-result
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("run() error = %v, want context deadline exceeded", err)
	}
	releaseStageOnce()
	select {
	case <-stageFinished:
	case <-time.After(time.Second):
		t.Fatal("context-ignoring stage did not finish after release")
	}
	select {
	case <-listenerFinished:
	case <-time.After(time.Second):
		t.Fatal("listener did not finish after release")
	}
}

func TestRunSignalCallbacksAreOrderedAndExactlyOnce(t *testing.T) {
	listenerStarted := make(chan struct{})
	listenerFinished := make(chan struct{})
	releaseListener := make(chan struct{})
	releaseListenerOnce := sync.OnceFunc(func() { close(releaseListener) })
	t.Cleanup(releaseListenerOnce)
	signals := make(chan os.Signal, 1)
	var callbacks []string

	result := make(chan error, 1)
	go func() {
		result <- run(testContext(), Config{
			ListenAndServe: func(convCtx.Context) error {
				defer close(listenerFinished)
				close(listenerStarted)
				<-releaseListener
				return nil
			},
			ShutdownTimeout: time.Second,
			Stages: [][]Stage{{{Name: "cleanup", Fn: func(convCtx.Context) error {
				callbacks = append(callbacks, "stage")
				return nil
			}}}},
			OnSignal: func(convCtx.Context) {
				callbacks = append(callbacks, "signal")
			},
			OnSignalShutdown: func(_ convCtx.Context, err error) {
				if err != nil {
					t.Errorf("OnSignalShutdown() error = %v", err)
				}
				callbacks = append(callbacks, "shutdown")
				releaseListenerOnce()
			},
		}, signals)
	}()

	<-listenerStarted
	signals <- syscall.SIGTERM
	if err := <-result; err != nil {
		t.Fatalf("run() error = %v", err)
	}
	<-listenerFinished
	if got, want := strings.Join(callbacks, ","), "signal,stage,shutdown"; got != want {
		t.Fatalf("callback order = %q, want %q", got, want)
	}
}

func TestRunReturnsShutdownErrorOnSignal(t *testing.T) {
	sentinel := errors.New("cleanup failed")
	listenerStarted := make(chan struct{})
	listenerFinished := make(chan struct{})
	releaseListener := make(chan struct{})
	releaseListenerOnce := sync.OnceFunc(func() { close(releaseListener) })
	t.Cleanup(releaseListenerOnce)
	signals := make(chan os.Signal, 1)
	result := make(chan error, 1)
	callbackResult := make(chan error, 1)

	go func() {
		result <- run(testContext(), Config{
			ListenAndServe: func(convCtx.Context) error {
				defer close(listenerFinished)
				close(listenerStarted)
				<-releaseListener
				return nil
			},
			ShutdownTimeout: time.Second,
			Stages: [][]Stage{{{
				Name: "cleanup",
				Fn:   func(convCtx.Context) error { return sentinel },
			}}},
			OnSignalShutdown: func(_ convCtx.Context, err error) {
				callbackResult <- err
			},
		}, signals)
	}()

	<-listenerStarted
	signals <- syscall.SIGTERM
	err := <-result
	releaseListenerOnce()
	<-listenerFinished
	if !errors.Is(err, sentinel) {
		t.Fatalf("run() error = %v, want shutdown error %v", err, sentinel)
	}
	if callbackErr := <-callbackResult; !errors.Is(callbackErr, sentinel) {
		t.Fatalf("OnSignalShutdown() error = %v, want shutdown error %v", callbackErr, sentinel)
	}
}

func TestRunRegistersSIGTERM(t *testing.T) {
	listenerStarted := make(chan struct{})
	listenerFinished := make(chan struct{})
	releaseListener := make(chan struct{})
	releaseListenerOnce := sync.OnceFunc(func() { close(releaseListener) })
	t.Cleanup(releaseListenerOnce)
	stageRan := make(chan struct{})
	result := make(chan error, 1)

	go func() {
		result <- Run(testContext(), Config{
			ListenAndServe: func(convCtx.Context) error {
				defer close(listenerFinished)
				close(listenerStarted)
				<-releaseListener
				return nil
			},
			ShutdownTimeout: time.Second,
			Stages: [][]Stage{{{
				Name: "cleanup",
				Fn: func(convCtx.Context) error {
					close(stageRan)
					releaseListenerOnce()
					return nil
				},
			}}},
		})
	}()

	<-listenerStarted
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM: %v", err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run() did not return after SIGTERM")
	}
	<-listenerFinished
	select {
	case <-stageRan:
	default:
		t.Fatal("shutdown stage did not run after SIGTERM")
	}
}

func TestRunSecondSIGTERMRestoresDefaultTermination(t *testing.T) {
	const (
		helperEnv    = "CONVENTION_LIFECYCLE_SECOND_SIGTERM_HELPER"
		stageStarted = "shutdown-stage-started"
	)
	if os.Getenv(helperEnv) == "1" {
		err := Run(testContext(), Config{
			ListenAndServe: func(convCtx.Context) error {
				if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
					return err
				}
				select {}
			},
			ShutdownTimeout: 10 * time.Second,
			Stages: [][]Stage{{{
				Name: "context-ignoring cleanup",
				Fn: func(convCtx.Context) error {
					_, _ = os.Stdout.WriteString(stageStarted + "\n")
					if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
						return err
					}
					select {}
				},
			}}},
		})
		t.Fatalf("Run() returned after second SIGTERM with error %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRunSecondSIGTERMRestoresDefaultTermination$")
	command.Env = append(os.Environ(), helperEnv+"=1")
	output, err := command.CombinedOutput()
	if !strings.Contains(string(output), stageStarted) {
		t.Fatalf("helper output = %q, want shutdown stage marker", output)
	}
	if ctx.Err() != nil {
		t.Fatalf("second SIGTERM did not terminate blocked shutdown promptly: %v", ctx.Err())
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("helper error = %v, want SIGTERM exit", err)
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGTERM {
		t.Fatalf("helper exit status = %v, want SIGTERM", exitErr.Sys())
	}
}

func TestRunListenerCompletionJoinsErrorsWithoutSignalCallbacks(t *testing.T) {
	listenerErr := errors.New("listener failed")
	stageErr := errors.New("cleanup failed")
	var callbackCalls atomic.Int32

	err := run(testContext(), Config{
		ListenAndServe:  func(convCtx.Context) error { return listenerErr },
		ShutdownTimeout: time.Second,
		Stages: [][]Stage{{{
			Name: "cleanup",
			Fn:   func(convCtx.Context) error { return stageErr },
		}}},
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
		{name: "no phases", config: Config{ListenAndServe: func(convCtx.Context) error { return nil }, ShutdownTimeout: time.Second}, want: "at least one shutdown phase is required"},
		{name: "empty phase", config: Config{ListenAndServe: func(convCtx.Context) error { return nil }, ShutdownTimeout: time.Second, Stages: [][]Stage{{}}}, want: "shutdown phase 0 is empty"},
		{name: "unnamed stage", config: Config{ListenAndServe: func(convCtx.Context) error { return nil }, ShutdownTimeout: time.Second, Stages: [][]Stage{{{Fn: func(convCtx.Context) error { return nil }}}}}, want: "stage 0 has no name"},
		{name: "missing stage function", config: Config{ListenAndServe: func(convCtx.Context) error { return nil }, ShutdownTimeout: time.Second, Stages: [][]Stage{{{Name: "cleanup"}}}}, want: "stage \"cleanup\" has no function"},
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

func TestRunRejectsZeroValueContextBeforeStartingListener(t *testing.T) {
	var listenerCalls atomic.Int32
	config := Config{
		ListenAndServe: func(convCtx.Context) error {
			listenerCalls.Add(1)
			return nil
		},
		ShutdownTimeout: time.Second,
		Stages: [][]Stage{{{
			Name: "cleanup",
			Fn:   func(convCtx.Context) error { return nil },
		}}},
	}

	var err error
	var panicValue any
	func() {
		defer func() {
			panicValue = recover()
		}()
		err = Run(convCtx.Context{}, config)
	}()

	if panicValue != nil {
		t.Fatalf("Run() panicked for a zero-value context: %v", panicValue)
	}
	if err == nil {
		t.Fatal("Run() error = nil, want invalid context error")
	}
	if got := listenerCalls.Load(); got != 0 {
		t.Fatalf("listener calls = %d, want validation before listener start", got)
	}
}

func TestRunRejectsDuplicateStageNamesBeforeStartingListener(t *testing.T) {
	var listenerCalls atomic.Int32
	err := Run(testContext(), Config{
		ListenAndServe: func(convCtx.Context) error {
			listenerCalls.Add(1)
			return nil
		},
		ShutdownTimeout: time.Second,
		Stages: [][]Stage{{
			{Name: "close dependency", Fn: func(convCtx.Context) error { return nil }},
			{Name: "close dependency", Fn: func(convCtx.Context) error { return nil }},
		}},
	})

	if err == nil {
		t.Fatal("Run() error = nil, want duplicate stage name error")
	}
	if got := listenerCalls.Load(); got != 0 {
		t.Fatalf("listener calls = %d, want validation before listener start", got)
	}
}

func TestRunRejectsNoShutdownStagesBeforeStartingListener(t *testing.T) {
	var listenerCalls atomic.Int32
	err := Run(testContext(), Config{
		ListenAndServe: func(convCtx.Context) error {
			listenerCalls.Add(1)
			return nil
		},
		ShutdownTimeout: time.Second,
	})

	if err == nil {
		t.Fatal("Run() error = nil, want missing shutdown stages error")
	}
	if got := listenerCalls.Load(); got != 0 {
		t.Fatalf("listener calls = %d, want validation before listener start", got)
	}
}

func TestRunDrainsHTTPServerBeforeReturning(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	handlerStarted := make(chan struct{})
	releaseHandler := make(chan struct{})
	releaseHandlerOnce := sync.OnceFunc(func() { close(releaseHandler) })
	t.Cleanup(releaseHandlerOnce)
	server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		close(handlerStarted)
		<-releaseHandler
		writer.WriteHeader(http.StatusNoContent)
	})}
	t.Cleanup(func() { _ = server.Close() })
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
			Stages: [][]Stage{{{Name: "drain http server", Fn: func(ctx convCtx.Context) error {
				return server.Shutdown(ctx.Context)
			}}}},
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
	releaseHandlerOnce()

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
