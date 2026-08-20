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
callbacks and returns the shutdown error. The configured timeout covers the
shutdown phases, not callback execution, so callbacks must return promptly.

After the first signal starts shutdown, the process restores the default signal
behavior. A second SIGINT or SIGTERM can therefore terminate a blocked shutdown
immediately.

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
