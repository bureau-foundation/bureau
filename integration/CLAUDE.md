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

For daemon notifications (typed `m.bureau.*` messages), use the generic
package-level function instead of `roomWatch` methods:

- `waitForNotification[T](t, &watch, msgtype, senderID, predicate, description)` — typed daemon notification

This filters on `msgtype` + sender, unmarshals the event content into `T`,
and optionally applies a predicate. The predicate can be nil to accept
the first matching message. All daemon notifications use typed structs
and `MsgType*` constants defined in `lib/schema/events.go`.

Create a watch BEFORE triggering the action, then call the appropriate
Wait method after:

```go
watch := watchRoom(t, admin, machine.ConfigRoomID)
pushMachineConfig(t, admin, machine, config)  // triggers daemon action
waitForNotification[schema.GrantsUpdatedMessage](
    t, &watch, schema.MsgTypeGrantsUpdated, machine.UserID,
    func(m schema.GrantsUpdatedMessage) bool {
        return m.GrantCount == 1
    }, "grants updated with 1 grant")
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
admin socket push), but the daemon posts a typed
`m.bureau.service_directory_updated` notification to the config room after
`pushServiceDirectory` completes. The message includes `added`, `removed`,
and `updated` fields listing changed service localparts. Match on the
specific service name to avoid cross-test interference — all daemons share
`#bureau/service` and will post to their own config rooms when any service
changes:

```go
serviceWatch := watchRoom(t, admin, machine.ConfigRoomID)
admin.SendStateEvent(ctx, serviceRoomID, "m.bureau.service", "service/stt/test", content)
waitForNotification[schema.ServiceDirectoryUpdatedMessage](
    t, &serviceWatch, schema.MsgTypeServiceDirectoryUpdated, machine.UserID,
    func(m schema.ServiceDirectoryUpdatedMessage) bool {
        return slices.Contains(m.Added, "service/stt/test")
    }, "service directory update adding service/stt/test")
entries := proxyServiceDiscovery(t, proxyClient, "")
```

The message is posted after all running proxies have been updated, so the
proxy query is guaranteed to see the new directory.

**Unit tests with time-dependent logic** should use injectable clocks, not
real time.

## Use production code paths, not inline logic

Integration tests must exercise the same code paths that production uses.
Every business operation has a library function in `lib/` or a helper in
`machine_test.go` that calls it. Tests call these — never inline the
implementation.

### Why this matters

When a test inlines business logic (manual credential encryption, direct
room resolution + state event publishing, hand-constructed config payloads),
it creates a parallel implementation that:

- **Drifts silently** from production — a change to the library function
  doesn't propagate to the test, so the test passes but the system is broken
- **Hides regressions** — the test exercises its own copy of the logic, not
  the code that ships
- **Defeats type safety** — using `map[string]any` instead of typed structs
  skips compile-time validation of the protocol schema

### Required helpers

Use these instead of inlining the equivalent operations:

| Operation | Helper | Production path |
|-----------|--------|-----------------|
| Provision credentials | `pushCredentials(t, admin, machine, account)` | `lib/credential.Provision` |
| Push machine config | `pushMachineConfig(t, admin, machine, config)` | `schema.MachineConfig` via `SendStateEvent` |
| Publish standard test template | `publishTestAgentTemplate(t, admin, machine, name)` | `lib/template.Push` |
| Grant template room access | `grantTemplateAccess(t, admin, machine)` | `InviteUser` to template room |
| Register a principal account | `registerPrincipal(t, localpart, password)` | `messaging.Client.Register` |
| Deploy principals end-to-end | `deployPrincipals(t, admin, machine, config)` | credentials + config + wait for sockets |

For custom templates (non-standard content like `RequiredServices`,
intentionally failing binaries, etc.), use `template.Push` directly with
`grantTemplateAccess`:

