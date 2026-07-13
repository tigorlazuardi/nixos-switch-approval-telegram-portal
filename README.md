# nixos-switch-approval-telegram-portal

A least-privilege Go daemon (`switchd`) + CLI (`request-switch`) that let an LLM
agent — or a systemd timer — request a `nixos-rebuild switch` which the operator
**approves from Telegram**. Built for a mobile operator who doesn't want to type a
long sudo password.

- **Sync:** an agent runs `request-switch`, blocks until you tap **Approve** in
  Telegram, then gets the build/activation logs streamed back for introspection.
- **Async:** a timer runs `nix flake update` on fast-moving inputs and, if the
  lock changed, asks you to approve the switch whenever.

The daemon runs as a **non-root** user and reaches root only through a **scoped
sudoers rule for the exact fixed activation command** — the client never supplies
what runs. See [HANDOVER.md](./HANDOVER.md) for the design + the
(non-negotiable) security invariants.

Consumed by the `homelab` repo as a **flake input**; that repo owns the host
wiring (user, sudoers, sops secret, systemd units, flake-update timer).

## Status

Core daemon + CLI are implemented with Go stdlib only. `flake.nix` builds both
binaries (`nix build .#`); `nix develop` drops a Go dev shell.

## Usage

```sh
request-switch "upgrade homelab after flake update"
request-switch -mode async "timer updated fast-moving inputs"
```

The CLI sends only `{mode, reason}` to the Unix socket. The daemon chooses the
configured repo/flake and command; clients never provide commands or paths.

## Config

All configuration is via env vars; every secret also has a `<VAR>_FILE` form
(the host module points these at sops secret paths, `_FILE` wins):

| var | default | notes |
| --- | --- | --- |
| `SWITCHD_BOT_TOKEN` / `_FILE` | required | Telegram bot token; never logged |
| `SWITCHD_ALLOWED_USER_IDS` / `_FILE` | required | comma-separated approver Telegram user IDs |
| `SWITCHD_CHAT_ID` / `_FILE` | required | chat where approval messages are sent |
| `SWITCHD_SOCKET_PATH` | `/run/switchd/sock` | Unix socket, chmod `0660` |
| `SWITCHD_REPO_DIR` / `_FILE` | required | privacy-sensitive working tree path to build/switch; host module must require either direct value or file |
| `SWITCHD_FLAKE_REF` / `_FILE` | `<repo>#homeserver` | fixed flake ref used by daemon; `_FILE` supported for path privacy |
| `SWITCHD_SYNC_TIMEOUT` | `30m` | sync build + approval window; not reused for activation |
| `SWITCHD_ASYNC_TIMEOUT` | `24h` | async build + approval window; not reused for activation |
| `SWITCHD_ACTIVATE_TIMEOUT` | `30m` | activation timeout started only after approval; set `0` for no artificial activation deadline |
| `SWITCHD_ACTIVATE_CMD` | `sudo` | argv prefix used to run the already-built toplevel's `bin/switch-to-configuration switch` |
| `SWITCHD_LOG_DIR` | `/var/log/switchd` | persisted build/switch logs |
| `SWITCHD_METRICS_ADDR` | `127.0.0.1:9464` | Prometheus text metrics; set empty to disable |

Metrics exposed: `switchd_requests_total{mode,outcome}` and
`switchd_pending_requests`. Logs are JSON via `slog` with request IDs/outcomes.

Host-side user, sudoers/polkit, sops secrets, systemd units, and timer wiring live
in the separate `homelab` repo. Its privilege rule should allow `switchd` to
activate the already-built immutable toplevel (`sudo /nix/store/.../bin/switch-to-configuration switch`), not run a second `nixos-rebuild switch --flake` after approval.
