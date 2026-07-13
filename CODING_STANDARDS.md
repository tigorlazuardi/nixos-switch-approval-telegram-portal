# Coding Standards

This repo is a small, root-capable Go daemon. Security invariants in `HANDOVER.md` are binding; when in doubt, make the diff smaller and the privilege surface narrower.

## Formatting

Use standard Go formatting: `gofmt` for every Go file. `go vet ./...` must stay clean.

## Dependencies

- Prefer the Go standard library. Do not add a Telegram bot framework.
- Keep `go.mod` dependency-free unless a requirement is impossible or unsafe with stdlib.
- If a dependency is added, explain the security reason in the PR/commit and update `flake.nix` `vendorHash`.

## Layout

- `cmd/switchd` owns daemon startup, config loading, socket server, Telegram polling, request state, and command execution.
- `cmd/request-switch` owns CLI parsing, socket connection, frame streaming, and exit-code propagation.
- If shared code becomes necessary, put only truly shared protocol/config helpers under `internal/`; do not create abstractions for one caller.

## Security rules

- Never execute a shell string. Use `exec.Command` with an argv vector.
- The client request may carry only `mode` and `reason`; it must never choose commands, flake refs, or paths.
- The daemon runs only fixed configured actions from `HANDOVER.md`: build/diff before approval, switch after approval.
- Telegram callbacks must be approved only when `callback_query.from.id` is in `SWITCHD_ALLOWED_USER_IDS`.
- Approval IDs are one-shot nonces. Consume on approve, reject, or expiry.
- Treat `reason` and Telegram text as untrusted display text: escape for Telegram, never pass to exec, never use as a metric label.
- Never log bot tokens or secret file contents.

## Config

- Every config value comes from env.
- For secrets, support both `VAR` and `VAR_FILE`; `VAR_FILE` wins.
- Defaults must match `HANDOVER.md`.
- Invalid config should fail fast with a clear error before opening the socket or polling Telegram.

## Errors and logging

- Return errors with context using `fmt.Errorf("...: %w", err)`.
- Log with `log/slog` JSON in daemon code.
- Logs for request work must include `request_id`, `mode`, and final `outcome` where available.

## Protocol

- Keep the Unix-socket protocol line-delimited JSON unless a stronger need appears.
- Every sync request gets terminal frame with an exit code.
- Unknown frame/message types must fail closed with a useful error.

## Testing

- Add small tests for parsing, framing, nonce/allow-list behavior, and config `_FILE` precedence.
- Do not mock the whole world. Unit-test pure helpers; use tiny local socket/process seams only where needed.

## Documentation

- README must list finalized env vars, show a sync CLI example, and state the host-side sudoers/systemd/sops contract lives in the homelab repo.
- Public docs must not include real tokens, chat IDs, or user IDs.

The Fowler smell baseline from the `code-review` skill still applies below these standards. Where this document and the baseline disagree, this document wins.

The first ticket touched in any area sets the living pattern for that area. Reviewers check new code against both this document and the first working code; disagreement means the standard may need updating.
