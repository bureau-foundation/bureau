-- Copyright 2026 The Bureau Authors
-- SPDX-License-Identifier: Apache-2.0

/-
  Bureau Reconciliation — Formal Model

  Models the daemon's reconciliation loop as a state machine operating on
  sets of principals. The goal is to prove convergence, idempotency, and
  safety properties of the principal lifecycle management.

  This is a specification, not an implementation. The Go code in
  cmd/bureau-daemon/sync.go is the implementation; this model describes
  what properties that implementation should satisfy.
-/

/-! ## Principal Identity and Configuration -/

/-- Principal identifier (e.g., "bureau/fleet/prod/agent/code-reviewer").
    Modeled as Nat for simplicity — the actual identity type is irrelevant
    to the reconciliation properties we care about. -/
abbrev Principal : Type := Nat

/-- Restart policy determines behavior after sandbox exit. -/
inductive RestartPolicy where
  | always    -- Restart regardless of exit code
  | onFailure -- Restart only on non-zero exit
  | never     -- Never restart
  deriving DecidableEq, Repr

/-- Exit outcome of a sandbox process. -/
inductive ExitOutcome where
  | success   -- Exit code 0
  | failure   -- Non-zero exit code
  deriving DecidableEq, Repr

/-- Whether a principal should restart given its policy and exit outcome. -/
def shouldRestart (policy : RestartPolicy) (outcome : ExitOutcome) : Bool :=
  match policy, outcome with
  | .always, _             => true
  | .onFailure, .failure   => true
  | .onFailure, .success   => false
  | .never, _              => false

/-! ## Start Conditions -/

/-- Abstract representation of a start condition's evaluation result.
    In the real system, this involves fetching Matrix state events and
    evaluating content match criteria. Here we model it as a pure function
    from world state to Bool. -/
structure StartCondition where
  /-- Whether the condition is currently satisfied. -/
  satisfied : Bool
  deriving DecidableEq, Repr

/-- A principal assignment from MachineConfig. -/
structure Assignment where
  principal : Principal
  autoStart : Bool
  condition : Option StartCondition
  restartPolicy : RestartPolicy
  /-- Whether this is a dynamic principal (pipeline executor).
      Dynamic principals manage their own lifecycle and are never killed
      by reconcile when their condition becomes false. -/
  isDynamic : Bool
  deriving Repr

/-- A start condition is met when it's absent (always start) or satisfied. -/
def conditionMet (condition : Option StartCondition) : Bool :=
  match condition with
  | none => true
  | some c => c.satisfied

/-! ## Backoff State -/

/-- Tracks start failure backoff for a principal.
    Backoff doubles from 1 to 30 (capped), cleared by external events. -/
structure BackoffState where
  attempts : Nat
  /-- Time steps remaining until retry is allowed. -/
  cooldown : Nat
  deriving DecidableEq, Repr

/-- Initial backoff after first failure. -/
def initialBackoff : Nat := 1

/-- Maximum backoff duration. -/
def maxBackoff : Nat := 30

/-- Compute backoff duration from attempt count: min(2^attempts, 30). -/
def backoffDuration (attempts : Nat) : Nat :=
  min (2 ^ attempts) maxBackoff

/-- Record a start failure, incrementing backoff. -/
def BackoffState.recordFailure (state : Option BackoffState) : BackoffState :=
  match state with
  | none => { attempts := 0, cooldown := initialBackoff }
  | some s =>
    let nextAttempts := s.attempts + 1
    { attempts := nextAttempts, cooldown := backoffDuration nextAttempts }

/-- Tick the backoff timer by one time step. Returns none if cooldown reached zero. -/
def BackoffState.tick (state : BackoffState) : Option BackoffState :=
  if state.cooldown ≤ 1 then none
  else some { state with cooldown := state.cooldown - 1 }

/-! ## Per-Principal Runtime State -/

/-- The lifecycle state of a single principal from the daemon's perspective. -/
inductive LifecycleState where
  /-- Not tracked by daemon (not running, not failed, not completed). -/
  | absent
  /-- Sandbox is running. -/
  | running
  /-- SIGTERM sent, waiting for graceful exit or timeout. -/
  | draining
  /-- Completed with don't-restart policy. Excluded from desired set
      until config change or daemon restart. -/
  | completed
  /-- Start failed, in exponential backoff. -/
  | startFailed (backoff : BackoffState)
  deriving Repr

