service_has_gid() {
  wanted_gid=$1
  for member_gid in $service_gids; do
    [ "$member_gid" = "$wanted_gid" ] && return 0
  done
  return 1
}

is_untrusted_mode() {
  uid=$1 gid=$2 mode=$3
  # Service-owned paths are mutable via chmod even when owner-write is clear.
  [ "$uid" = "$service_uid" ] && return 0
  service_has_gid "$gid" && [ $((mode & 0020)) -ne 0 ] && return 0
  [ $((mode & 0002)) -ne 0 ] && return 0
  return 1
}

acl_awk_program='
  BEGIN {
    split(service_gids, gids, " ")
    for (i in gids) service_group[gids[i]] = 1
    access_mask = default_mask = "rwx"
  }
  $1 == "mask" && $2 == "" { access_mask = $3 }
  $1 == "default" && $2 == "mask" && $3 == "" { default_mask = $4 }
  $1 == "user" && $2 == service_uid && index($3, "w") { access_write = 1 }
  $1 == "group" && $2 == "" && service_group[owner_gid] && index($3, "w") { access_write = 1 }
  $1 == "group" && $2 != "" && service_group[$2] && index($3, "w") { access_write = 1 }
  $1 == "default" && $2 == "user" && $3 == service_uid && index($4, "w") { default_write = 1 }
  $1 == "default" && $2 == "group" && $3 == "" && service_group[owner_gid] && index($4, "w") { default_write = 1 }
  $1 == "default" && $2 == "group" && $3 != "" && service_group[$3] && index($4, "w") { default_write = 1 }
  $1 == "default" && $2 == "other" && $3 == "" && index($4, "w") { default_other_write = 1 }
  END {
    if ((access_write && index(access_mask, "w")) ||
        (default_write && index(default_mask, "w")) || default_other_write) exit 1
  }
'

validate_trusted_acl_output() {
  acl_gid=$1
  awk -F: -v service_uid="$service_uid" -v service_gids="$service_gids" -v owner_gid="$acl_gid" "$acl_awk_program"
}

require_trusted_acl() {
  acl_path=$1 acl_gid=$2
  acl=$(getfacl -cpn -- "$acl_path") || fail "could not inspect trusted path ACL: $acl_path"
  printf '%s\n' "$acl" | validate_trusted_acl_output "$acl_gid" ||
    fail "trusted path ACL grants service user/group write access: $acl_path"
}

require_trusted_path() {
  path=$1
  [ -n "$path" ] || fail "trusted path is empty"
  case "$path" in
    /*) ;;
    *) fail "trusted path must be absolute: $path" ;;
  esac
  [ -e "$path" ] || fail "trusted path does not exist: $path"

  real_path=$(readlink -f -- "$path")
  [ "$path" = "$real_path" ] || fail "trusted path must be canonical and contain no symlinks: $path"

  check_path=$real_path
  while :; do
    metadata=$(stat -c '%u:%g:%a' -- "$check_path")
    old_ifs=$IFS
    IFS=:
    set -- $metadata
    IFS=$old_ifs
    [ "$#" -eq 3 ] || fail "could not parse trusted path permissions: $check_path"
    mode=$((0$3))
    if is_untrusted_mode "$1" "$2" "$mode"; then
      fail "trusted path is writable by service user/group or world: $check_path"
    fi
    require_trusted_acl "$check_path" "$2"
    [ "$check_path" = / ] && break
    check_path=$(dirname -- "$check_path")
  done

  # Git worktree indirection can make Nix read metadata outside this scanned tree.
  git_file=$(find "$real_path" -name .git -type f -print -quit)
  [ -z "$git_file" ] || fail "trusted path contains unsupported gitdir indirection: $git_file"
  common_dir=$(find "$real_path" -path '*/.git/commondir' -print -quit)
  [ -z "$common_dir" ] || fail "trusted path contains unsupported git commondir indirection: $common_dir"

  # Symlink mode bits are meaningless; protected parent dirs govern replacement.
  # Keep targets inside scanned tree or immutable Nix store to prevent root following mutable external data.
  find "$real_path" -type l -exec sh -eu -c '
    root=$1
    shift
    for link do
      target=$(readlink -f -- "$link") || {
        echo "switchd-activate: trusted path contains a dangling symlink: $link" >&2
        exit 1
      }
      case "$target" in
        "$root"|"$root"/*|/nix/store/*) ;;
        *)
          echo "switchd-activate: trusted path symlink escapes source tree and Nix store: $link" >&2
          exit 1
          ;;
      esac
    done
  ' sh "$real_path" {} + || exit 1

  find "$real_path" ! -type l -exec sh -eu -c '
    service_uid=$1
    service_gids=$2
    shift 2
    for entry do
      metadata=$(stat -c "%u:%g:%a" -- "$entry")
      old_ifs=$IFS
      IFS=:
      set -- $metadata
      IFS=$old_ifs
      mode=$((0$3))
      [ "$1" != "$service_uid" ] || exit 1
      for gid in $service_gids; do
        [ "$2" != "$gid" ] || [ $((mode & 0020)) -eq 0 ] || exit 1
      done
      [ $((mode & 0002)) -eq 0 ] || exit 1
    done
  ' sh "$service_uid" "$service_gids" {} + || fail "trusted path contains service-user/group/world-writable entries: $real_path"

  find "$real_path" ! -type l -exec sh -eu -c '
    service_uid=$1
    service_gids=$2
    acl_awk_program=$3
    shift 3
    for entry do
      gid=$(stat -c %g -- "$entry")
      acl=$(getfacl -cpn -- "$entry") || exit 1
      printf "%s\n" "$acl" | awk -F: -v service_uid="$service_uid" -v service_gids="$service_gids" -v owner_gid="$gid" "$acl_awk_program" || exit 1
    done
  ' sh "$service_uid" "$service_gids" "$acl_awk_program" {} + || fail "trusted path ACL inspection failed or grants service write access: $real_path"
}
