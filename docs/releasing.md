# Releasing

## Making a release

```sh
git tag -s v0.1.0 -m "nvweaver 0.1.0"
git push origin v0.1.0
```

The [release workflow](../.github/workflows/release.yml) then runs everything CI runs,
including all end-to-end suites, and publishes only if they all pass:

| What | Where |
|---|---|
| Kernel image | `ghcr.io/xphyr/nvweaver:0.1.0`, and `:latest` |
| SPDK image | `ghcr.io/xphyr/nvweaver:0.1.0-spdk`, and `:latest-spdk` |
| Binary, unit, Quadlet, notices | `nvweaver-0.1.0-linux-amd64.tar.gz` on the GitHub release |
| Checksums | `SHA256SUMS` on the GitHub release |
| Provenance | Signed attestations for the images and the tarball |

A tag with a suffix, like `v0.1.0-rc1`, makes a pre-release and leaves `latest` alone. Use one
to try the pipeline.

The images are the ones the suites just tested, not a rebuild. The publish job fails if an image
doesn't report the tagged version.

## Before the first release

Set these up in the repository's settings on GitHub:

- **Environment:** create one called `release`, optionally requiring your approval. The publish
  job waits in it, which gives you a last check before anything is published.
- **Tags:** consider a tag protection rule for `v*`, since pushing a tag publishes.

## Verifying a release

```sh
sha256sum -c SHA256SUMS
gh attestation verify nvweaver-0.1.0-linux-amd64.tar.gz --repo xphyr/nvweaver
gh attestation verify oci://ghcr.io/xphyr/nvweaver:0.1.0 --repo xphyr/nvweaver
```

## Versions

Versions follow [SemVer](https://semver.org). The API is `/v1`, and until 1.0.0 anything may
still change. `nvweaverd --version` and `GET /v1/system` report the version.

## What's built

Only `linux/amd64` so far. arm64 is planned for v0.2.0, as in `docs/decisions.md`, entry 27. CI already tests Arm on every
pull request and on `main`, but publishes nothing from it yet. Its runners have no KVM, so that job runs `e2e.sh` directly
rather than the VM suites. Emulated Arm VMs are a manual check, with `LAB_ARCH=aarch64` and `TEST_TIMEOUT_SCALE=10`.

## Running the same checks locally

```sh
make lint test                   # needs Podman, for shellcheck and actionlint
make images                      # all three images, with rootless Podman
./test/vm/lab.sh base            # once: builds the VM base image
./test/vm/run-all.sh             # every end-to-end suite, in fresh VMs
make package                     # the release tarball and checksums, in build/
```
