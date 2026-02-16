# Bureau Observation

[fundamentals.md](fundamentals.md) defines the five primitives. [architecture.md](architecture.md) defines
the runtime topology. This document defines the fourth primitive:
live bidirectional terminal access to any principal in the fleet, from
any machine.

Observation is what makes Bureau tangible. Without it, agents are
invisible processes producing commit logs. With it, you inhabit the
system — you see what an agent sees, type where it types, scroll
through its history. The interface is the real terminal: escape
sequences, colors, TUI rendering, mouse events, cursor positioning.
If it works over SSH + tmux, it works through Bureau observation.

---

## The Relay Model

Observation is a bidirectional byte stream between a local terminal and
a remote tmux session, routed through the daemon transport layer.

```
Local machine                         Remote machine
─────────────                         ──────────────

Your local tmux                       tmux: bureau/iree/amdgpu/pm
┌────────────────────┐                 ┌──────────────────────────┐
│ pane: observe      │                 │ pane 0: claude ...       │
│  iree/amdgpu/pm    │                 │ pane 1: shell            │
│                    │                 │                          │
│ (bureau-observe    │                 │                          │
│  process running)  │                 │                          │
└────────┬───────────┘                 └───────────┬──────────────┘
         │ stdin/stdout                            │ PTY master fd
         │                                         │
   bureau observe                           bureau-observe-relay
         │                                         │
    unix socket                               unix socket
         │                                         │
   bureau-daemon ◄─── transport (WebRTC) ───► bureau-daemon
```

Three processes do the work:

**bureau observe** (client side) runs in a local tmux pane. Connects to
the local daemon over a unix socket, sends a JSON handshake requesting
observation of a principal, then switches to the binary protocol.
Reads the daemon socket, writes to stdout. Reads stdin, writes to the
daemon socket. Handles SIGWINCH by sending resize messages. Prints a
status line on connect. Exits when the remote session ends or the user
closes the pane.

**bureau-observe-relay** (remote side) is forked by the remote daemon
for each observation session. Inherits a unix socket (fd 3) from the
daemon via a socketpair. Allocates a PTY pair, attaches to the
principal's tmux session on the slave side, then bridges bytes
bidirectionally between the PTY master and the inherited socket.
Maintains a ring buffer of recent terminal output for history replay.
Handles resize messages by applying TIOCSWINSZ to the PTY master.

**bureau-daemon** (both sides) routes observation streams through the
transport layer. On the remote side: receives an observation request,
authenticates and authorizes the observer, forks a relay process,
bridges the relay's socket to the transport stream. On the local side:
receives the client's request, opens a transport stream to the remote
daemon, bridges it to the client's socket. The daemon does not parse
observation protocol messages — it moves bytes between sockets
without interpreting the content.

### Why a separate relay process

The relay is a separate binary for the same reasons the proxy is:
isolation, independent updates, and minimal daemon surface.

A relay crash kills one observation session. The daemon, all other
observations, all proxies, all sandboxes — unaffected. The relay
holds no secrets, no transport state, no Matrix session. Upgrading
the relay binary takes effect on new sessions without restarting the
daemon or disrupting existing observations.

The daemon's observation code is fork-and-bridge: authenticate the
request, fork a relay, copy bytes. The implementation logic — PTY
management, ring buffer, resize handling, tmux attachment — lives
entirely in the relay process and the `observe/` library.

### Failure modes

**Relay process crashes**: the observation session dies. The client
sees the socket close, prints a status line. The remote tmux session
is unaffected — it runs independently of whether anyone is observing.
Reconnection spawns a fresh relay.

**Daemon crashes**: all transport connections drop. All relay processes
lose their socket to the daemon and exit. All observations die. But
all remote tmux sessions survive (they do not depend on the daemon),
and all proxies and sandboxes survive. When the daemon restarts and
transport reconnects, observations can be re-established.

**Network partition**: the transport stream drops. Both ends detect it.
On reconnect, the relay sends the ring buffer contents accumulated
during the gap as a history message, filling the observer's scrollback
with what they missed.

