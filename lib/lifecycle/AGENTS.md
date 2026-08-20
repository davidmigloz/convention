# Lifecycle Package Implementation Details

> IMPORTANT: AI agents must treat AGENTS.md and README.md as authoritative living documents. Any change to the implementation that affects behaviors must be mirrored in both files. The code and documentation must never drift apart. When the implementation changes, these documents must be updated immediately so they always reflect the current system.

`lifecycle.Run` owns SIGINT and SIGTERM registration, the listener goroutine,
and one bounded deadline shared by all shutdown phases.

## Public Contract

- Keep the exported API limited to `Config`, `Stage`, and `Run`.
- `ListenAndServe` receives the caller's convention context.
- `OnSignal` and `OnSignalShutdown` are synchronous, optional, and signal-only.
- Listener completion returns `errors.Join(listenerErr, shutdownErr)`.
- Signal completion reports the shutdown result through `OnSignalShutdown` and
  returns the same error.
- Stage errors retain declaration order and include their stage name.
- Validation rejects a nil embedded context, missing or empty shutdown phases,
  duplicate stage names, and incomplete stages before starting the listener.

## Concurrency Contract

Phases run sequentially under one global deadline. Stages in the same phase run
concurrently against one copied `convCtx.Context`. Its embedded context keeps
the caller's values, detaches caller cancellation, and carries the shutdown
deadline. A stage panic becomes a named stage error. A stage that ignores
cancellation is abandoned at the deadline. This is safe only when the caller is
on a terminal process-shutdown path. Tests must release blocked stages before
completing so they do not leave permanent goroutines behind.

Callbacks run outside the shutdown deadline and must return promptly. Receiving
the first signal restores default signal behavior so a second SIGINT or SIGTERM
terminates a blocked shutdown.

Most tests inject signals through the unexported `run` helper. A focused
subprocess-safe test must exercise production registration in `Run` with a real
SIGTERM.
