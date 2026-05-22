# Releasing aprgo

## Cut a release

```bash
git tag v0.1.0
git push origin v0.1.0
```

That's it. The `release` workflow (`.github/workflows/release.yml`) takes over:

1. Builds the binary for **amd64 / arm64 / armv7 / armv6 / i386**.
2. Packages 5 `.deb` + 4 `.rpm` files via nfpm (no ARMv6 RPM).
3. Attests build provenance.
4. Publishes a GitHub Release at `github.com/cartpauj/aprgo/releases/tag/v0.1.0` with all 9 packages attached.

Wall-clock: ~2 minutes.

## Tag conventions

- Must start with `v`. The workflow only fires on `v*` tags.
- Use SemVer: `vMAJOR.MINOR.PATCH` (e.g. `v1.2.3`).
- Pre-releases: `v1.2.3-rc1`, `v1.2.3-beta`, etc. — these still trigger the workflow, so use them to test changes to the release pipeline itself without committing to a "real" version.

## Re-doing a tag

Tags are immutable on the remote. If you pushed a bad `v0.1.0` and need to redo:

```bash
# 1. Delete the tag locally and on the remote.
git tag -d v0.1.0
git push origin :refs/tags/v0.1.0

# 2. Delete the GitHub Release page in the web UI
#    (Releases → click the release → "Delete this release"). The workflow
#    fails to overwrite an existing release page, so this step is required.

# 3. Re-tag and push.
git tag v0.1.0
git push origin v0.1.0
```

## What gets stamped into the binary

The git tag is the single source of truth for the version. The workflow extracts it from `$GITHUB_REF_NAME` and:

- Bakes it into the binary via `-ldflags="-X main.Version=v0.1.0"` (visible in `aprgo --version` and the web UI footer).
- Strips the leading `v` (→ `0.1.0`) and passes it to nfpm as `$VERSION`, which uses it for `.deb` / `.rpm` metadata + filenames.

You do **not** edit `cmd/aprgo/main.go` to bump the version. Dev builds without `-ldflags` show `"dev"` as the version, which is correct.

## Verifying a release locally before tagging

Sanity-check the package build without actually tagging:

```bash
# Build the binary like CI does.
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-s -w -X main.Version=v0.1.0-test" \
    -trimpath -o /tmp/aprgo ./cmd/aprgo

# Install nfpm if you don't have it:
#   curl -sfL https://install.goreleaser.com/github.com/goreleaser/nfpm.sh | sh

# Produce a .deb.
VERSION=0.1.0-test ARCH=amd64 GO_BINARY=/tmp/aprgo \
    nfpm pkg --packager deb --target /tmp/ --config deploy/nfpm.yaml

# Install + run.
sudo apt install /tmp/aprgo_0.1.0-test_amd64.deb
sudo systemctl start aprgo
```

## Reverting / yanking a release

`.deb` and `.rpm` aren't published to a long-lived repo — they live attached to the GitHub Release page. To make a release effectively disappear:

1. Delete the GitHub Release page (Releases → click the release → "Delete this release"). The tag remains in git history.
2. (Optional) Also delete the underlying tag if you want it gone:
   ```bash
   git tag -d v0.1.0
   git push origin :refs/tags/v0.1.0
   ```

Existing operators who installed the deleted release continue running fine — the binary is on their disk; the package manager doesn't phone home. They just won't get the bad version from the one-line installer (`get.sh`) anymore because it always pulls the latest extant release.

## Checklist before tagging

- [ ] `go test ./...` is green.
- [ ] Manually smoke-tested on the Wyse iGate (or other real hardware).
- [ ] `TODO.md` updated if the release closes any open items.
- [ ] README + RELEASE notes don't reference unmerged features.