### Post-mortem capture vs live observation

Live observation requires authorization grants: the observer needs an
`observe` (or `observe/read-write`) grant for the target principal, and
the target needs an allowance permitting that observer. This is
enforced by the daemon when the observation request arrives.

Post-mortem crash capture is a separate mechanism that does not use
the observation protocol. When a sandboxed process exits with a
non-zero exit code, the launcher captures the tmux pane content via
`capture-pane` (enabled by starting the tmux server with
`remain-on-exit on`), then returns the captured output to the daemon
through the `wait-sandbox` IPC response. The daemon posts the last
50 lines to the machine's config room and logs the full capture
(up to 500 lines) at WARN level.

The two mechanisms have different access models:

- **Live observation**: grant-controlled, per-principal. Only
  authorized observers can attach.
- **Post-mortem capture**: config room membership. Only the admin,
  machine daemon, and fleet controllers see the output. No sandbox
  principals are config room members.

Both models are appropriate for their use case. Live observation is
fine-grained because it gives interactive terminal access to a running
process — a high-privilege operation. Post-mortem capture is
machine-scoped because it serves operators diagnosing infrastructure
failures, and the config room audience already controls the sandbox's
credentials and configuration.

---

## Observation Protocol

Four message types on a bidirectional byte stream. The framing is:

```
[1 byte: message type] [4 bytes: payload length, big-endian uint32] [payload]
```

**Data (0x01)**: raw terminal bytes. Bidirectional. Remote-to-local
carries terminal output (escape sequences, screen updates).
Local-to-remote carries keyboard and mouse input. The payload is
opaque bytes passed through unmodified — no interpretation, no
transformation.

**Resize (0x02)**: terminal dimensions. Local-to-remote only. Payload
is 4 bytes: columns (uint16 big-endian) then rows (uint16 big-endian).
Sent when the observer's terminal resizes (SIGWINCH on the client
process). The relay applies TIOCSWINSZ to the PTY master, which
propagates SIGWINCH to tmux.

**History (0x03)**: ring buffer dump. Remote-to-local only, sent once
on connect before live data begins. Payload is raw terminal bytes
from the ring buffer — the same escape sequences the terminal
originally produced. The client writes them to stdout; the local
tmux pane captures them as scrollback. Full-fidelity history with
colors and formatting intact, scrollable locally.

**Metadata (0x04)**: session information as JSON. Remote-to-local
only, sent once on connect before history. Contains the tmux session
name, pane list with dimensions, active pane indicator, principal
identity, and machine identity. Used by the client for display and
by dashboard logic for composite views.

### Connection sequence

On connect, the remote side sends metadata, then history, then begins
streaming live data. The local side can send data (input) and resize
at any time after receiving metadata. The handshake is one JSON
request/response on the unix socket, then the protocol switches to
binary framing for the remainder of the session.

### Flow control

Terminal traffic is low bandwidth. A fast-scrolling `find /` produces
maybe 10 MB/s; typical agent activity is orders of magnitude less.
The transport's own flow control (SCTP flow control within WebRTC
data channels) and unix socket buffering handle back-pressure. If a slow observer
cannot keep up, back-pressure propagates through the transport to the
relay, to the PTY read, to tmux's rendering — the right behavior with
no application-level flow control needed.

The relay path does not buffer. Every write to a socket or PTY flushes
immediately. Terminal interaction is latency-sensitive at the
millisecond level; a 4 KB buffer fill delay is perceptible.

---

## Ring Buffer

The relay maintains a circular buffer of raw terminal output (default
1 MB — roughly 500K–1M lines of typical terminal output, hours of
coding agent activity). The buffer serves two purposes:

**History on connect**: when an observer connects, the relay sends the
ring buffer contents as a history message. The observer gets scrollback
from before they connected — the agent's terminal output since the
relay started (or since the buffer wrapped, whichever is more recent).