/-! ## System State -/

/-- The complete state visible to the reconciliation function. -/
structure SystemState where
  /-- Machine config: which principals are assigned to this machine. -/
  assignments : List Assignment
  /-- Current lifecycle state of each known principal. -/
  lifecycle : Principal → LifecycleState
  /-- Whether a config change occurred since last reconcile
      (clears backoff and completed sets). -/
  configChanged : Bool

/-! ## Desired Set Computation -/

/-- Compute the desired set: principals that should be running.
    A principal is desired when:
    - It appears in assignments with autoStart = true
    - Its start condition is met (absent or satisfied)
    - It has not completed with a don't-restart policy -/
def desiredSet (state : SystemState) : List Principal :=
  state.assignments.filterMap fun assignment =>
    if assignment.autoStart && conditionMet assignment.condition then
      match state.lifecycle assignment.principal with
      | .completed => none  -- Completed principals excluded
      | _ => some assignment.principal
    else
      none

/-- Look up a principal's assignment. -/
def findAssignment (assignments : List Assignment) (p : Principal) : Option Assignment :=
  assignments.find? fun a => a.principal == p

/-! ## Reconcile Step -/

/-- The result of reconciling a single principal. -/
inductive ReconcileAction where
  /-- No action needed (already in correct state). -/
  | noAction
  /-- Create the sandbox. -/
  | create
  /-- Drain and destroy (condition no longer met or removed from config). -/
  | drain
  /-- Destroy immediately (proxy-only, no agent to drain). -/
  | destroy
  /-- Skip: in backoff, retry later. -/
  | backoff
  deriving DecidableEq, Repr

/-- Determine what action to take for a single principal given the desired set.
    This is the core decision function of reconciliation. -/
def reconcileAction
    (lifecycle : LifecycleState)
    (isDesired : Bool)
    (isDynamic : Bool) : ReconcileAction :=
  match lifecycle, isDesired with
  -- Desired and already running: nothing to do
  | .running, true => .noAction
  -- Desired but absent: create it
  | .absent, true => .create
  -- Desired but in startFailed: check if backoff has expired
  | .startFailed b, true =>
    if b.cooldown == 0 then .create  -- Backoff expired: retry
    else .backoff                    -- Still cooling down: wait
  -- Desired but draining: this shouldn't happen in steady state
  -- (draining means we decided to stop it, but now we want it again)
  -- In practice the drain completes and it gets recreated next cycle.
  | .draining, true => .noAction
  -- Completed is already filtered from desired set, but for completeness:
  | .completed, true => .noAction
  -- Running but not desired: drain (unless dynamic)
  | .running, false => if isDynamic then .noAction else .drain
  -- Draining and not desired: already draining, let it finish
  | .draining, false => .noAction
  -- Not running and not desired: nothing to do
  | .absent, false => .noAction
  | .startFailed _, false => .noAction
  | .completed, false => .noAction

/-! ## State Transition -/

/-- Apply the result of creating a sandbox (success or failure).
    On failure, preserves the attempt history from the previous state
    so backoff grows: 1s → 2s → 4s → 8s → 16s → 30s.
    This matches the Go code where d.startFailures[p] persists and
    accumulates across retries. -/
def applyCreateResult
    (lifecycle : LifecycleState)
    (success : Bool) : LifecycleState :=
  if success then .running
  else
    let previousBackoff := match lifecycle with
      | .startFailed b => some b
      | _ => none
    .startFailed (BackoffState.recordFailure previousBackoff)

/-- Apply a sandbox exit event. -/
def applyExit
    (policy : RestartPolicy)
    (outcome : ExitOutcome) : LifecycleState :=
  if shouldRestart policy outcome then .absent  -- Will be recreated next reconcile
  else .completed

/-- Apply a drain completion (sandbox exited during drain or timeout). -/
def applyDrainComplete : LifecycleState := .absent

/-- Clear backoff and completed state (triggered by config change). -/
def clearFailureState (lifecycle : LifecycleState) : LifecycleState :=
  match lifecycle with
  | .startFailed _ => .absent
  | .completed => .absent
  | other => other

/-! ## Full Reconcile Pass -/

