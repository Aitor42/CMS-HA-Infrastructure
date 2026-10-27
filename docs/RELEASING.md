# Releasing `cms-ha`

Releases are built from annotated tags by `.github/workflows/release.yml`. The
workflow uses GoReleaser to produce `linux/amd64` and `linux/arm64` archives and
a single `SHA256SUMS` file. Syft attaches an SBOM for each archive, and Cosign
signs the checksum file with a short-lived keyless certificate (OIDC from
GitHub Actions). No long-lived signing key is stored in the repository.

## Cut a release

1. Confirm the PXE/Cobbler gate is green for the commit on `main` (the hosted
   cloud run is a fast iteration path, not the deliverable gate).
2. Create and push an annotated tag:

   ```bash
   git tag -a v2.2.0 -m "cms-ha v2.2.0"
   git push origin v2.2.0
   ```

3. The **Release** workflow publishes the GitHub release and uploads
   `release-evidence` as a workflow artifact. The binary reports its identity:

   ```bash
   cms-ha --version
   # cms-ha version v2.2.0 (commit <sha>, built <timestamp>)
   ```

## Verify a download

```bash
sha256sum -c SHA256SUMS

cosign verify-blob \
  --bundle SHA256SUMS.sigstore.json \
  --certificate-identity-regexp 'https://github.com/Aitor42/CMS-HA-Infrastructure/.github/workflows/release.yml@refs/tags/v.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  SHA256SUMS

# Inspect the attached SBOM (one per archive).
syft packages cms-ha_2.2.0_linux_amd64.tar.gz -o table
```

The checksum file is the root of trust for the archives; verify it first, then
verify each archive against it. A release without `SHA256SUMS.sigstore.json` or
an SBOM is incomplete and should not be deployed.

## Local dry run

GoReleaser can build the same archives without publishing or signing:

```bash
goreleaser check
goreleaser release --snapshot --clean --skip=publish,sign,sbom
```

The output lands in `dist/` (ignored by git). `make build` also injects
`version`, `commit` and `date`, so `--version` works outside a release.
