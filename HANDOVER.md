# Handover — NixOS Switch Approval Telegram Portal

Implementation brief for the coding agent. This repo builds a **Go daemon**
(`switchd`) + a **CLI** (`request-switch`) that let an LLM agent (or a systemd
timer) request a `nixos-rebuild switch` which the operator approves from
**Telegram** — so a mobile operator never types the long sudo password.

The daemon is **root-capable but least-privilege**. Read the Security section
first; its invariants are non-negotiable.

> This repo produces the binaries + a `flake.nix`. The NixOS host wiring
> (`services/switch-daemon.nix`: the `switchd` user, scoped sudoers, the sops
> secret, the systemd units, the flake-update timer) lives in the separate
> `homelab` repo and consumes THIS repo as a flake input. Keep the two contracts
> (below) stable.

## Goal

Two layers over one daemon:

1. **Sync (agent) request** — an LLM agent runs `request-switch`, which **blocks**
   until the operator approves in Telegram; the daemon then runs the switch and
   **streams the build/activation logs back** to the CLI (→ the agent's stdout,
   for failure introspection). Motivation: mobile approval, no password typing.
2. **Async (timer) request** — a systemd timer (host side) runs `nix flake update`
   on fast-moving inputs; if the lock changed it submits an **async** request:
   fire-and-forget, the operator approves whenever, the daemon then switches and
   reports the result to Telegram (no attached caller).

## Decided design (locked in the grill)

- **Language:** Go. **Minimal deps** — Telegram via stdlib `net/http` + `encoding/json`
  (do NOT pull a bot framework). Zero external modules is the target so
  `buildGoModule` can use `vendorHash = null`. This small supply-chain surface is
  a deliberate security property of a root-capable daemon.
- **Two binaries:** `cmd/switchd` (daemon) + `cmd/request-switch` (client CLI).
- **Transport agent↔daemon:** **unix domain socket** (default `/run/switchd/sock`),
  group-restricted perms. The CLI connects, sends a request (`{mode, reason}`),
  then reads a stream of framed messages (status updates, log lines) ending in an
  exit code. No TCP listener.
- **Telegram receive:** **long-poll `getUpdates`** (outbound only, no public
  webhook). Inline keyboard with **Approve / Reject** buttons.
- **flake.nix REQUIRED** (the host consumes this repo as a flake input):
  - `packages.default` (or named) building both binaries via `buildGoModule`
    (`vendorHash = null` while dependency-free).
  - `packages.switchd` + `packages.request-switch` if split is convenient.
  - a `devShells.default` with `go`, `gopls`, `gotools`.
  - `x86_64-linux` is the only target that must work (the host arch).
- **Config: everything via env, secrets via `*_FILE`.** The host exposes ALL
  config through the NixOS module — env vars for plain values, and a `<VAR>_FILE`
  variant for every secret (the module points these at sops secret file paths).
  A `_FILE` var wins over its plain counterpart. See Config contract.
- **New Telegram bot** — its own token + a separate sops secret in homelab (NOT
  the smartd bot).

## Security (invariants — do NOT weaken)

The daemon can cause a root `nixos-rebuild switch`. Containment:

1. **The client never supplies a command or flake.** The daemon executes ONE
   hardcoded action shape: `nixos-rebuild switch --flake <REPO_FLAKE_REF>` (ref
   from config, not from the request). The request carries only `mode` + a
   free-text `reason`. This is what removes RCE: submitting a request can, at
   worst, spam approval prompts — it can never choose what runs.
2. **Execution is gated solely on an Approve from an allow-listed Telegram user
   id.** Verify `callback_query.from.id ∈ ALLOWED_USER_IDS`. Ignore approvals from
   anyone else. A leaked bot token does not grant approval (the attacker is not
   you pressing the button).