**Gap fill on reconnect**: when an observer reconnects after a network
interruption, the relay sends the portion of the ring buffer generated
during the gap.

The buffer stores raw bytes — terminal escape sequences and all. A
buffer that strips escape sequences would lose colors, cursor
positioning, and TUI rendering. The observer receives the same byte
stream the terminal originally produced and replays it into the local
pane, preserving full fidelity.

### Sequence tracking

The ring buffer tracks a monotonically increasing byte offset (total
bytes ever written). When an observer connects, it sends its last
known offset (zero for first connection). The relay sends everything
from that offset forward. If the requested offset is older than the
buffer's oldest retained data (the buffer wrapped), the relay sends
everything it has — the observer missed some output, but gets
everything still available. The memory cost per relay process is
negligible (1 MB per active session).

---

## Layout System

### Layouts as state events

Pane arrangements are `m.bureau.layout` state events in rooms. The
content describes a tmux session structure: windows, panes, split
directions, and what each pane shows.

A layout has four pane types:

**Observe** — relay to a remote principal's tmux session. Used in
channel layouts for composite views. The pane runs `bureau observe`
connected to the specified principal.

```json
{"observe": "iree/amdgpu/pm", "split": "horizontal", "size": 50}
```

**Command** — run a local process. Used for tooling panes in composite
layouts (dashboards, shells, monitoring).

```json
{"command": "beads-tui --project iree/amdgpu", "split": "horizontal", "size": 30}
```

**Role** — identifies the pane's purpose in a principal's own layout.
The launcher resolves roles to concrete commands based on the agent
template. The layout describes structure; the template describes
behavior.

```json
{"role": "agent", "split": "horizontal", "size": 65}
```

**ObserveMembers** — dynamically populates panes from room membership.
When the daemon encounters this pane type, it queries room members,
filters by label patterns, and creates one Observe pane per matching
member. Used in channel layouts for views that automatically adapt
when agents join or leave.

```json
{"observe_members": {"labels": {"role": "agent"}}, "split": "horizontal"}
```

### Layout examples

A **channel layout** (workstream view) in a workspace room:

```json
{
  "type": "m.bureau.layout",
  "state_key": "",
  "content": {
    "prefix": "C-a",
    "windows": [
      {
        "name": "agents",
        "panes": [
          {"observe": "iree/amdgpu/pm", "split": "horizontal", "size": 50},
          {"observe": "iree/amdgpu/codegen", "size": 50}
        ]
      },
      {
        "name": "tools",
        "panes": [
          {"command": "beads-tui --project iree/amdgpu", "split": "horizontal", "size": 30},
          {"observe": "iree/amdgpu/ci-runner", "size": 70}
        ]
      }
    ]
  }
}
```

A **principal layout** (the agent's own session):

```json
{
  "type": "m.bureau.layout",
  "state_key": "iree/amdgpu/pm",
  "content": {
    "windows": [
      {
        "name": "main",
        "panes": [
          {"role": "agent", "split": "horizontal", "size": 65},
          {"role": "shell", "size": 35}
        ]
      }
    ]
  }
}
```

For channel layouts, the state key is empty (room-level layout). For
principal layouts, the state key is the principal's localpart.

### Bidirectional layout sync

The layout state event and the live tmux session are kept in sync
bidirectionally.

**tmux to Matrix**: the daemon monitors managed tmux sessions via tmux
control mode (`tmux -C attach`). Control mode emits notifications for
layout-relevant events: `%layout-change` (pane split, resize, close),
`%window-add`, `%window-close`, `%window-renamed`. The daemon
debounces these (500ms — fast enough to feel responsive, slow enough
to coalesce rapid changes like dragging a splitter), reads the current
tmux layout via `list-panes`, and publishes it to Matrix if it changed
(structural comparison avoids redundant publishes).

**Matrix to tmux**: the daemon receives layout state event changes via
Matrix sync. When the layout changes (an operator updates it, an
orchestrator agent reconfigures the workstream), the daemon applies the
changes to the running tmux session — creating, splitting, closing,
and resizing panes to match.

The daemon uses the state event as the desired state and the tmux
session as the current state. Conflicts (simultaneous change from both
sides) resolve in favor of the most recent Matrix state event — Matrix
is the source of truth for desired configuration.

### Dynamic member expansion

Channel layouts can use `observe_members` panes that expand
dynamically based on room membership. The daemon fetches the room's
member list, filters against the pane's label patterns (subset match:
a member must have every key-value pair specified in the filter), and
creates one Observe pane per matching member.