/-- Apply config-change clearing to all principals. -/
def clearOnConfigChange (state : SystemState) : SystemState :=
  if state.configChanged then
    { state with
      lifecycle := fun p => clearFailureState (state.lifecycle p)
      configChanged := false }
  else state

/-- Compute the reconcile action for every assigned principal.
    This is a pure function of the current state. -/
def reconcileAllActions (state : SystemState) : List (Principal × ReconcileAction) :=
  let desired := desiredSet state
  state.assignments.map fun assignment =>
    let p := assignment.principal
    let isDesired := desired.contains p
    (p, reconcileAction (state.lifecycle p) isDesired assignment.isDynamic)

/-! ## Properties -/

section Properties

/-- Mutual exclusion is trivially guaranteed by the LifecycleState type:
    a principal is in exactly one state at a time. This is a design property
    of the model — the Go implementation uses separate maps (d.running,
    d.draining, d.completed, d.startFailures), so mutual exclusion there
    depends on correct map management. The fact that this model makes it
    structural is itself a finding: the Go code could benefit from a single
    state enum per principal. -/
theorem lifecycle_mutual_exclusion (s : LifecycleState) :
    (s = .running ∨ s = .draining ∨ s = .completed ∨ s = .absent ∨
     (∃ b, s = .startFailed b)) := by
  cases s with
  | absent => exact Or.inr (Or.inr (Or.inr (Or.inl rfl)))
  | running => exact Or.inl rfl
  | draining => exact Or.inr (Or.inl rfl)
  | completed => exact Or.inr (Or.inr (Or.inl rfl))
  | startFailed b => exact Or.inr (Or.inr (Or.inr (Or.inr ⟨b, rfl⟩)))

/-- A running principal that is still desired will not be drained or destroyed.
    This is the no-regression property for desired principals. -/
theorem desired_running_stable (isDynamic : Bool) :
    reconcileAction .running true isDynamic = .noAction := by
  simp [reconcileAction]

/-- A running non-dynamic principal that is no longer desired will be drained. -/
theorem undesired_running_drains :
    reconcileAction .running false false = .drain := by
  simp [reconcileAction]

/-- A running dynamic principal is never drained by reconcile,
    even when no longer desired. -/
theorem dynamic_never_drained :
    reconcileAction .running false true = .noAction := by
  simp [reconcileAction]

/-- An absent principal that is desired will be created. -/
theorem desired_absent_creates (isDynamic : Bool) :
    reconcileAction .absent true isDynamic = .create := by
  simp [reconcileAction]

/-- Reconcile action is idempotent for running desired principals:
    deciding to do nothing and then deciding again still does nothing. -/
theorem reconcile_running_idempotent (isDynamic : Bool) :
    let action := reconcileAction .running true isDynamic
    action = .noAction := by
  simp [reconcileAction]

/-- clearFailureState is idempotent: clearing twice is the same as clearing once. -/
theorem clear_failure_idempotent (s : LifecycleState) :
    clearFailureState (clearFailureState s) = clearFailureState s := by
  cases s with
  | absent => simp [clearFailureState]
  | running => simp [clearFailureState]
  | draining => simp [clearFailureState]
  | completed => simp [clearFailureState]
  | startFailed b => simp [clearFailureState]

/-- After clearing failure state, a principal is never in startFailed or completed. -/
theorem clear_removes_failures (s : LifecycleState) :
    ∀ b, clearFailureState s ≠ .startFailed b := by
  intro b
  cases s with
  | absent => simp [clearFailureState]
  | running => simp [clearFailureState]
  | draining => simp [clearFailureState]
  | completed => simp [clearFailureState]
  | startFailed _ => simp [clearFailureState]

theorem clear_removes_completed (s : LifecycleState) :
    clearFailureState s ≠ .completed := by
  cases s with
  | absent => simp [clearFailureState]
  | running => simp [clearFailureState]
  | draining => simp [clearFailureState]
  | completed => simp [clearFailureState]
  | startFailed _ => simp [clearFailureState]

/-- 2^n is always at least 1. Helper for backoff reasoning. -/
private theorem two_pow_pos (n : Nat) : 1 ≤ 2 ^ n := by
  induction n with
  | zero => simp
  | succ k ih => calc
      1 ≤ 2 ^ k := ih
      _ ≤ 2 ^ k + 2 ^ k := Nat.le_add_right _ _
      _ = 2 ^ (k + 1) := by rw [Nat.pow_succ]; omega