3. **Least privilege.** The daemon runs as a **non-root** user (`switchd`). It
   invokes the privileged action through a **scoped sudoers/polkit** rule that
   permits ONLY the exact fixed commands (`nixos-rebuild switch --flake <ref>` and
   `nix flake update ...` in the fixed repo dir), NOPASSWD, fixed args. If the
   daemon is exploited, blast radius = those two commands, not a root shell. The
   daemon must invoke them as an argv vector (`exec`), never through a shell
   string.
4. **Request↔approval binding via nonce.** Each request gets an unguessable id;
   the inline-keyboard callback carries it. An old Approve button must not trigger
   a new/different switch (no replay). One-shot: a nonce is consumed on
   approve/reject/expiry.
5. **No shell interpolation of client/telegram strings.** `reason` and any text
   shown in Telegram is HTML/Markdown-escaped for display and NEVER passed to a
   shell. All exec is argv-vector.
6. **Secrets least-exposed.** Bot token + allowed-user-id loaded from `*_FILE`
   paths owned `switchd:switchd 0400` (sops on the host). Never log the token.
   Socket perms restrict who can submit.
7. **Informed approval.** The approval message shows WHAT would change (see
   Approval UX) so the operator does not rubber-stamp a planted change.

## Approval UX (default — confirm/adjust)

On a request, before asking for approval, the daemon should **build first, then
ask**:

1. Run `nixos-rebuild build --flake <ref>` (produces `./result` / a toplevel path,
   no activation, no root needed for build).
2. Compose the Telegram approval message: the `reason`, `git -C <repo> log
   --oneline -5`, the dirty-file list (`git status --porcelain`), and a change
   summary from **`nvd diff /run/current-system <new-toplevel>`** (package
   adds/removes/upgrades). Inline keyboard: **Approve / Reject**.
3. On **Approve** → run `nixos-rebuild switch` activating the already-built
   toplevel (fast, no rebuild) via the scoped-sudo command; stream logs.
4. On **Reject** / timeout → abort, report.

Rationale: operator approves informed; activation is fast because the build
already happened. Cost: a build runs even if later rejected (acceptable — it is
how you produce the diff).

## Open decisions (defaults chosen — flag if you disagree)

- **What gets switched:** the **working tree** at `REPO_DIR` (default
  `/home/homeserver/homelab`), ref `<REPO_DIR>#homeserver`. Matches the manual
  flow (dirty tree included). Configurable.
- **Timeouts:** sync (agent) approval window default **30 min**; async (timer)
  window default **24 h**. Expiry → auto-reject + notify.
- **Concurrency:** **single-flight** — one pending/active switch at a time. A new
  request while one is pending is **rejected with "busy"** (simpler than a queue).
  Revisit if it bites.
- **Log streaming:** sync → line-framed stdout+stderr to the CLI, final frame =
  exit code; also persist the full log to a file. async → stream a tail / on
  failure the last N lines to Telegram, full log to a file, path in the message.
- **flake-update timer scope (host side):** `nix flake update <inputs>` for a
  configurable fast-mover list (default: `herdr`, `llm-agents`); if `flake.lock`
  changed → submit async request. (Timer lives in the homelab module, not here,
  but the daemon must accept an async request carrying the lock diff summary.)

## Telemetry (OpenTelemetry — part of "done")

Export OTLP to the host Alloy gateway (`OTEL_EXPORTER_OTLP_ENDPOINT`, default the
homelab Alloy `http://127.0.0.1:4318`). Service name `switchd`.

- **Metrics:**
  - Counter `switchd_requests_total{mode=sync|async, outcome=approved|rejected|expired|failed|busy}`.
  - Histogram `switchd_switch_duration_seconds` (build+activate), buckets
    `[1, 10, 60, 300, 900, 1800, 3600]` — a switch spans seconds to tens of
    minutes; default OTel buckets are wrong here.
  - UpDownCounter/Gauge `switchd_pending_requests`.
