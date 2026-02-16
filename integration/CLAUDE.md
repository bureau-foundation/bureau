# Integration Tests

## Hard timing requirements

No individual integration test may take more than 10 seconds. The full
integration test suite must complete in under 40 seconds (wall clock, excluding
Docker startup in TestMain). The Bazel timeout is set to 60 seconds as a hard
kill — if the suite hits that, something is broken, not slow.

These are not guidelines. There are no exceptions for "machine load", "CI
variability", or "this test does a lot." If a test takes more than 10 seconds,
the test has a bug — find it and fix it. Every wait in the test suite is either
inotify (instant on event) or Matrix /sync long-poll (instant on event). There
is no legitimate reason for any operation to take 10 seconds.

When running integration tests from the CLI, never set `--test_timeout` to
anything longer than 60 seconds. The correct response to a timeout is to
diagnose why the test is slow, not to give it more time.

## Diagnosing hangs

When an integration test hangs, the first step is always to check the logs.
Run with `--test_arg=-test.v --test_output=all` to see structured JSON logs
from all daemon, launcher, and fleet controller processes interleaved with Go
test output. The logs tell you exactly what happened and where the process
stopped.

Do NOT try to diagnose hangs by reading source code and guessing. The logs
exist specifically so you don't have to do that. If the logs don't make the
root cause obvious, then improving the logging is P0 — higher priority than
fixing the hang itself. We must be able to diagnose hangs in production from
logs alone, and integration tests are our proving ground for that.

Concrete steps:

1. Run the hanging test in isolation with verbose logging:
   `bazel test //integration:integration_test --test_filter='TestName' --test_arg=-test.v --test_output=all`
2. Read the logs from the bottom (timeout/panic) upward to find the last
   successful operation.
3. The gap between the last log line and the timeout IS the diagnosis — it
   tells you exactly which operation is stuck.
4. If step 3 doesn't point directly at the fix, add logging to the stuck
   code path and re-run. This logging improvement is part of the fix, not
   throwaway debug output.

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
config reconciliation) use the existing helpers in helpers_test.go:

- `waitForFile` — file appears on disk via inotify (proxy socket, launcher socket)
- `waitForFileGone` — file removed via inotify (sandbox teardown)
- `watchRoom` + `roomWatch` methods — all Matrix event waiting

### Room watches

Room watches are the **only** pattern for waiting on Matrix events. All
Matrix-based waiting uses `roomWatch` methods built on `WaitForEvent`:

- `WaitForEvent(t, predicate, description)` — core primitive, matches any event
- `WaitForMessage(t, bodyContains, senderID)` — m.room.message from a sender
- `WaitForStateEvent(t, eventType, stateKey)` — state event, returns raw JSON
- `WaitForMachineStatus(t, stateKey, predicate, description)` — typed MachineStatus
- `WaitForCommandResults(t, requestID, count)` — collect command result events

Create a watch BEFORE triggering the action, then call the appropriate
Wait method after:

```go
watch := watchRoom(t, admin, machine.ConfigRoomID)
pushMachineConfig(t, admin, machine, config)  // triggers daemon action
watch.WaitForMessage(t, "expected message", machine.UserID)
```

```go
watch := watchRoom(t, admin, machineRoomID)
startProcess(t, "daemon", daemonBinary, args...)
watch.WaitForStateEvent(t, "m.bureau.machine_status", machine.Name)
```

```go
watch := watchRoom(t, admin, machine.ConfigRoomID)
admin.SendEvent(ctx, roomID, "m.room.message", command)
results := watch.WaitForCommandResults(t, requestID, 2)
```

The watch captures a sync checkpoint. All Wait methods use Matrix /sync
long-polling (server holds the connection until events arrive). No timeout
parameter — bounded by t.Context() (test timeout). Never inline a
sleep-and-retry loop.

### Service directory propagation

Service directory changes flow through a non-Matrix path (daemon → proxy
admin socket push), but the daemon posts a "Service directory updated"
message to the config room after `pushServiceDirectory` completes. The
message names each changed service (e.g., "Service directory updated:
added service/stt/test, removed service/foo/bar"). Match on the specific
service name to avoid cross-test interference — all daemons share
`#bureau/service` and will post to their own config rooms when any service
changes:

```go
serviceWatch := watchRoom(t, admin, machine.ConfigRoomID)
admin.SendStateEvent(ctx, serviceRoomID, "m.bureau.service", "service/stt/test", content)
serviceWatch.WaitForMessage(t, "added service/stt/test", machine.UserID)
entries := proxyServiceDiscovery(t, proxyClient, "")
```

The message is posted after all running proxies have been updated, so the
proxy query is guaranteed to see the new directory.

**Unit tests with time-dependent logic** should use injectable clocks, not
real time.