The first expanded pane inherits the original pane's split direction
and size. Subsequent panes use the same direction with no explicit
size (tmux divides evenly). If no members match, the pane is removed
entirely. When agents join or leave the room, the daemon re-expands
and updates the tmux session.

---

## Observation Targets

### Principal observation

`bureau observe iree/amdgpu/pm`

Direct relay to one principal's tmux session on the machine where it
runs. The observer sees the full remote tmux session — all its windows,
panes, status bar. One local tmux pane becomes one remote tmux session.

The remote tmux session is created by the launcher when the principal
starts. Session names follow `bureau/<localpart>` (e.g.,
`bureau/iree/amdgpu/pm`). The layout comes from the principal's layout
state event, or from the agent template if no state event exists.

### Channel observation

`bureau dashboard #iree/amdgpu/general`

Composite view of a workstream. The CLI queries the daemon for the
channel's expanded layout (the daemon fetches the `m.bureau.layout`
state event from the room, expands `observe_members` panes, and
returns the result). The CLI then creates a local tmux session and
populates it: each Observe pane gets a `bureau observe` process
connected to the relevant remote principal, each Command pane runs a
local process.

This is the primary observation mode for day-to-day work. You are not
watching one agent — you are watching a project. The channel groups
the agents, the layout arranges them, and the local tmux session is
the live workspace.

Multiple observers of the same channel each get their own local tmux
session built from the same layout. They can independently switch
windows and scroll without affecting each other.

### Machine observation

`bureau dashboard`

Sysadmin view of a machine. The daemon auto-generates a layout from
all running principals on the local machine, sorted alphabetically
and stacked vertically. Useful for monitoring, debugging, and getting
a quick view of what is running.

Machine observation can also target remote machines when the
transport layer provides connectivity.

---

## Input Handling

### Nested tmux prefix keys

