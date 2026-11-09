# Releasing

## Making a release

First, update [ReleaseNotes.md](../ReleaseNotes.md): move what shipped out of "not yet released", and give it the version. Also
update `info.version` in `api/openapi.yaml`, with no suffix even for a pre-release, as the release fails early if it doesn't match the tag.

```sh
git tag -s v0.2.0 -m "nvweaver 0.2.0"
git push origin v0.2.0
```

The [release workflow](../.github/workflows/release.yml) then runs everything CI runs,
including all end-to-end suites, and publishes only if they all pass:

| What | Where |
|---|---|
| Kernel image, both architectures | `ghcr.io/xphyr/nvweaver:0.2.0`, and `:latest` |
| SPDK image, both architectures | `ghcr.io/xphyr/nvweaver:0.2.0-spdk`, and `:latest-spdk` |
| CSI driver image, both architectures, from v0.4.0 | `ghcr.io/xphyr/nvweaver-csi:0.4.0`, and `:latest` |
| One architecture's image | The same tags, ending `-amd64` or `-arm64`, as with `:0.2.0-spdk-arm64`, but not for `latest` |
| Binaries, unit, Quadlet, notices | `nvweaver-0.2.0-linux-amd64.tar.gz` and `-linux-arm64.tar.gz` on the GitHub release |
| Command line utility alone | `nvweaver-cli-0.2.0-linux-amd64` and `-linux-arm64` on the GitHub release |
| SBOMs, in SPDX JSON | On the GitHub release, one for each of those: `nvweaver-0.2.0-image-amd64.spdx.json`, `-spdk-image-`, `-linux-`, `nvweaver-cli-0.2.0-linux-amd64.spdx.json`, and `nvweaver-csi-0.4.0-image-amd64.spdx.json` |
| Checksums | `SHA256SUMS` on the GitHub release, covering all of those files |
| Provenance | Signed attestations for the multi-arch tags and all of those files |

A tag with a suffix, like `v0.1.0-rc1`, makes a pre-release and leaves `latest` alone. Use one
to try the pipeline.

**A release only reads the build cache,** `ghcr.io/xphyr/nvweaver-buildcache`, which only CI on `main` writes, and a failure to
write it there builds again without it and warns. The cache only saves time, and v0.4.0's first attempt failed when the
workflow's token was suddenly refused writing to that package, though it had Admin access.

CI cancels its own older runs of a pull request or of `main` when a newer one starts, so merging several pull requests in a
row leaves one run on `main`. A release's tests are never cancelled that way: see `concurrency` in `ci.yml`.

**Before a release, run `test/vm/k3s-csi.sh`** with the release's images, as CI doesn't: hosted runners are too small for the
k3s lab. **The first release to publish a new image makes a new package,** as `nvweaver-csi` did in v0.4.0, and GitHub makes new
packages private: make it public, under the package's settings, before people need to pull it.

The images are the ones the suites just tested, not a rebuild, and each architecture's are built on that architecture. The
publish job fails if an image doesn't report the tagged version or is the wrong architecture, if a tarball's binary is the wrong
architecture, or if the registry doesn't then serve both architectures.

## Before the first release

Set these up in the repository's settings on GitHub:

- **Environment:** create one called `release`, optionally requiring your approval. The publish
  job waits in it, which gives you a last check before anything is published.
- **Tags:** consider a tag protection rule for `v*`, since pushing a tag publishes.

`.github/make-sboms.sh` makes the SBOMs from those same images and files, with Syft. Syft can't see SPDK, DPDK or ISA-L, compiled together into `nvmf_tgt`, so `Containerfile.spdk` records what it built from, and
the script adds them. It fails if an SBOM is missing anything `THIRD_PARTY_NOTICES.md` lists, and a new row there must be added to the script, or if the images and binaries weren't all built with the same Go. Every pull request runs it for amd64.

## Verifying a release

```sh
sha256sum -c --ignore-missing SHA256SUMS
gh attestation verify nvweaver-0.2.0-linux-amd64.tar.gz --repo xphyr/nvweaver
gh attestation verify oci://ghcr.io/xphyr/nvweaver:0.2.0 --repo xphyr/nvweaver
```

Attestations are on the multi-arch tags, whose digests pin each architecture's image. Verifying a tag ending `-amd64` or
`-arm64` directly won't find one.

## Versions

Versions follow [SemVer](https://semver.org). The API is `/v1`, and until 1.0.0 anything may
still change. `nvweaverd --version` and `GET /v1/system` report the version.

## What's built

`linux/amd64` and `linux/arm64`, as in `docs/decisions.md`, entry 27. CI tests Arm on every pull request and on `main`. Its
runners have no KVM, so that job runs `e2e.sh` directly rather than the VM suites, with Podman's volumes on a throwaway XFS
for reflink. Emulated Arm VMs are a manual check, with `LAB_ARCH=aarch64` and `TEST_TIMEOUT_SCALE=10`.

## Running the same checks locally

```sh
make lint test                   # needs Podman, for shellcheck and actionlint
make images                      # all three images, with rootless Podman
./test/vm/lab.sh base            # once: builds the VM base image
./test/vm/run-all.sh             # every end-to-end suite, in fresh VMs
make package                     # the release tarball and checksums for this machine, in build/
```
