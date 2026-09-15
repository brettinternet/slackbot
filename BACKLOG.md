# Backlog

Prioritized improvements for reliability, security, operability, and maintainability.

## P0 — Webhook reliability and security

### SB-001: Harden the Slack Events endpoint

Status: Complete

Limit and validate inbound requests before parsing them.

Acceptance criteria:

- Only `POST` requests are accepted on the configured Slack Events path.
- Request bodies have a documented maximum size and oversized requests return `413`.
- Invalid JSON returns `400`; internal failures remain distinguishable from client errors.
- Slack signature verification continues to use the original request body.
- Tests cover method validation, body limits, malformed payloads, signatures, and URL verification.

### SB-002: Acknowledge Slack events without processor backpressure

Status: Complete

Decouple Slack's HTTP acknowledgement deadline from downstream event processing.

Acceptance criteria:

- Valid events receive a successful response without waiting on a busy processor queue.
- Every processor has an explicit bounded queue and overflow policy.
- Overflow and unavailable-processor behavior is logged without leaking message content.
- Shutdown either drains accepted work within its deadline or reports what was abandoned.
- Tests exercise saturated queues, concurrent delivery, and shutdown.

Dependencies: SB-001

### SB-003: Deduplicate Slack event retries

Status: Complete

Prevent Slack retries from producing duplicate side effects.

Acceptance criteria:

- Events are deduplicated by Slack event ID for a configurable retention window.
- The deduplication check is concurrency-safe and bounded in memory.
- Duplicate deliveries are acknowledged successfully but not dispatched again.
- Tests cover duplicates, expiry, concurrency, and events without usable IDs.

Dependencies: SB-002

## P1 — Runtime correctness

### SB-004: Make readiness reflect real dependencies

Status: Complete

Replace the fixed two-second readiness delay with lifecycle-driven state.

Acceptance criteria:

- `/ready` stays unhealthy until required startup work has completed.
- Readiness becomes unhealthy as soon as shutdown begins.
- Required and optional dependencies are clearly defined; a disabled feature cannot block readiness.
- Tests cover startup, dependency failure, disabled services, and shutdown.

### SB-005: Define consistent configuration reload behavior

Status: Complete

Make each setting explicitly reloadable or restart-required.

Acceptance criteria:

- Documentation identifies dynamic and restart-required settings.
- Reloadable AI chat, persona, and shower-thought settings update safely without duplicate workers.
- Restart-required changes emit one actionable warning that names the changed settings.
- Invalid updates preserve the last valid configuration.
- Tests cover enabling, disabling, and changing each service at runtime.

### SB-006: Isolate processor failures

Status: Complete

Ensure a slow or failed feature cannot stall unrelated Slack functionality.

Acceptance criteria:

- Each event processor runs independently with bounded concurrency.
- Panic and error handling cannot terminate dispatch for other processors.
- Queue capacity and overflow behavior are documented.
- Tests prove one stalled processor does not delay another.

Dependencies: SB-002

### SB-007: Tighten service lifecycle and shutdown handling

Status: Complete

Make startup and shutdown behavior deterministic and consistently report failures.

Acceptance criteria:

- Shutdown attempts every initialized service and joins all returned errors.
- `ForceShutdown` has defined behavior or is removed from the lifecycle contract.
- Partially completed setup can be cleaned up safely.
- Repeated shutdown calls are safe.
- Tests cover partial startup, multiple failures, repeated shutdown, and deadline expiry.

Dependencies: SB-004

## P2 — Operability

### SB-008: Add optional Prometheus metrics

Status: Complete

Provide Prometheus instrumentation without requiring it in deployments.

Acceptance criteria:

- Metrics are disabled by default through configuration.
- When disabled, no metrics endpoint is registered and existing behavior is unchanged.
- When enabled, a configurable endpoint exposes Prometheus-format metrics.
- Metrics cover request outcomes, deduplicated events, processor queue depth/overflow, Slack and OpenAI request latency/errors, AI token usage when available, and vibecheck outcomes.
- Metric labels use bounded cardinality and never contain user IDs, channel IDs, message text, or prompts.
- Health and readiness do not depend on a Prometheus server or scraper.
- README and `config.yaml` include an opt-in example, but deployment manifests remain unchanged.
- Tests cover disabled and enabled modes plus representative counters and gauges.

Dependencies: SB-002, SB-003

### SB-009: Improve structured operational logging

Status: Complete

Make failures traceable across webhook receipt and feature processing.

Acceptance criteria:

- Logs include a safe correlation identifier where available.
- Error logs consistently identify the component and operation.
- Secrets, prompts, message bodies, and other sensitive content are excluded by default.
- Tests cover redaction for representative error paths.

Dependencies: SB-002

## P2 — AI data and context

### SB-010: Add AI conversation privacy controls

Status: Complete

Give operators and users control over persisted conversation history.

Acceptance criteria:

- Context retention is configurable and cleanup runs automatically.
- A supported command or administrative operation can clear context by conversation scope.
- Cleanup includes expired persona assignments.
- Documentation states what is stored, where it is stored, and for how long.
- Tests cover retention, scoped deletion, and concurrent reads and cleanup.

### SB-011: Use model-aware token counting

Status: Complete

Replace the four-characters-per-token estimate with counting appropriate for the configured model.

Acceptance criteria:

- Context selection uses an established tokenizer compatible with the configured model.
- Unsupported models use a documented conservative fallback.
- Token accounting includes message framing overhead where applicable.
- Tests cover Unicode, long messages, model fallback, and exact budget boundaries.

### SB-012: Introduce versioned database migrations

Status: Complete

Make persisted schema evolution explicit and repeatable.

Acceptance criteria:

- The database records its schema version.
- Migrations run transactionally and in order during startup.
- Existing databases upgrade without losing conversation or persona data.
- A failed migration leaves the prior schema usable or fails startup with an actionable error.
- Tests upgrade from every supported historical schema.

Dependencies: SB-010

## P3 — Repository hygiene and CI

### SB-013: Keep runtime artifacts out of source directories

Formalize the existing ignore rules and prevent accidental commits of local state.

Acceptance criteria:

- Development commands write binaries, databases, and mutable state only to documented ignored paths.
- CI fails if representative generated artifacts are committed.
- Existing `.gitignore` coverage for `cmd/bot/build`, `cmd/bot/tmp`, databases, and binaries is tested or documented.
- Setup and cleanup documentation identifies the generated paths.

### SB-014: Add focused integration tests for the event pipeline

Exercise the complete signed-request-to-processor path in addition to package unit tests.

Acceptance criteria:

- Tests submit correctly signed Slack fixtures through the HTTP handler.
- Tests cover URL verification, normal dispatch, duplicate retries, saturation, and shutdown.
- Tests are deterministic and require no Slack or OpenAI credentials.

Dependencies: SB-001, SB-002, SB-003, SB-007

## Suggested delivery order

1. SB-001 → SB-002 → SB-003
2. SB-004 → SB-007
3. SB-006 and SB-009
4. SB-008 when metrics are wanted; it remains opt-in
5. SB-010 → SB-012, then SB-011
6. SB-005, SB-013, and SB-014

## Definition of done

Each item is complete when its acceptance criteria are covered by tests, relevant configuration and
operator documentation are updated, `task check` and `task test` pass, and no credentials or runtime
artifacts are committed.
