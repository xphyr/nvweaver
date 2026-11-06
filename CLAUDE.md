# nvweaver

An NVMe/TCP target with a REST API, for testing locally what an enterprise array would
do. A small Go control plane configures one of two targets, the kernel's `nvmet` or
SPDK. It is not in the data path.

Read these before changing related code:

- [docs/architecture.md](docs/architecture.md): how the pieces fit together
- [docs/decisions.md](docs/decisions.md): what was chosen, what was rejected and why
- [docs/testing.md](docs/testing.md): coverage, latest results, untested areas, pitfalls
- [docs/releasing.md](docs/releasing.md): CI, releases and what they publish
- [api/openapi.yaml](api/openapi.yaml): the API. Change it first, then the code; tests compare them
- [TODO.md](TODO.md): the roadmap, in dependency order

## Commands

```sh
make lint test                    # gofmt, shellcheck, actionlint; go vet and unit tests
make images                       # all images, with rootless Podman
./test/vm/run-all.sh              # every end-to-end suite, in fresh VMs
./test/e2e.sh                     # kernel target, end to end
BACKEND=spdk ./test/e2e.sh        # SPDK target, end to end
./test/systemd-e2e.sh             # make install, the unit and the Quadlet, under systemd
./test/vm/vm-e2e.sh               # separate initiator, emulated disks, passthrough, real reboot
./test/bench.sh                   # NVMe/TCP overhead against null_blk; also BACKEND=spdk
./test/webui-e2e.sh [DIR]         # the web interface in a real browser, with no root. DIR keeps its screenshots. Look at them
```

`test/vm/lab.sh` manages disposable VMs with their own kernels, which need no root on the machine
running them. Prefer them over loading modules on a shared machine. `SKIP_BUILD=1` reuses existing images. The SPDK image takes several minutes to build
from scratch. The Go code uses the standard library, with one exception, vendored: `golang.org/x/crypto/bcrypt`, for passwords. Don't add others without asking: see decision 40.

**Podman is the container engine. Docker is only a fallback**, with `ENGINE=docker`. Suites
default to `ENGINE="sudo podman"`, because the kernel target and the initiator need real root.

Run what a change can affect, using the table in decision 39 of `docs/decisions.md`. If a full run later finds a bug that should have been caught, add it to "Escaped a scoped run" in `docs/testing.md`.

End-to-end suites need privileged containers. They temporarily load kernel modules and
create NVMe devices on the machine running them, then clean up. Check that they did:
no leftover containers, volumes, loop devices, NVMe controllers or modules.

## Rules

- **Don't install anything on a development machine.** That includes `make install`
  without `DESTDIR`, and enabling units. To test under systemd, use a disposable VM from
  `test/vm/lab.sh`, where installing is fine, or `test/systemd-e2e.sh`.
- **Only touch containers and images you created**, by name. Build hosts may run other
  containers, so never use `prune`, or remove anything by wildcard.
- **Never point a test at a disk you weren't explicitly given.** Use loopback addresses,
  throwaway files and loop devices. Only name a physical drive that the user has said can
  be overwritten.
- **Commit only when asked, and the user does the pushing.** Committing to `main` is fine when
  the user says so; otherwise use a branch.
- **Don't bypass commit signing.** If signing fails, retry or ask.
- **Don't push secrets or machine details.** This repo goes to GitHub. Hostnames, lab
  addresses and credentials belong in `CLAUDE.local.md`, which is git-ignored.

## Project requirements

- **Production-like scale is a requirement** for the CSI driver, not just lab volumes.
- **Releases are cross-platform, x86-64 and arm64.** Don't add x86 assumptions to new code without
  saying so.
- **No physical NVMe hardware is available right now.** Build and test on emulation, and
  keep the deferred hardware tests in `TODO.md` up to date.
- **Both backends share the API and test suite.** A difference between them is either
  refused with a clear error and reported under `capabilities`, or documented in the
  README. Don't let a setting be silently ignored without saying so.

## Keeping this knowledge current

Update these in the same commit as the work they describe:

- A design choice, or a rejected alternative: `docs/decisions.md`
- A test result, a new pitfall, or a failed experiment: `docs/testing.md`
- A structural change: `docs/architecture.md`
- Finished or newly found work: `TODO.md`
- A major change that users would notice: `ReleaseNotes.md`
- A new program or kernel module that the service, or its build or tests, depends on: "Tools" in `README.md`

Report only what was actually run. A check that can't fail is worse than no check: this
project has already had false passes, listed in `docs/testing.md`.
