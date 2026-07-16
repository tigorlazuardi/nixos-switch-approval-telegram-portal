require_trusted_flake_ref() {
  ref=$1
  case "$ref" in
    *'
'*) fail "trusted flake ref must be one line" ;;
  esac

  path_part=${ref%%#*}
  ref_suffix=${ref#"$path_part"}
  trusted_local_path=
  trusted_local_kind=

  case "$path_part" in
    /*)
      case "$path_part" in *'?'*) fail "local flake refs must not contain query parameters" ;; esac
      trusted_local_path=$path_part
      trusted_local_kind=absolute
      ;;
    ./*|../*) fail "relative path flake refs are not allowed for activation trust" ;;
    path:/*)
      case "$path_part" in *'?'*) fail "local flake refs must not contain query parameters" ;; esac
      trusted_local_path=${path_part#path:}
      trusted_local_kind=path
      ;;
    path:*) fail "path flake refs must use an absolute canonical path" ;;
    file:///*)
      case "$path_part" in *'?'*) fail "local flake refs must not contain query parameters" ;; esac
      trusted_local_path=/${path_part#file:///}
      trusted_local_kind=file
      ;;
    file:*) fail "file flake refs must use file:/// plus an absolute canonical path" ;;
    git+file:///*)
      case "$path_part" in *'?'*) fail "local flake refs must not contain query parameters" ;; esac
      trusted_local_path=/${path_part#git+file:///}
      trusted_local_kind=git-file
      ;;
    git+file:*) fail "git+file flake refs must use git+file:/// plus an absolute canonical path" ;;
  esac

  if [ -n "$trusted_local_path" ]; then
    require_trusted_path "$trusted_local_path"
  fi
  trusted_flake_ref=$ref
}

snapshot_trusted_flake_ref() {
  snapshot_parent=$1
  [ -n "$trusted_local_path" ] || return 0

  source_snapshot=$snapshot_parent/source
  mkdir -m 0700 -- "$source_snapshot"
  if find "$trusted_local_path" ! -path '*/.git' ! -path '*/.git/*' ! -type d ! -type f ! -type l -print -quit | grep -q .; then
    fail "trusted local flake contains unsupported special files"
  fi
  # ponytail: all Git metadata is data outside approved flake semantics; omit it instead of validating Git's extensible config surface.
  tar -C "$trusted_local_path" --exclude=.git --exclude='*/.git' --sort=name --format=posix -cf - . |
    tar -C "$source_snapshot" --no-same-owner --no-same-permissions -xf -
  require_trusted_path "$source_snapshot"

  # Force path semantics for every local spelling so neither Nix nor Git discovers copied metadata.
  trusted_flake_ref=path:$source_snapshot$ref_suffix
}
