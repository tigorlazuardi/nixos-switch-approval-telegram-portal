{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.services.switchd;

  package = pkgs.callPackage ./package.nix { };

  fileEnv = env: file: lib.optionalAttrs (file != null) { ${env + "_FILE"} = toString file; };

  valueOrFile =
    env: value: file:
    lib.optionalAttrs (value != null) { ${env} = toString value; } // fileEnv env file;

  trustedFlakeRefScript = ''
    ${lib.optionalString (cfg.repoDir != null) ''
      configured_repo_dir=${lib.escapeShellArg cfg.repoDir}
      require_trusted_path "$configured_repo_dir"
    ''}
    ${lib.optionalString (cfg.repoDirFile != null) ''
      repo_dir_file=${lib.escapeShellArg (toString cfg.repoDirFile)}
      require_trusted_path "$repo_dir_file"
      configured_repo_dir=$(read_one_line "$repo_dir_file" "repoDirFile")
      require_trusted_path "$configured_repo_dir"
    ''}
    ${lib.optionalString (cfg.flakeRefFile != null) ''
      flake_ref_file=${lib.escapeShellArg (toString cfg.flakeRefFile)}
      require_trusted_path "$flake_ref_file"
      configured_flake_ref=$(read_one_line "$flake_ref_file" "flakeRefFile")
    ''}
    ${lib.optionalString (cfg.flakeRef != null) ''
      configured_flake_ref=${lib.escapeShellArg cfg.flakeRef}
    ''}

    ${
      if cfg.flakeRefFile != null || cfg.flakeRef != null then
        ''require_trusted_flake_ref "$configured_flake_ref"''
      else if cfg.repoDirFile != null || cfg.repoDir != null then
        ''require_trusted_flake_ref "$configured_repo_dir#homeserver"''
      else
        ''fail "services.switchd.flakeRef, flakeRefFile, repoDir, or repoDirFile is required"''
    }
  '';

  activateHelper = pkgs.writeShellScriptBin "switchd-activate" ''
        set -eu

        PATH=${
          lib.makeBinPath [
            pkgs.acl
            pkgs.coreutils
            pkgs.findutils
            pkgs.gawk
            pkgs.glibc
            pkgs.gnused
            pkgs.gnutar
            config.nix.package
            pkgs.git
            pkgs.nixos-rebuild
          ]
        }
        export PATH

        fail() {
          printf 'switchd-activate: %s\n' "$1" >&2
          exit 1
        }

        read_one_line() {
          file=$1 name=$2
          value=$(cat -- "$file")
          [ -n "$value" ] || fail "$name is empty"
          line_count=$(awk 'END { print NR }' "$file")
          [ "$line_count" -eq 1 ] || fail "$name must contain exactly one line"
          printf '%s\n' "$value"
        }

        service_uid=$(id -u ${lib.escapeShellArg cfg.user})
        service_gids=$(id -G ${lib.escapeShellArg cfg.user})
        [ -n "$service_gids" ] || fail "service group memberships could not be resolved"

        ${builtins.readFile ./trusted-path.sh}

        ${builtins.readFile ./trusted-flake-ref.sh}

        [ "$#" -eq 2 ] || fail "expected: <toplevel>/bin/switch-to-configuration switch"
        [ "$2" = switch ] || fail "second argument must be switch"

        switch_to_configuration=$1
        case "$switch_to_configuration" in
          /nix/store/*/bin/switch-to-configuration) ;;
          *) fail "first argument must be a Nix store switch-to-configuration path" ;;
        esac

        expected_toplevel=''${switch_to_configuration%/bin/switch-to-configuration}
        [ "$expected_toplevel/bin/switch-to-configuration" = "$switch_to_configuration" ] || fail "switch-to-configuration must be directly below the toplevel bin directory"

        ${trustedFlakeRefScript}
        [ -n "$trusted_flake_ref" ] || fail "trusted flake ref is empty"

        tmp=$(mktemp -d /run/switchd-activate.XXXXXXXXXX)
        chmod 0700 "$tmp"
        cleanup() {
          rm -rf -- "$tmp"
        }
        trap cleanup EXIT HUP INT TERM

        # ponytail: local refs use a root-owned exact snapshot; remote refs retain native Nix pinning semantics.
        snapshot_trusted_flake_ref "$tmp"
        trusted_flake_path=''${trusted_flake_ref%%#*}
        trusted_flake_fragment=''${trusted_flake_ref#*#}
        [ "$trusted_flake_path" != "$trusted_flake_ref" ] || fail "trusted flake ref must contain a fragment"
        [ -n "$trusted_flake_path" ] || fail "trusted flake ref path is empty"
        case "$trusted_flake_path" in -*) fail "trusted flake ref must not begin with an option prefix" ;; esac
        [ -n "$trusted_flake_fragment" ] || fail "trusted flake ref fragment is empty"
        case "$trusted_flake_fragment" in *'#'*) fail "trusted flake ref must contain exactly one fragment separator" ;; esac
        case "$trusted_flake_fragment" in *[![:print:]]*) fail "trusted flake fragment contains unsupported characters" ;; esac
        escaped_flake_fragment=$(printf '%s' "$trusted_flake_fragment" | sed 's/\\/\\\\/g; s/"/\\"/g; s/[$][{]/\\&/g')
        trusted_installable="$trusted_flake_path#nixosConfigurations.\"$escaped_flake_fragment\".config.system.build.toplevel"
        ${config.nix.package}/bin/nix build --out-link "$tmp/result" "$trusted_installable"
        [ -L "$tmp/result" ] || fail "Nix did not create the toplevel out-link"
        trusted_toplevel=$(readlink -f -- "$tmp/result")
        case "$trusted_toplevel" in /nix/store/*) ;; *) fail "rebuilt toplevel is not in the Nix store" ;; esac

        if [ "$trusted_toplevel" != "$expected_toplevel" ]; then
          printf 'switchd-activate: rebuilt toplevel mismatch\n' >&2
          printf 'switchd-activate: expected %s\n' "$expected_toplevel" >&2
          printf 'switchd-activate: rebuilt  %s\n' "$trusted_toplevel" >&2
          exit 1
        fi

        cleanup
        trap - EXIT HUP INT TERM
        exec "$trusted_toplevel/bin/switch-to-configuration" switch
  '';

  activateHelperPath = "${activateHelper}/bin/switchd-activate";

  env =
    fileEnv "SWITCHD_BOT_TOKEN" cfg.botTokenFile
    // fileEnv "SWITCHD_ALLOWED_USER_IDS" cfg.allowedUserIdsFile
    // fileEnv "SWITCHD_CHAT_ID" cfg.chatIdFile
    // valueOrFile "SWITCHD_REPO_DIR" cfg.repoDir cfg.repoDirFile
    // valueOrFile "SWITCHD_FLAKE_REF" cfg.flakeRef cfg.flakeRefFile
    // {
      SWITCHD_SOCKET_PATH = cfg.socketPath;
      SWITCHD_SYNC_TIMEOUT = cfg.syncTimeout;
      SWITCHD_ASYNC_TIMEOUT = cfg.asyncTimeout;
      SWITCHD_ACTIVATE_TIMEOUT = cfg.activateTimeout;
      SWITCHD_ACTIVATE_CMD = "/run/wrappers/bin/sudo ${activateHelperPath}";
      SWITCHD_LOG_DIR = cfg.logDir;
      SWITCHD_METRICS_ADDR = cfg.metricsAddr;
    };

  hasOne = value: file: value != null || file != null;
  hasBoth = value: file: value != null && file != null;
in
{
  options.services.switchd = {
    enable = lib.mkEnableOption "switchd Telegram-approved NixOS switch daemon";

    package = lib.mkOption {
      type = lib.types.package;
      default = package;
      defaultText = lib.literalExpression "pkgs.callPackage ./nix/package.nix { }";
      description = "Package providing the switchd and request-switch binaries.";
    };

    user = lib.mkOption {
      type = lib.types.str;
      default = "switchd";
      description = "User that runs switchd.";
    };

    group = lib.mkOption {
      type = lib.types.str;
      default = "switchd";
      description = "Primary group for switchd and the Unix socket.";
    };

    botTokenFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      description = "File containing the Telegram bot token.";
    };
    allowedUserIdsFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      description = "File containing allowedUserIds.";
    };
    chatIdFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      description = "File containing chatId.";
    };
    repoDir = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      description = "Repository working tree to build and switch.";
    };
    repoDirFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      description = "File containing repoDir.";
    };
    flakeRef = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      description = "Fixed flake ref to build; defaults in switchd to <repoDir>#homeserver.";
    };
    flakeRefFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      description = "File containing flakeRef.";
    };

    socketPath = lib.mkOption {
      type = lib.types.str;
      default = "/run/switchd/sock";
      description = "Unix socket path.";
    };
    syncTimeout = lib.mkOption {
      type = lib.types.str;
      default = "30m";
      description = "Sync request build and approval timeout.";
    };
    asyncTimeout = lib.mkOption {
      type = lib.types.str;
      default = "24h";
      description = "Async request build and approval timeout.";
    };
    activateTimeout = lib.mkOption {
      type = lib.types.str;
      default = "30m";
      description = "Activation timeout after approval; 0 disables the deadline.";
    };
    logDir = lib.mkOption {
      type = lib.types.str;
      default = "/var/log/switchd";
      description = "Directory for persisted switch logs.";
    };
    metricsAddr = lib.mkOption {
      type = lib.types.str;
      default = "127.0.0.1:9464";
      description = "Prometheus metrics listen address; empty disables metrics.";
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = [
      {
        assertion = cfg.botTokenFile != null;
        message = "services.switchd.botTokenFile is required; direct secrets are unsupported.";
      }
      {
        assertion = cfg.allowedUserIdsFile != null;
        message = "services.switchd.allowedUserIdsFile is required; direct secrets are unsupported.";
      }
      {
        assertion = cfg.chatIdFile != null;
        message = "services.switchd.chatIdFile is required; direct secrets are unsupported.";
      }
      {
        assertion = hasOne cfg.repoDir cfg.repoDirFile;
        message = "services.switchd.repoDir or repoDirFile is required independently of flakeRef.";
      }
      {
        assertion = !hasBoth cfg.repoDir cfg.repoDirFile;
        message = "Set only one of services.switchd.repoDir or repoDirFile.";
      }
      {
        assertion = !hasBoth cfg.flakeRef cfg.flakeRefFile;
        message = "Set only one of services.switchd.flakeRef or flakeRefFile.";
      }
    ];

    users.groups.${cfg.group} = { };
    users.users.${cfg.user} = {
      isSystemUser = true;
      group = cfg.group;
    };

    systemd.tmpfiles.rules = [
      "d ${builtins.dirOf cfg.socketPath} 0750 ${cfg.user} ${cfg.group} - -"
      "d ${cfg.logDir} 0750 ${cfg.user} ${cfg.group} - -"
    ];

    security.sudo.extraRules = [
      {
        users = [ cfg.user ];
        runAs = "root";
        commands = [
          {
            command = activateHelperPath;
            options = [ "NOPASSWD" ];
          }
        ];
      }
    ];

    systemd.services.switchd = {
      description = "Telegram-approved NixOS switch daemon";
      wantedBy = [ "multi-user.target" ];
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];
      path = [
        config.nix.package
        pkgs.git
        pkgs.nixos-rebuild
        pkgs.nvd
      ];
      environment = env;
      serviceConfig = {
        ExecStart = "${cfg.package}/bin/switchd";
        User = cfg.user;
        Group = cfg.group;
        Restart = "on-failure";
        PrivateTmp = true;
        UMask = "0077";
        LockPersonality = true;
        RestrictRealtime = true;
        SystemCallArchitectures = "native";
        ProtectControlGroups = true;
      };
    };
  };
}