/-- The backoff duration is always positive. -/
theorem backoff_positive (attempts : Nat) : backoffDuration attempts ≥ 1 := by
  unfold backoffDuration maxBackoff
  simp [Nat.min_def]
  split
  · exact two_pow_pos attempts
  · omega

/-- The backoff duration is bounded by maxBackoff. -/
theorem backoff_bounded (attempts : Nat) : backoffDuration attempts ≤ maxBackoff := by
  simp [backoffDuration]
  exact Nat.min_le_right _ _

/-- shouldRestart is total and covers all cases (no silent fallthrough). -/
theorem restart_decision_total (policy : RestartPolicy) (outcome : ExitOutcome) :
    shouldRestart policy outcome = true ∨ shouldRestart policy outcome = false := by
  cases policy with
  | always => simp [shouldRestart]
  | onFailure => cases outcome with
    | success => simp [shouldRestart]
    | failure => simp [shouldRestart]
  | never => simp [shouldRestart]

end Properties

/-! ## Convergence

  The central question: if a principal is desired and creation eventually
  succeeds, does the reconciliation loop always reach the running state?

  We model this by decomposing the loop into phases:
  1. From absent (or retry-ready), reconcile attempts creation
  2. If creation fails, the principal enters startFailed with incremented
     backoff (attempt count accumulates: 1 → 2 → 4 → 8 → 16 → 30 cap)
  3. Backoff cooldown ticks to zero while preserving attempt history
  4. reconcileAction sees cooldown == 0 and retries creation
  5. The cycle repeats until creation succeeds

  Key modeling decision: backoff does NOT transition through absent. The
  principal stays in startFailed with decreasing cooldown, preserving the
  attempt count across retries. This matches the Go code where
  d.startFailures[p] persists and accumulates. The original model drained
  to absent (losing attempt history), which the convergence proof exposed
  as incorrect — exactly the kind of assumption-surfacing de Moura describes.

  Convergence follows from: backoff terminates (backoff_drains_to_zero) +
  creation eventually succeeds (assumption) → finite steps to running
  (convergence_from_absent, convergence_retry_ready).
-/

section Convergence

/-- One reconcile cycle for a desired, non-dynamic principal.
    Takes a creation outcome for when the action is create. -/
def reconcileCycle
    (state : LifecycleState)
    (creationSucceeds : Bool) : LifecycleState :=
  match reconcileAction state true false with
  | .create => applyCreateResult state creationSucceeds
  | _ => state

/-- One time tick: decrements backoff cooldown toward zero.
    The principal stays in startFailed with cooldown 0 (preserving attempt
    count). Reconcile checks cooldown == 0 to retry. -/
def tickCycle (state : LifecycleState) : LifecycleState :=
  match state with
  | .startFailed b =>
    if b.cooldown == 0 then state
    else .startFailed { b with cooldown := b.cooldown - 1 }
  | other => other

/-- Apply n consecutive tick cycles. -/
def tickN : LifecycleState → Nat → LifecycleState
  | state, 0 => state
  | state, n + 1 => tickN (tickCycle state) n

/-- Ticking a non-startFailed state is a no-op. -/
theorem tick_non_failed (s : LifecycleState) (h : ∀ b, s ≠ .startFailed b) :
    tickCycle s = s := by
  cases s with
  | absent => rfl
  | running => rfl
  | draining => rfl
  | completed => rfl
  | startFailed b => exact absurd rfl (h b)

/-- Ticking running is a no-op. -/
theorem tick_running : tickCycle .running = .running := by
  simp [tickCycle]

/-- Backoff drains to zero: from startFailed with cooldown c,
    exactly c tick steps reach cooldown 0 (retry-ready).
    The attempt count is preserved throughout — this is what makes
    backoff accumulate correctly across retries. -/
theorem backoff_drains_to_zero (a c : Nat) :
    tickN (.startFailed ⟨a, c⟩) c = .startFailed ⟨a, 0⟩ := by
  induction c with
  | zero => simp [tickN]
  | succ k ih =>
    simp only [tickN, tickCycle]
    simp (config := { decide := true })
    exact ih

