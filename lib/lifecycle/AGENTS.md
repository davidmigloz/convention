# Lifecycle Package Implementation Details

`lifecycle.Run` owns SIGINT and SIGTERM registration, the listener goroutine,
and one bounded deadline shared by all shutdown stages.

## Public Contract

- Keep the exported API limited to `Config`, `Stage`, and `Run`.
- `ListenAndServe` receives the caller's convention context.
- `OnSignal` and `OnSignalShutdown` are synchronous, optional, and signal-only.
- Listener completion returns `errors.Join(listenerErr, shutdownErr)`.
- Signal completion reports the shutdown result through `OnSignalShutdown` and
  returns nil.
- Stage errors retain declaration order and include their stage name.

## Concurrency Contract

Stages run concurrently against one copied `convCtx.Context` whose embedded
context carries the shutdown deadline. A stage that ignores cancellation is
abandoned at the deadline. This is safe only when the caller is on a terminal
process-shutdown path. Tests must release blocked stages before completing so
they do not leave permanent goroutines behind.

Tests inject signals through the unexported `run` helper. Production code must
continue to register signals inside `Run`.
