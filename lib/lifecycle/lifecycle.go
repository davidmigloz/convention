// Package lifecycle coordinates signal-driven service shutdown.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync"
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
	Stages           []Stage
	OnSignal         func(convCtx.Context)
	OnSignalShutdown func(convCtx.Context, error)
}

// Run serves until SIGINT, SIGTERM, or listener completion, then executes all
// shutdown stages concurrently within one deadline.
//
// A stage that ignores its context may continue after Run returns. Call Run
// only on a terminal process-shutdown path, and make stages honor cancellation
// whenever possible.
func Run(ctx convCtx.Context, config Config) error {
	if err := validate(config); err != nil {
		return err
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)

	return run(ctx, config, signals)
}

func validate(config Config) error {
	if config.ListenAndServe == nil {
		return errors.New("lifecycle: ListenAndServe is required")
	}
	if config.ShutdownTimeout <= 0 {
		return errors.New("lifecycle: ShutdownTimeout must be positive")
	}
	for i, stage := range config.Stages {
		if stage.Name == "" {
			return fmt.Errorf("lifecycle: stage %d has no name", i)
		}
		if stage.Fn == nil {
			return fmt.Errorf("lifecycle: stage %q has no function", stage.Name)
		}
	}
	return nil
}

func run(ctx convCtx.Context, config Config, signals <-chan os.Signal) error {
	listenerResult := make(chan error, 1)
	go func() {
		listenerResult <- config.ListenAndServe(ctx)
	}()

	select {
	case <-signals:
		if config.OnSignal != nil {
			config.OnSignal(ctx)
		}
		shutdownErr := shutdown(ctx, config.ShutdownTimeout, config.Stages)
		if config.OnSignalShutdown != nil {
			config.OnSignalShutdown(ctx, shutdownErr)
		}
		return nil
	case listenerErr := <-listenerResult:
		return errors.Join(listenerErr, shutdown(ctx, config.ShutdownTimeout, config.Stages))
	}
}

func shutdown(ctx convCtx.Context, timeout time.Duration, stages []Stage) error {
	if len(stages) == 0 {
		return nil
	}

	bounded := ctx
	var cancel context.CancelFunc
	bounded.Context, cancel = context.WithTimeout(ctx.Context, timeout)
	defer cancel()

	results := make([]chan error, len(stages))
	var wait sync.WaitGroup
	wait.Add(len(stages))
	for i, stage := range stages {
		results[i] = make(chan error, 1)
		go func() {
			defer wait.Done()
			results[i] <- stage.Fn(bounded)
		}()
	}

	allDone := make(chan struct{})
	go func() {
		wait.Wait()
		close(allDone)
	}()

	select {
	case <-allDone:
	case <-bounded.Done():
	}

	stageErrors := make([]error, len(stages))
	for i, stage := range stages {
		select {
		case err := <-results[i]:
			if err != nil {
				stageErrors[i] = fmt.Errorf("%s: %w", stage.Name, err)
			}
		default:
			stageErrors[i] = fmt.Errorf("%s: %w", stage.Name, context.DeadlineExceeded)
		}
	}

	return errors.Join(stageErrors...)
}
