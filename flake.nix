{
  description = "NixOS switch approval Telegram portal — switchd daemon + request-switch CLI";

  # Minimal inputs on purpose: this daemon is root-capable, so the supply-chain
  # surface is kept small (see HANDOVER.md). Just nixpkgs.
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
      ];
      forAllSystems = nixpkgs.lib.genAttrs systems;
    in
    {
      packages = forAllSystems (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          package = pkgs.callPackage ./nix/package.nix { };
        in
        {
          default = package;
          switchd = package;
          request-switch = package;
        }
      );

      checks = forAllSystems (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          eval = nixpkgs.lib.nixosSystem {
            inherit system;
            modules = [
              self.nixosModules.default
              {
                services.switchd = {
                  enable = true;
                  botTokenFile = "/run/secrets/switchd-bot-token";
                  allowedUserIdsFile = "/run/secrets/switchd-allowed-user-ids";
                  chatIdFile = "/run/secrets/switchd-chat-id";
                  repoDir = "/etc/nixos";
                };
              }
            ];
          };
          sudoRule = builtins.head (
            builtins.filter (rule: rule.users == [ "switchd" ]) eval.config.security.sudo.extraRules
          );
          sudoCommand = (builtins.head sudoRule.commands).command;
          serviceConfig = eval.config.systemd.services.switchd.serviceConfig;
          environment = eval.config.systemd.services.switchd.environment;
          hasActivateCmdOption = eval.options.services.switchd ? activateCmd;
          flakeOnlyEval = nixpkgs.lib.nixosSystem {
            inherit system;
            modules = [
              self.nixosModules.default
              {
                services.switchd = {
                  enable = true;
                  botTokenFile = "/run/secrets/token";
                  allowedUserIdsFile = "/run/secrets/users";
                  chatIdFile = "/run/secrets/chat";
                  flakeRef = "github:example/flake#host";
                };
              }
            ];
          };
          failedAssertions = builtins.filter (
            assertion: !assertion.assertion
          ) flakeOnlyEval.config.assertions;
        in
        {
          unix-group-access = pkgs.testers.runNixOSTest {
            name = "switchd-unix-group-access";
            nodes.machine = { pkgs, ... }: {
              users.groups.switchd = { };
              users.users = {
                operator = {
                  isNormalUser = true;
                  group = "users";
                  extraGroups = [ "switchd" ];
                };
                switchd = {
                  isSystemUser = true;
                  group = "switchd";
                };
              };
              environment.systemPackages = [ pkgs.python3 ];
              systemd.services.switchd-socket-test = {
                wantedBy = [ "multi-user.target" ];
                serviceConfig = {
                  User = "switchd";
                  Group = "switchd";
                  RuntimeDirectory = "switchd-test";
                  ExecStart = "${pkgs.python3}/bin/python ${pkgs.writeText "switchd-socket-test.py" ''
                    import os, socket
                    path = "/run/switchd-test/sock"
                    server = socket.socket(socket.AF_UNIX)
                    server.bind(path)
                    os.chmod(path, 0o660)
                    server.listen(1)
                    connection, _ = server.accept()
                    connection.close()
                  ''}";
                };
              };
            };
            testScript = ''
              start_all()
              machine.succeed("install -d -o operator -g switchd -m 0710 /home/operator")
              machine.succeed("install -d -o operator -g switchd -m 0750 /home/operator/repo")
              machine.succeed("printf readable > /home/operator/repo/flake.nix")
              machine.succeed("chown operator:switchd /home/operator/repo/flake.nix && chmod 0640 /home/operator/repo/flake.nix")
              machine.succeed("runuser -u switchd -- cat /home/operator/repo/flake.nix | grep -Fx readable")
              machine.fail("runuser -u switchd -- sh -c 'printf x >> /home/operator/repo/flake.nix'")
              machine.wait_for_unit("switchd-socket-test.service")
              machine.succeed("runuser -u operator -- python -c 'import socket; s=socket.socket(socket.AF_UNIX); s.connect(\"/run/switchd-test/sock\")'")
            '';
          };

          trusted-path = pkgs.runCommand "switchd-trusted-path" { nativeBuildInputs = [ pkgs.acl ]; } ''
            set -eu

            fail() {
              echo "$1" >&2
              exit 1
            }
            service_uid=4294967294
            supplementary_gid=$(id -g)
            service_gids="4294967294 $supplementary_gid"
            ${builtins.readFile ./nix/trusted-path.sh}

            mkdir -m 0755 trusted
            printf safe > trusted/target
            chmod 0644 trusted/target
            ln -s target trusted/result
            ln -s ${pkgs.hello}/bin/hello trusted/store-result
            require_trusted_path "$PWD/trusted"

            mkdir -m 0750 group-readable
            chgrp "$supplementary_gid" group-readable
            printf safe > group-readable/flake.nix
            chgrp "$supplementary_gid" group-readable/flake.nix
            chmod 0640 group-readable/flake.nix
            require_trusted_path "$PWD/group-readable"
            chmod 0660 group-readable/flake.nix
            if (require_trusted_path "$PWD/group-readable"); then
              echo "service-group-writable source passed trust scan" >&2
              exit 1
            fi

            ln -s /etc/passwd trusted/escape
            if (require_trusted_path "$PWD/trusted"); then
              echo "external symlink passed trust scan" >&2
              exit 1
            fi
            rm trusted/escape

            printf unsafe > trusted/writable
            chmod 0666 trusted/writable
            if (require_trusted_path "$PWD/trusted"); then
              echo "world-writable source entry passed trust scan" >&2
              exit 1
            fi
            rm trusted/writable

            printf unsafe > trusted/supplementary-group
            chgrp "$supplementary_gid" trusted/supplementary-group
            chmod 0660 trusted/supplementary-group
            if (require_trusted_path "$PWD/trusted"); then
              echo "supplementary-group-writable entry passed trust scan" >&2
              exit 1
            fi
            rm trusted/supplementary-group

            expect_acl_rejected() {
              fixture=$1
              if printf '%s\n' "$fixture" | validate_trusted_acl_output "$supplementary_gid"; then
                echo "unsafe ACL fixture passed: $fixture" >&2
                exit 1
              fi
            }
            expect_acl_allowed() {
              fixture=$1
              printf '%s\n' "$fixture" | validate_trusted_acl_output "$supplementary_gid" || {
                echo "safe ACL fixture rejected: $fixture" >&2
                exit 1
              }
            }
            expect_acl_rejected 'user:4294967294:rw-'
            expect_acl_rejected "group:$supplementary_gid:rw-"
            expect_acl_allowed 'user:4294967294:rw-
mask::r--'
            expect_acl_rejected 'user:4294967294:rw-
mask::rw-'
            expect_acl_rejected 'default:user:4294967294:rw-'
            expect_acl_rejected "default:group:$supplementary_gid:rw-"
            expect_acl_allowed 'default:user:4294967294:rw-
default:mask::r--'
            expect_acl_rejected 'default:user:4294967294:rw-
default:mask::rw-'

            printf unsafe > trusted/acl-writable
            chmod 0600 trusted/acl-writable
            if ${pkgs.acl}/bin/setfacl -m u:4294967294:rw trusted/acl-writable 2>/dev/null; then
              if (require_trusted_path "$PWD/trusted"); then
                echo "ACL-writable entry passed trust scan" >&2
                exit 1
              fi
            else
              echo "live ACL regression skipped: build filesystem has no POSIX ACL support" >&2
            fi
            rm trusted/acl-writable

            printf 'gitdir: /tmp/mutable-gitdir\n' > trusted/.git
            if (require_trusted_path "$PWD/trusted"); then
              echo "linked-worktree gitdir indirection passed trust scan" >&2
              exit 1
            fi
            rm trusted/.git

            mkdir trusted/.git
            printf '../mutable-common\n' > trusted/.git/commondir
            if (require_trusted_path "$PWD/trusted"); then
              echo "git commondir indirection passed trust scan" >&2
              exit 1
            fi

            touch $out
          '';

          trusted-flake-ref = pkgs.runCommand "switchd-trusted-flake-ref" { } ''
            set -eu

            fail() {
              printf '%s\n' "$1" >&2
              exit 1
            }
            require_trusted_path() { :; }
            ${builtins.readFile ./nix/trusted-flake-ref.sh}

            expect_rejected() {
              if (require_trusted_flake_ref "$1"); then
                echo "unsafe local flake ref passed: $1" >&2
                exit 1
              fi
            }
            expect_rejected '/etc/nixos?dir=subdir#host'
            expect_rejected 'path:/etc/nixos?rev=mutable#host'
            expect_rejected 'file:///etc/nixos?dir=subdir#host'
            expect_rejected 'git+file:///etc/nixos?ref=main#host'

            require_trusted_flake_ref '/etc/nixos#host'
            [ "$trusted_local_path" = /etc/nixos ]
            [ "$trusted_local_kind" = absolute ]
            [ "$trusted_flake_ref" = '/etc/nixos#host' ]

            require_trusted_flake_ref 'github:example/flake?rev=abc#host'
            [ -z "$trusted_local_path" ]
            [ "$trusted_flake_ref" = 'github:example/flake?rev=abc#host' ]

            mkdir source snapshot source/.git source/nested source/nested/.git
            printf dirty-v1 > source/dirty-file
            ln -s dirty-file source/dirty-link
            printf '[include]\npath=/tmp/evil\n[core]\nworktree=/tmp/worktree\nfsmonitor=/tmp/helper\n' > source/.git/config
            printf /tmp/objects > source/.git/alternates
            printf nested-metadata > source/nested/.git/config
            require_trusted_flake_ref "$PWD/source#host"
            snapshot_trusted_flake_ref "$PWD/snapshot"
            printf dirty-v2 > source/dirty-file
            [ "$(cat snapshot/source/dirty-file)" = dirty-v1 ] || {
              echo "snapshot changed with source after copy" >&2
              exit 1
            }
            [ "$(readlink snapshot/source/dirty-link)" = dirty-file ] || {
              echo "snapshot did not preserve an internal symlink" >&2
              exit 1
            }
            [ ! -e snapshot/source/.git ] || { echo "top-level Git metadata copied" >&2; exit 1; }
            [ ! -e snapshot/source/nested/.git ] || { echo "nested Git metadata copied" >&2; exit 1; }
            [ "$trusted_flake_ref" = "path:$PWD/snapshot/source#host" ] || {
              echo "snapshot flake ref did not force path semantics: $trusted_flake_ref" >&2
              exit 1
            }

            mkdir special special-snapshot
            mkfifo special/fifo
            require_trusted_flake_ref "$PWD/special#host"
            if (snapshot_trusted_flake_ref "$PWD/special-snapshot"); then
              echo "special file passed snapshot validation" >&2
              exit 1
            fi
            touch $out
          '';

          module-sudo-helper =
            pkgs.runCommand "switchd-module-sudo-helper"
              (
                {
                  SWITCHD_ACTIVATE_CMD = eval.config.systemd.services.switchd.environment.SWITCHD_ACTIVATE_CMD;
                  SUDO_COMMAND = sudoCommand;
                  UMask = serviceConfig.UMask;
                  SystemCallArchitectures = serviceConfig.SystemCallArchitectures;
                  SecretEnvNames = builtins.concatStringsSep " " (
                    builtins.filter (name: builtins.match "SWITCHD_(BOT_TOKEN|ALLOWED_USER_IDS|CHAT_ID)" name != null) (
                      builtins.attrNames environment
                    )
                  );
                  SecretFileEnvNames = builtins.concatStringsSep " " (
                    builtins.filter (
                      name: builtins.match "SWITCHD_(BOT_TOKEN|ALLOWED_USER_IDS|CHAT_ID)_FILE" name != null
                    ) (builtins.attrNames environment)
                  );
                  HasActivateCmdOption = if hasActivateCmdOption then "1" else "0";
                  FlakeOnlyFailure =
                    if
                      builtins.any (
                        assertion: builtins.match ".*repoDir.*required.*" assertion.message != null
                      ) failedAssertions
                    then
                      "1"
                    else
                      "0";
                }
                // pkgs.lib.genAttrs [
                  "PrivateTmp"
                  "LockPersonality"
                  "RestrictRealtime"
                  "ProtectControlGroups"
                ] (name: if serviceConfig.${name} then "1" else "0")
                // pkgs.lib.genAttrs [
                  "NoNewPrivileges"
                  "ProtectKernelModules"
                  "ProtectKernelTunables"
                  "PrivateDevices"
                ] (name: if builtins.hasAttr name serviceConfig then "set" else "unset")
              )
              ''
                set -eu

                case "$SWITCHD_ACTIVATE_CMD" in
                  '/run/wrappers/bin/sudo /nix/store/'*'/bin/switchd-activate') ;;
                  *) echo "bad SWITCHD_ACTIVATE_CMD: $SWITCHD_ACTIVATE_CMD" >&2; exit 1 ;;
                esac

                helper=''${SWITCHD_ACTIVATE_CMD#'/run/wrappers/bin/sudo '}
                [ "$SUDO_COMMAND" = "$helper" ] || {
                  echo "sudo command does not match helper: $SUDO_COMMAND != $helper" >&2
                  exit 1
                }

                case "$SUDO_COMMAND" in
                  *'/nix/store/*/bin/switch-to-configuration'*) echo "sudo command still uses wildcard activation" >&2; exit 1 ;;
                esac
                helper_script=$(dirname "$(dirname "$SUDO_COMMAND")")/bin/switchd-activate
                grep -F 'snapshot_trusted_flake_ref "$tmp"' "$helper_script" >/dev/null || {
                  echo "activation helper does not snapshot local source before rebuild" >&2
                  exit 1
                }
                snapshot_line=$(grep -n -F 'snapshot_trusted_flake_ref "$tmp"' "$helper_script" | cut -d: -f1)
                grep -F 'trusted_flake_ref=path:$source_snapshot$ref_suffix' "$helper_script" >/dev/null || {
                  echo "activation helper does not force sanitized path semantics" >&2
                  exit 1
                }
                grep -F -- '--exclude=.git' "$helper_script" >/dev/null || {
                  echo "activation helper does not exclude Git metadata" >&2
                  exit 1
                }
                rebuild_line=$(grep -n -F 'nixos-rebuild build --flake "$trusted_flake_ref"' "$helper_script" | cut -d: -f1)
                [ "$snapshot_line" -lt "$rebuild_line" ] || {
                  echo "activation helper rebuilds before snapshot binding" >&2
                  exit 1
                }

                ${pkgs.lib.concatMapStringsSep "\n"
                  (name: ''
                    [ "${"$"}${name}" = 1 ] || { echo "${name} hardening disabled" >&2; exit 1; }
                  '')
                  [
                    "PrivateTmp"
                    "LockPersonality"
                    "RestrictRealtime"
                    "ProtectControlGroups"
                  ]
                }
                [ "$UMask" = 0077 ] || { echo "UMask hardening mismatch: $UMask" >&2; exit 1; }
                [ "$SystemCallArchitectures" = native ] || { echo "SystemCallArchitectures mismatch: $SystemCallArchitectures" >&2; exit 1; }
                [ -z "$SecretEnvNames" ] || { echo "direct secret env leaked into unit: $SecretEnvNames" >&2; exit 1; }
                [ "$SecretFileEnvNames" = "SWITCHD_ALLOWED_USER_IDS_FILE SWITCHD_BOT_TOKEN_FILE SWITCHD_CHAT_ID_FILE" ] || {
                  echo "secret file env mismatch: $SecretFileEnvNames" >&2
                  exit 1
                }
                [ "$HasActivateCmdOption" = 0 ] || { echo "activateCmd remains user-configurable" >&2; exit 1; }
                [ "$FlakeOnlyFailure" = 1 ] || { echo "flakeRef-only config did not fail repoDir assertion" >&2; exit 1; }
                for setting in NoNewPrivileges ProtectKernelModules ProtectKernelTunables PrivateDevices; do
                  eval "value=\$$setting"
                  [ "$value" = unset ] || { echo "$setting must not be set" >&2; exit 1; }
                done

                touch $out
              '';
        }
      );

      devShells = forAllSystems (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        {
          default = pkgs.mkShell {
            packages = with pkgs; [
              go
              gopls
              gotools
            ];
          };
        }
      );

      nixosModules.default = import ./nix/module.nix;
      nixosModules.switchd = self.nixosModules.default;
    };
}
