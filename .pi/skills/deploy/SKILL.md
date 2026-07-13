---
name: deploy
description: Repo deploy/release workflow. Use whenever the user says deploy, release, publish, tag a version, bump semver, update flake hash, push a release, or asks to sync flake.nix with git tags. Ensures semver, flake.nix version/vendorHash, build checks, commit, and git tag all agree.
---

# Deploy / release workflow

Deploy means: ship one semver release where `flake.nix`, build output, commit, and git tag all point at the same version.

## Invariants

- Git tag format: `vMAJOR.MINOR.PATCH`.
- `flake.nix` package `version` must equal tag without `v`.
- The release commit must contain the version/hash change.
- The git tag must point at that exact release commit.
- Do not push a tag that does not match `flake.nix`.
- Do not deploy from a dirty tree except the intended release changes.

## Steps

1. Inspect current state:
   ```sh
   git status --short
   git tag --sort=-v:refname | head
   grep -n 'version = ' flake.nix
   grep -n 'vendorHash = ' flake.nix
   ```

2. Pick semver:
   - If user gives a version, use it exactly.
   - Else infer the smallest correct bump from the diff:
     - patch: fixes/docs/internal only
     - minor: new user-visible feature
     - major: breaking behavior/config/protocol
   - If no prior tags exist, use the existing `flake.nix` version if it is unreleased; otherwise ask.

3. Update `flake.nix`:
   - Set `version = "MAJOR.MINOR.PATCH";`.
   - If dependencies changed and `buildGoModule.vendorHash` is not `null`, run `nix build .#` and update the hash from the mismatch error.
   - If project is stdlib-only and `vendorHash = null;`, leave it null. Do not invent a hash.

4. Verify:
   ```sh
   gofmt -w $(git ls-files '*.go')
   go test ./...
   go vet ./...
   nix build .#
   ```
   Use timeouts/background tools for long commands in Pi. Report failures honestly; do not tag on red checks.

5. Commit release changes:
   ```sh
   git add flake.nix flake.lock README.md HANDOVER.md CODING_STANDARDS.md cmd .pi/skills/deploy/SKILL.md .gitignore
   git commit -m "Release vMAJOR.MINOR.PATCH"
   ```
   Adjust staged paths to actual intended changes. Never stage `.pi/tasks/`.

6. Create an annotated tag:
   ```sh
   git tag -a vMAJOR.MINOR.PATCH -m "vMAJOR.MINOR.PATCH"
   ```

7. Sync-check before push:
   ```sh
   test "$(git show -s --format=%H vMAJOR.MINOR.PATCH)" = "$(git rev-parse HEAD)"
   grep -q 'version = "MAJOR.MINOR.PATCH";' flake.nix
   git status --short
   ```

8. Push commit and tag together:
   ```sh
   git push origin HEAD
   git push origin vMAJOR.MINOR.PATCH
   ```
   Or use `git push --follow-tags` only if the annotated tag exists locally.

## Hash update notes

For `buildGoModule` with external deps:

1. Temporarily set `vendorHash = pkgs.lib.fakeHash;` or the old hash.
2. Run `nix build .#`.
3. Copy the `got: sha256-...` value from the error into `vendorHash`.
4. Re-run `nix build .#` until green.

For this repo while it stays stdlib-only, `vendorHash = null;` is correct.
