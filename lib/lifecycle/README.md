# Lifecycle Package

The lifecycle package runs a service until its listener exits or the process
receives SIGINT or SIGTERM. It then runs named shutdown stages concurrently
under one deadline.

## Usage

```go
err := lifecycle.Run(ctx, lifecycle.Config{
	ListenAndServe:   svc.ListenAndServe,
	ShutdownTimeout: 20 * time.Second,
	Stages: []lifecycle.Stage{
		{Name: "drain http server", Fn: svc.Shutdown},
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
to the caller without invoking the callbacks.

## Shutdown Stages

Every stage receives the same context with the configured deadline. Stages run
concurrently, and returned errors retain declaration order with the stage name
attached.

A stage that ignores cancellation does not block `Run` past the deadline, but
its goroutine can continue afterward. Use this package only on a terminal
process-shutdown path, and make shutdown stages honor their context whenever
possible.