Bureau-managed remote tmux sessions use a different prefix key than
the user's local tmux. If the user's local prefix is Ctrl-b, remote
sessions use Ctrl-a (configurable via the layout's `prefix` field).

This eliminates the double-prefix problem entirely. Ctrl-b is always
local (meta-navigation: switching windows, detaching, opening new
observations). Ctrl-a is always remote (pane selection, remote tmux
commands, scrolling). There is no ambiguity and no double-press needed.

### Keyboard and mouse

All keystrokes flow through the observation relay to the remote tmux.
Typing in Claude Code, switching remote panes, interacting with TUIs
— all work transparently. The local tmux prefix key is the only
keystroke intercepted by local tmux.

Mouse events pass through to the remote tmux session. Click on a
remote pane, and remote tmux selects it. Scroll in a TUI, and the TUI
handles it. Drag a remote pane border, and remote tmux resizes it.
Click on the remote status bar, and remote tmux switches windows.

Observation panes in local tmux have `mouse off` at the pane level
(tmux per-pane mouse setting). This ensures all mouse events reach the
remote tmux rather than being consumed by local tmux. For local tmux
operations, keyboard shortcuts only — a natural split between
keyboard for meta-navigation and mouse for interacting with the
observed content.

### Local scrollback via history replay

When the client first connects, it receives the ring buffer history
and writes it to stdout. The local tmux pane captures this as
scrollback. Ctrl-b [ (local copy mode) gives searchable local history
going back to before the observer connected, with full terminal
formatting preserved.

For TUI content that redraws the screen (rather than appending),
local scrollback shows the raw escape sequences from redraws. For
TUIs, scroll within the remote application (mouse events pass
through) or use the remote tmux's copy mode.

---

## Cross-Machine Routing

Observation rides on the daemon-to-daemon transport layer. The local
daemon receives the client's request, determines which machine hosts
the target principal (from the service directory or machine config),
and routes the observation stream to that machine's daemon.

**Local observation**: the target principal runs on the same machine.
The daemon forks a relay process directly and bridges the client
socket to the relay socket. No transport involved.

**Remote observation**: the target principal runs on a different
machine. The local daemon opens a transport connection to the remote
daemon, sends the observation request, and bridges the client socket
to the transport stream. The remote daemon authenticates, authorizes,
forks a relay, and bridges its end. The two daemons are transparent
pipes — they do not interpret the observation protocol.

### Latency

Bureau adds under 0.05 ms to the observation round trip. Each hop
(client → daemon socket, daemon → relay socket) is kernel IPC at
sub-0.01 ms. Network RTT (0.5 ms on LAN, 20–100 ms over internet)
is the only perceptible factor. This is competitive with SSH because
the physics are the same — just with DTLS/SCTP (WebRTC) instead of
SSH's TCP channel multiplexing.

Persistent daemon-to-daemon PeerConnections mean zero setup latency
for new observation sessions. Opening an observation creates a data
channel on an existing PeerConnection, not a fresh TCP + key exchange
handshake. And SCTP's independent stream multiplexing means a stalled
retransmission on one observation does not block another — no
head-of-line blocking between concurrent sessions.

---

## Authorization

Observation is a **target-side authorization decision**: the target
principal's allowances in the authorization index control who may
observe and in what mode. The observer's identity is established by
authentication (Matrix token verification); the access control question
is answered entirely by the target's allowances and allowance denials.
Actor-side grants are not checked — observation authorization does not
require the observer to hold a grant.

### Allowance structure

Observation allowances use two actions:

- **`observe`**: permits read-only observation of the target principal.
- **`observe/read-write`**: permits interactive observation (send input).

Each allowance specifies actor patterns (glob matching on observer
identity). Example allowances in an authorization policy:

```json
{
  "allowances": [
    {"actions": ["observe"], "actors": ["admin", "iree/**"]},
    {"actions": ["observe/read-write"], "actors": ["admin"]}
  ]
}
```

`"admin"` matches exactly the admin identity. `"iree/**"` matches any
identity under the `iree/` namespace. `"*"` matches any single-segment
identity. A principal with no `observe` allowance is invisible to all
observers — default-deny.

**Mode downgrade**: when an observer requests read-write mode but the
target has no matching `observe/read-write` allowance, the daemon
downgrades to read-only. The relay attaches to tmux with the read-only
flag (`tmux attach -r`) and does not write to the PTY master.

**ObservePolicy synthesis**: for principals whose configuration uses
the legacy `ObservePolicy` fields (`AllowedObservers`,
`ReadWriteObservers`) rather than an explicit `Authorization` block,
the daemon synthesizes equivalent allowances during authorization index
rebuild. `AllowedObservers` patterns become `observe` allowances;
`ReadWriteObservers` patterns become `observe/read-write` allowances.
Per-principal `ObservePolicy` overrides `DefaultObservePolicy`.

### Authentication

Every observation request carries a Matrix access token. The daemon
verifies this token against the homeserver before granting access. The
verified identity (not the asserted identity in the request) is checked
against the target principal's allowances in the authorization index.
Token verification is cached to avoid per-request homeserver
round-trips.

### Runtime policy enforcement

When the authorization index is rebuilt (machine config update via
Matrix sync), the daemon compares each running principal's allowances
before and after the rebuild. If a principal's observation allowances
changed, the daemon re-evaluates all active observation sessions
targeting that principal. Sessions that no longer satisfy the updated
allowances are terminated immediately — the daemon closes the client
connection, which triggers relay cleanup.

### Audit

Every observation session is logged: who observed what, when, for how
long, and in what mode (read-only or read-write).

---

## Tmux Session Management

### Bureau tmux server

Bureau-managed sessions use a dedicated tmux server, isolated from
the user's personal tmux. The `lib/tmux` package provides a `Server`
abstraction that injects the `-S <socket>` flag on every tmux command.
All Bureau tmux operations go through this abstraction — there is no
path in the codebase where a bare `tmux` command without `-S` targets
Bureau sessions.

This guarantees that Bureau's tmux configuration (prefix, mouse,
scrollback, hooks) does not affect the user's tmux, and the user's
tmux config does not affect Bureau sessions.

### Session naming

Bureau tmux session names follow `bureau/<localpart>`:
`bureau/iree/amdgpu/pm`, `bureau/service/stt/whisper`. The `/` in the
localpart creates a natural hierarchy (tmux treats `/` as a regular
character in session names). The launcher creates the session as part
of the sandbox launch sequence. The relay attaches to it. Layout
detection queries it.

### Multiple observers

tmux natively supports multiple clients attached to the same session.
Multiple observers of the same principal share the same view — same
window, same scroll position. If one observer types, the others see
it. Useful for pair debugging (two humans watching the same agent) or
audit (a monitoring agent observing alongside a human).

Each observer's terminal dimensions may differ. tmux handles this: the
session size is the minimum of all attached clients' dimensions. This
is standard tmux behavior.

---

## Daemon Socket Protocol

The daemon listens for observation requests on a unix socket separate
from the launcher IPC socket. All observation operations — streaming
sessions, layout queries, target listing, machine layout queries —
share this socket using a JSON request line to distinguish between
request types.

### Streaming observation

Request:
```json
{"principal": "iree/amdgpu/pm", "mode": "readwrite", "observer": "@admin:bureau.local", "token": "..."}
```

Response (success):
```json
{"ok": true, "session": "bureau/iree/amdgpu/pm", "machine": "machine/workstation", "granted_mode": "readwrite"}
```

After a successful response, the socket switches to the binary
observation protocol. The `granted_mode` may differ from the requested
mode if the observer has read-only access.

### Layout query

Request:
```json
{"action": "query_layout", "channel": "#iree/amdgpu/general:bureau.local", "observer": "...", "token": "..."}
```

The daemon resolves the channel alias to a room ID, fetches the
`m.bureau.layout` state event, expands `observe_members` panes
against room membership, and returns the expanded layout as JSON.
The connection closes after the response — no streaming.

### Machine layout query

Request:
```json
{"action": "query_machine_layout", "observer": "...", "token": "..."}
```

The daemon generates a layout from all running principals on the
local machine, filtered to those the observer is authorized to see.
Returns the layout and the machine name. Connection closes after
response.

### Target listing

Request:
```json
{"action": "list", "observable": true, "observer": "...", "token": "..."}
```

Returns all known principals and machines, with observability and
location metadata. When `observable` is true, only principals that are
actively observable (running locally, or running on a reachable remote
machine) are included.

---

## Relationship to Other Primitives

Observation completes the feedback loop. Without it, Bureau is a
system you deploy into and read logs from. With it, Bureau is a system
you inhabit.

**Sandbox**: observation peers into sandboxes via PTY fd inheritance.
The sandboxed process has file descriptors connected to a tmux session
— it does not know, does not care, and cannot interfere. The tmux
process runs outside the sandbox ([fundamentals.md](fundamentals.md)). This is
the key architectural insight: the observation infrastructure is completely
invisible to the observed agent.

**Proxy**: orthogonal. The proxy handles API access; observation
handles terminal access. They do not interact.

**Transport**: observation rides on the daemon-to-daemon transport
layer. The same persistent connections that carry service routing
and Matrix operations carry observation streams.

**Messaging**: layout state events use Matrix for persistence and
sharing. Channel observation targets use room membership. Observation
and messaging are complementary views of the same rooms — messaging
provides the data model, observation provides the interactive
terminal.
