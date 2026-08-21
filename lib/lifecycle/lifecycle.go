// Package lifecycle coordinates signal-driven service shutdown.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	convCtx "github.com/sofmon/convention/lib/ctx"
)

// Stage is one named unit of shutdown work.
type Stage struct {
	Name string
	Fn   func(convCtx.Context) error
}

// Config defines a service listener and its bounded shutdown behavior.
// OnSignal and OnSignalShutdown run synchronously only on the signal path.
// They are not called when the listener exits, and they must return promptly.
type Config struct {
	ListenAndServe   func(convCtx.Context) error
	ShutdownTimeout  time.Duration
	Stages           [][]Stage
	OnSignal         func(convCtx.Context)
	OnSignalShutdown func(convCtx.Context, error)
}

// Run serves until SIGINT, SIGTERM, or listener completion, then executes all
// shutdown phases in order within one deadline. Stages in the same phase run
// concurrently.
//
// A stage that ignores its context may continue after Run returns. Call Run
// only on a terminal process-shutdown path, and make stages honor cancellation
// whenever possible.
func Run(ctx convCtx.Context, config Config) (err error) {
	if err = validate(ctx, config); err != nil {
		return err
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)

	return run(ctx, config, signals)
}

func validate(ctx convCtx.Context, config Config) (err error) {
	if ctx.Context == nil {
		return errors.New("lifecycle: context is required")
	}
	if config.ListenAndServe == nil {
		return errors.New("lifecycle: ListenAndServe is required")
	}
	if config.ShutdownTimeout <= 0 {
		return errors.New("lifecycle: ShutdownTimeout must be positive")
	}
	if len(config.Stages) == 0 {
		return errors.New("lifecycle: at least one shutdown phase is required")
	}

	seenNames := make(map[string]struct{})
	stageIndex := 0
	for phaseIndex, phase := range config.Stages {
		if len(phase) == 0 {
			return fmt.Errorf("lifecycle: shutdown phase %d is empty", phaseIndex)
		}
		for _, stage := range phase {
			if stage.Name == "" {
				return fmt.Errorf("lifecycle: stage %d has no name", stageIndex)
			}
			if stage.Fn == nil {
				return fmt.Errorf("lifecycle: stage %q has no function", stage.Name)
			}
			if _, exists := seenNames[stage.Name]; exists {
				return fmt.Errorf("lifecycle: duplicate stage name %q", stage.Name)
			}
			seenNames[stage.Name] = struct{}{}
			stageIndex++
		}
	}
	return nil
}

func run(ctx convCtx.Context, config Config, signals chan os.Signal) (err error) {
	listenerResult := make(chan error, 1)
	go func() {
		listenerResult <- config.ListenAndServe(ctx)
	}()

	var (
		listenerErr error
		bySignal    bool
	)
	select {
	case <-signals:
		bySignal = true
	case listenerErr = <-listenerResult:
	}

	// Restore default signal handling on both paths before any shutdown work
	// runs. Leaving the notification in place would route a SIGINT or SIGTERM
	// arriving during shutdown into a channel nobody reads, so the process
	// would ignore its own termination request instead of dying.
	signal.Stop(signals)

	if !bySignal {
		return errors.Join(listenerFailure(listenerErr), shutdown(ctx, config.ShutdownTimeout, config.Stages))
	}

	if config.OnSignal != nil {
		config.OnSignal(ctx)
	}

	// Take the deadline before the phases start so collecting the listener
	// result shares the one shutdown budget instead of extending it.
	deadline := time.Now().Add(config.ShutdownTimeout)
	shutdownErr := shutdown(ctx, config.ShutdownTimeout, config.Stages)
	shutdownErr = errors.Join(shutdownErr, awaitListener(listenerResult, deadline))

	if config.OnSignalShutdown != nil {
		config.OnSignalShutdown(ctx, shutdownErr)
	}
	return shutdownErr
}

// awaitListener collects the listener result after a signal drove shutdown.
// Without it the listener error is left unread in its channel, so a listener
// that died for a real reason is reported as a clean exit. The wait ends at
// deadline, the same instant that bounds the shutdown phases, so collecting the
// result cannot push Run past one ShutdownTimeout; a listener still running
// then is abandoned and its error is not observed.
func awaitListener(listenerResult <-chan error, deadline time.Time) error {
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()

	select {
	case listenerErr := <-listenerResult:
		return listenerFailure(listenerErr)
	case <-timer.C:
		return nil
	}
}

// listenerFailure reports err unless the listener stopped because it was asked
// to. net/http returns http.ErrServerClosed once a shutdown stage closes the
// server, which is a successful stop rather than a failure. An adapter for any
// other listener should return nil for its own graceful-stop sentinel.
func listenerFailure(err error) error {
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func shutdown(ctx convCtx.Context, timeout time.Duration, phases [][]Stage) (err error) {
	bounded := ctx
	var cancel context.CancelFunc
	bounded.Context, cancel = context.WithTimeout(context.WithoutCancel(ctx.Context), timeout)
	defer cancel()

	stageErrors := make([]error, 0)
	for phaseIndex, phase := range phases {
		if bounded.Err() != nil {
			stageErrors = appendDeadlineErrors(stageErrors, phases[phaseIndex:], bounded.Err())
			break
		}
		stageErrors = append(stageErrors, runPhase(bounded, phase)...)
	}
	return errors.Join(stageErrors...)
}

type stageResult struct {
	index int
	err   error
}

func runPhase(ctx convCtx.Context, stages []Stage) (stageErrors []error) {
	results := make(chan stageResult, len(stages))
	for i, stage := range stages {
		go func() {
			results <- stageResult{index: i, err: invokeStage(ctx, stage.Fn)}
		}()
	}

	stageErrors = make([]error, len(stages))
	completed := make([]bool, len(stages))
	remaining := len(stages)
	for remaining > 0 {
		select {
		case result := <-results:
			completed[result.index] = true
			remaining--
			if result.err != nil {
				stageErrors[result.index] = fmt.Errorf("%s: %w", stages[result.index].Name, result.err)
			}
		case <-ctx.Done():
			for {
				select {
				case result := <-results:
					completed[result.index] = true
					remaining--
					if result.err != nil {
						stageErrors[result.index] = fmt.Errorf("%s: %w", stages[result.index].Name, result.err)
					}
				default:
					for i, stage := range stages {
						if !completed[i] {
							stageErrors[i] = fmt.Errorf("%s: %w", stage.Name, ctx.Err())
						}
					}
					return stageErrors
				}
			}
		}
	}
	return stageErrors
}

func invokeStage(ctx convCtx.Context, fn func(convCtx.Context) error) (err error) {
	defer func() {
		if panicValue := recover(); panicValue != nil {
			if panicErr, ok := panicValue.(error); ok {
				err = fmt.Errorf("panic: %w", panicErr)
				return
			}
			err = fmt.Errorf("panic: %v", panicValue)
		}
	}()
	return fn(ctx)
}

func appendDeadlineErrors(stageErrors []error, phases [][]Stage, deadlineErr error) []error {
	for _, phase := range phases {
		for _, stage := range phase {
			stageErrors = append(stageErrors, fmt.Errorf("%s: %w", stage.Name, deadlineErr))
		}
	}
	return stageErrors
}