- **Logs:** structured (`slog` JSON). Every line carries `request_id`, `mode`,
  `outcome`. Correlate with the span when tracing is on.
- **Traces (nice-to-have):** one span per request → child spans `build`,
  `await_approval`, `activate`. Record errors on the span, status ERROR on fail.
- **Sensitive data:**
  - **Tier A (always redact / never emit):** the bot token — never logged, never
    in a span attribute.
  - **Tier B (keep visible):** the Telegram operator `user_id` and `request_id` —
    single-operator infra, they are the debug join key.
  - **Tier D (free text):** `reason` — log it (single-operator, low risk) but
    display-escape in Telegram; never shell-interpolate. Keep it OFF metric labels
    (unbounded → cardinality).
- **Acceptance:** trigger a request, watch the span/log/metrics arrive in the
  host Grafana/Loki/Tempo with the expected attributes — not just tests passing.

## Config contract (env + `*_FILE`)

Every value below is set by the homelab NixOS module. Secrets use the `_FILE`
form (module points at a sops secret path); a `_FILE` var overrides its plain
counterpart. Suggested names (finalize + document in README):

| var | meaning | secret? |
| --- | --- | --- |
| `SWITCHD_BOT_TOKEN` / `SWITCHD_BOT_TOKEN_FILE` | Telegram bot token | **yes** (`_FILE`, sops) |
| `SWITCHD_ALLOWED_USER_IDS` / `..._FILE` | comma-list of approver Telegram user ids | yes (`_FILE`) |
| `SWITCHD_CHAT_ID` / `..._FILE` | chat to post approval messages to | yes (`_FILE`) |
| `SWITCHD_SOCKET_PATH` | unix socket path | no (default `/run/switchd/sock`) |
| `SWITCHD_REPO_DIR` | flake repo working dir | no (default `/home/homeserver/homelab`) |
| `SWITCHD_FLAKE_REF` | ref to switch | no (default `<REPO_DIR>#homeserver`) |
| `SWITCHD_SYNC_TIMEOUT` / `SWITCHD_ASYNC_TIMEOUT` | approval windows | no |
| `SWITCHD_REBUILD_CMD` | the scoped-sudo wrapper to invoke (e.g. `sudo nixos-rebuild`) | no |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Alloy OTLP | no |

## Host integration contract (separate homelab task — do NOT build here)

For reference so the interfaces stay stable. In the `homelab` repo:

- Add this repo as a flake input; new `services/switch-daemon.nix`:
  - user `switchd` (non-login), group for socket access.
  - `security.sudo` (or polkit) rule: `switchd` may run ONLY
    `nixos-rebuild switch --flake <ref>` and `nix flake update ...` NOPASSWD.
  - `systemd.tmpfiles` for the socket dir (`/run/switchd`, `switchd`-owned).
  - sops `secrets/switch-portal.yaml` (bot_token, allowed_user_ids, chat_id),
    owner `switchd`, exposed via the `*_FILE` env vars.
  - `systemd.services.switchd` (the daemon, OTLP env, config env).
  - `systemd.timers.switchd-flake-update` (+ service) for the async layer.
  - `request-switch` on the interactive user's PATH.
  - a `.claude/rules` entry instructing agents to use `request-switch` instead of
    handing a paste-the-command switch.

## First steps for the implementing agent

1. Flesh `go.mod` (module `github.com/tigorlazuardi/nixos-switch-approval-telegram-portal`,
   Go 1.23+), keep zero external deps if feasible.
2. Implement `cmd/switchd` (socket server + telegram long-poll + build/approve/
   switch state machine + streaming + telemetry) and `cmd/request-switch` (connect,
   send, stream to stdout, propagate exit code).
3. Make `flake.nix` build both (`vendorHash = null`) + a devShell.
4. `nix build .#` must succeed; `go vet ./...` clean.
5. Update README with the finalized config var names + a usage example.
6. Coordinate the host contract above with the homelab repo (separate task).
