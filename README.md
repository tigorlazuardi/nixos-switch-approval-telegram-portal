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

Consumed by the `homelab` repo as a **flake input**. This repo ships the
package and NixOS module; the host repo owns the actual secrets, imports, client
users, and flake-update timer wiring.

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
| `SWITCHD_ACTIVATE_CMD` | module-fixed `/run/wrappers/bin/sudo <switchd-activate-store-path>` | argv prefix set internally by the NixOS module; not user-configurable there. Manual non-Nix deployments may still set the daemon env var |
| `SWITCHD_LOG_DIR` | `/var/log/switchd` | persisted build/switch logs |
| `SWITCHD_METRICS_ADDR` | `127.0.0.1:9464` | Prometheus text metrics; set empty to disable |

Metrics exposed: `switchd_requests_total{mode,outcome}` and
`switchd_pending_requests`. Logs are JSON via `slog` with request IDs/outcomes.

## NixOS module

```nix
{
  inputs.switchd.url = "github:tigorlazuardi/nixos-switch-approval-telegram-portal";

  outputs = { nixpkgs, switchd, ... }: {
    nixosConfigurations.host = nixpkgs.lib.nixosSystem {
      system = "x86_64-linux";
      modules = [
        switchd.nixosModules.default
        ({ config, ... }: {
          services.switchd = {
            enable = true;
            user = "switchd";   # configurable; not baked into the module
            group = "switchd";  # add request-switch users to this group

            botTokenFile = config.sops.secrets.switchd-bot-token.path;
            allowedUserIdsFile = config.sops.secrets.switchd-allowed-user-ids.path;
            chatIdFile = config.sops.secrets.switchd-chat-id.path;

            repoDir = "/etc/nixos";
            # repoDirFile = config.sops.secrets.switchd-repo-dir.path;
            flakeRef = "/etc/nixos#homeserver";
            # flakeRefFile = config.sops.secrets.switchd-flake-ref.path;
          };

          users.users.alice.extraGroups = [ config.services.switchd.group ];
        })
      ];
    };
  };
}
```

Secrets are file-only in the public module: `botTokenFile`,
`allowedUserIdsFile`, and `chatIdFile` are required so secret values never enter
Nix derivations, store paths, or generated unit environment. `repoDir` or
`repoDirFile` is independently required because the daemon needs it for Git
status/log context; `flakeRef`/`flakeRefFile` only overrides what gets built.
Set either direct or file form for each non-secret path, not both. The module
manages the service user/group, socket/log directories, and a
scoped sudo rule for only the module-generated `switchd-activate` helper's exact
store path. After approval, the daemon supplies the pre-approved
`<toplevel>/bin/switch-to-configuration switch`; the root helper treats that
path only as the expected value, rebuilds the module-configured flake as root
with a private out-link, compares the rebuilt toplevel exactly to the
pre-approved toplevel, and activates only the rebuilt toplevel. No wildcard
store activation command is permitted in sudoers. The module fixes
`SWITCHD_ACTIVATE_CMD` to exactly `/run/wrappers/bin/sudo` plus its generated
helper store path; no module option can replace that command.

The service has conservative systemd hardening that should not block activation:
`PrivateTmp`, `UMask=0077`, native syscall architecture, locked personality, no
realtime scheduling, and protected control-group settings. It intentionally does
**not** set `NoNewPrivileges=true`, private devices, or kernel tunable/module
protection because activation inherits the service sandbox and must call sudo
and write NixOS activation kernel/device state.

The trusted flake source is part of the root boundary. `repoDir`, `repoDirFile`,
`flakeRefFile`, and any local path in `flakeRef` must resolve to canonical absolute
paths with no symlinks and must not be owned by the service user or writable by
that user, any of its primary/supplementary groups, ACL grants, or world. ACL
inspection fails closed. The helper checks every ancestor and local source-tree
entry, then copies a local flake into a root-owned `0700` snapshot under `/run`,
excluding every `.git` entry and forcing `path:` flake semantics so Git config,
includes, hooks/helpers, worktrees, and object alternates are never consumed by
the root build. The daemon uses the same sanitized-path policy before approval;
dirty and untracked filesystem content is preserved. Local absolute, `path:`,
`file:`, and `git+file:` refs containing `?` are rejected because query parameters
can change flake semantics beyond the validated path. Remote flake refs retain
Nix's native resolution semantics.

Trust assumption: any non-service actor permitted to write the configured source
is an authorized trusted source writer, equivalent to root for approved switches.
The scanner prevents mutation by the daemon user, its groups, ACL grants, and
world; it does not claim protection from concurrent root/authorized-admin edits.
