# Integration Tests

## No direct time usage

`time.Sleep`, polling loops with sleep intervals, and bare `time.After` are
**banned** in new test code. They destroy determinism and waste CI time.

### Why this matters

- A 500ms sleep that "works" locally becomes a flaky failure under CI load
- Polling loops with arbitrary intervals add seconds of dead time per test
- These patterns compound: 10 tests × 2 sleeps × 500ms = 10 seconds wasted

### What to do instead

**Synchronous operations need no waiting.** When a Matrix API call returns
200, the data is persisted. A subsequent read sees it immediately. Don't
sleep "just in case" — if you sent a state event and got an event_id back,
read it back immediately.

**Genuinely async operations** (daemon /sync propagation, sandbox creation,
config reconciliation) use the existing `waitFor*` helpers in helpers_test.go:

- `waitForFile` — file appears on disk (proxy socket, launcher socket)
- `waitForFileGone` — file removed (sandbox teardown)
- `waitForStateEvent` — Matrix state event appears in a room
- `waitForCommandResults` — command result events in room timeline
- `waitForServiceDiscovery` — service directory propagation
- `waitForMessageInRoom` — message from a specific sender

These have proper timeouts and descriptive failure messages. If none of them
fit, write a new purpose-built helper with the same pattern — never inline
a sleep-and-retry loop.

**Unit tests with time-dependent logic** should use injectable clocks, not
real time.
