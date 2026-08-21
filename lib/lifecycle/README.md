# Lifecycle Package

The lifecycle package runs a service until its listener exits or the process
receives SIGINT or SIGTERM. It then runs named shutdown stages in ordered phases
under one deadline. Stages within a phase run concurrently.

## Usage

```go
server, err := convAPI.NewServer(ctx, "", 443, policy, serviceAPI)
if err != nil {
	return err
}

err = lifecycle.Run(ctx, lifecycle.Config{
	ListenAndServe: func(convCtx.Context) error {
		return server.ListenAndServe()
	},
	ShutdownTimeout: 20 * time.Second,
	Stages: [][]lifecycle.Stage{
		{
			{Name: "drain http server", Fn: server.Shutdown},
		},
		{
			{Name: "close database", Fn: closeDatabase},
			{Name: "flush telemetry", Fn: flushTelemetry},
		},
	},
	OnSignal: func(ctx convCtx.Context) {
		ctx.Logger().Info("received shutdown signal")
	},
	OnSignalShutdown: func(ctx convCtx.Context, err error) {
		if err != nil {
			ctx.Logger().Error("shutdown failed", "error", err)
		}
	},
})
```

The signal callbacks are optional and run synchronously only when a signal
starts shutdown. Listener completion returns its listener and shutdown errors
to the caller without invoking the callbacks. Signal completion invokes both
callbacks and returns the shutdown error joined with any listener failure. The
configured timeout covers the shutdown phases, not callback execution, so
callbacks must return promptly.

Both paths report a listener that failed for a real reason. A listener stopped
on purpose is not a failure: `http.ErrServerClosed` is what `net/http` returns
once a shutdown stage closes the server, so it is reported as success. An
adapter for any other listener should return nil for its own graceful-stop
sentinel.

On the signal path the listener result is collected after the phases finish,
and that wait ends at the same deadline that bounds the phases, so `Run` still
returns within one `ShutdownTimeout`. If no stage ever stops the listener, that
budget is spent in full before `Run` returns and the listener error is not
observed.

Before shutdown begins, the process restores the default signal behavior. This
happens on both paths, so a SIGINT or SIGTERM arriving during shutdown
terminates the process immediately whether that shutdown was started by a
signal or by listener completion.

## Shutdown Stages

Every stage receives the same context with the configured deadline. Caller
cancellation is detached so shutdown can still use the full timeout, while
context values remain available. Phases run sequentially, stages within one
phase run concurrently, and errors retain phase and stage declaration order
with the stage name attached.

If the deadline expires before a stage publishes its result, that stage is
reported with the context deadline error. An error returned afterward is not
observed by `Run`.

A stage that ignores cancellation does not block `Run` past the deadline, but
its goroutine can continue afterward. Use this package only on a terminal
process-shutdown path, and make shutdown stages honor their context whenever
possible.
