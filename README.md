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
sudoers rule for the exact fixed commands** — the client never supplies what runs.
See [HANDOVER.md](./HANDOVER.md) for the design + the (non-negotiable) security
invariants.

Consumed by the `homelab` repo as a **flake input**; that repo owns the host
wiring (user, sudoers, sops secret, systemd units, flake-update timer).

## Status

Scaffold — the daemon is implemented per `HANDOVER.md`. `flake.nix` builds both
binaries (`nix build .#`); `nix develop` drops a Go dev shell.

## Config

All configuration is via env vars; every secret also has a `<VAR>_FILE` form
(the host module points these at sops secret paths, `_FILE` wins). See the config
contract in `HANDOVER.md`.