/-- A startFailed state with cooldown 0 triggers creation on reconcile. -/
theorem expired_backoff_creates (a : Nat) :
    reconcileAction (.startFailed ⟨a, 0⟩) true false = .create := by
  simp [reconcileAction]

/-- Successful creation from any state reaches running. -/
theorem create_success_running (s : LifecycleState) :
    applyCreateResult s true = .running := by
  simp [applyCreateResult]

/-! ### The reconcile loop

  Models the full retry cycle: attempt creation, and if it fails, tick down
  the backoff and try again. The attempt count accumulates, so backoff grows
  exponentially: 1 → 2 → 4 → 8 → 16 → 30 → 30 → ...

  `reconcileLoop n state` performs n failed attempts (each followed by
  backoff drain) and then one successful attempt. -/

/-- One attempt cycle: reconcile (attempt creation), then if failed,
    tick down the resulting backoff to zero for the next retry. -/
def attemptAndDrain (state : LifecycleState) (success : Bool) : LifecycleState :=
  let afterReconcile := reconcileCycle state success
  match afterReconcile with
  | .startFailed b => tickN (.startFailed b) b.cooldown
  | other => other

/-- The full reconcile loop: n failed attempts then one success. -/
def reconcileLoop : Nat → LifecycleState → LifecycleState
  | 0, state => reconcileCycle state true
  | n + 1, state =>
    let afterFailure := attemptAndDrain state false
    reconcileLoop n afterFailure

/-- From absent, a failed attempt followed by backoff drain reaches
    startFailed with cooldown 0 (retry-ready with attempt count 0). -/
theorem absent_fail_drains :
    attemptAndDrain .absent false = .startFailed ⟨0, 0⟩ := by
  simp [attemptAndDrain, reconcileCycle, reconcileAction, applyCreateResult,
        BackoffState.recordFailure, initialBackoff,
        tickN, tickCycle]

/-- From startFailed with cooldown 0, a failed attempt drains to
    startFailed with cooldown 0 and incremented attempt count. -/
theorem retry_fail_drains (a : Nat) :
    attemptAndDrain (.startFailed ⟨a, 0⟩) false =
      .startFailed ⟨a + 1, 0⟩ := by
  simp [attemptAndDrain, reconcileCycle, reconcileAction, applyCreateResult,
        BackoffState.recordFailure, backoffDuration]
  exact backoff_drains_to_zero (a + 1) (min (2 ^ (a + 1)) maxBackoff)

/-- Generalized convergence: from startFailed with cooldown 0 and any
    attempt count, the reconcile loop reaches running.

    This is the strongest convergence result: it shows that regardless
    of how many failures have already occurred (and thus how long the
    backoff has grown), eventual success always reaches running. -/
theorem convergence_retry_ready :
    ∀ n a, reconcileLoop n (.startFailed ⟨a, 0⟩) = .running := by
  intro n
  induction n with
  | zero =>
    intro a
    -- reconcileLoop 0 = reconcileCycle state true
    -- reconcileAction for startFailed ⟨a, 0⟩ with desired=true is .create
    -- applyCreateResult with true is .running
    simp [reconcileLoop, reconcileCycle, reconcileAction, applyCreateResult]
  | succ k ih =>
    intro a
    simp only [reconcileLoop]
    rw [retry_fail_drains]
    exact ih (a + 1)

/-- Convergence from absent: if creation eventually succeeds after n failures,
    the reconcile loop reaches running.

    This is the central safety property of reconciliation: the daemon will
    always eventually start a desired principal, provided the underlying
    failure is transient. The proof decomposes into:
    - First failure transitions absent → startFailed ⟨0, 0⟩ (after drain)
    - Generalized convergence handles the remaining n-1 failures
    - Each failure preserves the attempt count, so backoff grows correctly
    - Backoff is bounded (≤ 30), so worst-case wait is finite -/
theorem convergence_from_absent : ∀ n, reconcileLoop n .absent = .running := by
  intro n
  induction n with
  | zero =>
    -- First attempt succeeds: absent → create → running
    simp [reconcileLoop, reconcileCycle, reconcileAction, applyCreateResult]
  | succ k _ih =>
    -- First attempt fails, drain, then generalized convergence
    simp only [reconcileLoop]
    rw [absent_fail_drains]
    exact convergence_retry_ready k 0

end Convergence
