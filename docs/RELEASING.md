# Releasing `cms-ha`

Releases are triggered by `v*` tags in `.github/workflows/release.yml` (use
annotated tags by convention; the trigger itself accepts either tag kind).
The workflow runs reusable CI, then the full hosted ISO/PXE integration with
strict chaos and teardown, against the exact tagged commit before publishing.
Failure or cancellation prevents publication. It then uses GoReleaser to
produce `linux/amd64` and `linux/arm64` archives and
a single `SHA256SUMS` file. Syft attaches an SBOM for each archive, and Cosign
signs the checksum file with a short-lived keyless certificate (OIDC from
GitHub Actions). No long-lived signing key is stored in the repository.

> **Status on `ci/hosted-runner-lab` (2026-11-05).** The workflow exists in the
> tree but has not run yet: the existing tags (`v1.0.0`, `v1.1.0`, `v2.1`)
> predate it and there are no release runs. The same applies to the PXE/Cobbler
> path: the only self-hosted integration run is cancelled. Validate both with a
> pre-release tag before the first stable release; a merged workflow is not
> evidence that its gate passes.

## Cut a release

1. Review the target commit on `main` and record the applicable measurements
   using the [acceptance procedure](MANUAL.md#73-automated-failover-testing).
   A green cloud smoke run is not the ISO/PXE deliverable gate. The tag workflow reruns CI and PXE
   before publication; this can occupy a hosted runner for several hours.
2. Create and push an annotated tag:

   ```bash
   git tag -a v2.2.0 -m "cms-ha v2.2.0"
   git push origin v2.2.0
   ```

3. After both gates pass, the **Release** workflow publishes the GitHub release
   and uploads `release-evidence`. The integration artifacts include
   `failover.json` and expire after 30 days; attach a sanitized report and
   supporting evidence to a durable release location. There is no reliance on
   a subsequent `release.published` event. The binary reports its identity:

   ```bash
   cms-ha --version
   # cms-ha version v2.2.0 (commit <sha>, built <timestamp>)
   ```

## Verify a download

```bash
cosign verify-blob \
  --bundle SHA256SUMS.sigstore.json \
  --certificate-identity-regexp 'https://github.com/Aitor42/CMS-HA-Infrastructure/.github/workflows/release.yml@refs/tags/v.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  SHA256SUMS

# Only trust the checksums after the signature verification succeeds.
sha256sum -c SHA256SUMS

# Inspect the attached SBOM (one per archive).
syft packages cms-ha_2.2.0_linux_amd64.tar.gz -o table
```

Verify the checksum file's signature and the expected signer identity before
trusting its archive checksums. The signature authenticates the checksum file;
the checksum file alone is not a trust anchor. A release without `SHA256SUMS.sigstore.json` or
an SBOM is incomplete and should not be deployed.

## Local dry run

GoReleaser can build the same archives without publishing or signing:

```bash
goreleaser check
goreleaser release --snapshot --clean --skip=publish,sign,sbom
```

The output lands in `dist/` (ignored by git). `make build` also injects
`version`, `commit` and `date`, so `--version` works outside a release.