```go
grantTemplateAccess(t, admin, machine)

ref, err := schema.ParseTemplateRef("bureau/template:my-custom")
if err != nil {
    t.Fatalf("parse template ref: %v", err)
}
_, err = template.Push(ctx, admin, ref, schema.TemplateContent{
    Description:      "Custom template for this test",
    Command:          []string{testAgentBinary},
    RequiredServices: []string{"ticket"},
    // ... rest of template content
}, testServerName)
if err != nil {
    t.Fatalf("push template: %v", err)
}
```

### Anti-patterns

**Direct state event construction for business operations.** If the
operation has a library function, use it. Direct `admin.SendStateEvent`
with business types is a sign you're bypassing the production path.

```go
// BAD: inlines credential encryption, bypasses production provisioning
bundle := map[string]string{"MATRIX_TOKEN": token}
data, _ := json.Marshal(bundle)
encrypted, _ := sealed.Encrypt(data, publicKey)
admin.SendStateEvent(ctx, configRoomID, "m.bureau.credentials", key, encrypted)

// GOOD: uses production credential provisioning
pushCredentials(t, admin, machine, account)
```

```go
// BAD: inlines template room resolution and state event publish
templateRoomID, _ := admin.ResolveAlias(ctx, schema.FullRoomAlias(schema.RoomAliasTemplate, server))
admin.InviteUser(ctx, templateRoomID, machine.UserID)
admin.SendStateEvent(ctx, templateRoomID, schema.EventTypeTemplate, name, content)

// GOOD: uses production template push
grantTemplateAccess(t, admin, machine)
ref, _ := schema.ParseTemplateRef("bureau/template:" + name)
template.Push(ctx, admin, ref, content, testServerName)
```

```go
// BAD: manual MachineConfig with untyped map
admin.SendStateEvent(ctx, configRoomID, "m.bureau.machine_config", machineName,
    map[string]any{"principals": []map[string]any{{...}}})

// ALSO BAD: typed struct but inline instead of helper
admin.SendStateEvent(ctx, configRoomID, schema.EventTypeMachineConfig,
    machine.Name, schema.MachineConfig{...})

// GOOD: uses pushMachineConfig helper
pushMachineConfig(t, admin, machine, deploymentConfig{
    Principals: []principalSpec{{
        Account:  agent,
        Template: templateRef,
    }},
})
```

**Resolving well-known rooms manually.** Room aliases for system rooms
(`bureau/system`, `bureau/machine`, `bureau/template`, `bureau/service`,
per-machine config rooms) are resolved internally by the library functions
and helpers. Test code should not resolve them unless it genuinely needs
the room ID for a purpose the helpers don't cover (e.g., setting up a
room watch on a specific global room).

### When direct Matrix API calls ARE correct

Some operations are inherently test-specific and have no production
library equivalent. Direct `session.SendStateEvent`, `session.CreateRoom`,
and similar calls are appropriate when:

- **Reading state for assertions** — `admin.GetStateEvent` to verify a
  ticket was created, a config was published, etc. Tests need to read
  Matrix state to verify outcomes.
- **Sending test messages** — `admin.SendMessage` to trigger agent behavior
  from the test harness.
- **Setting up domain-specific fixtures** — creating project rooms with
  ticket configuration, publishing room service bindings, configuring
  power levels. These are specific to the domain (tickets, services) and
  don't have Bureau-level library functions.
- **Deliberately crafting invalid or edge-case state** — tests that verify
  error handling, recovery from inconsistent state, or security boundaries
  may need to construct state that the production libraries would reject.
  Comment clearly why the direct API call is intentional.
- **Room membership management** — `admin.InviteUser` and
  `session.JoinRoom` for test-specific room membership (e.g., making an
  agent join a config room so it can send messages). The helpers handle
  membership for standard operations; direct calls are for test-specific
  setup.

The test for whether a direct API call is appropriate: could this operation
be done incorrectly in a way that would silently break the test? If yes,
there should be a library function or helper. If no (because the operation
is simple and its correctness is obvious from the call site), a direct
call is fine.
