# Third-party notices

nvweaver itself is under the [MIT license](LICENSE). What it is distributed with has its
own licenses, and those notices are included where the code is:

| Component | Where | License | Notice in the image |
|---|---|---|---|
| Go runtime and standard library | `nvweaverd`, in every image and native build | BSD-3-Clause, with a patent grant | `go/` |
| [SPDK](https://spdk.io) | `nvmf_tgt`, in the SPDK image | BSD-3-Clause | `spdk/` |
| [DPDK](https://www.dpdk.org) | `nvmf_tgt`, in the SPDK image | BSD-3-Clause; some files have other permissive licenses, as described in its `README` | `dpdk/` |
| [ISA-L](https://github.com/intel/isa-l) | `nvmf_tgt`, in the SPDK image | BSD-3-Clause | `isa-l/` |

Those paths are under `/usr/share/licenses/nvweaver/` in each image.

SPDK's other optional parts are not built into the image, including the ones that bring in
further licenses. `Containerfile.spdk` lists what is switched off.

The images also contain packages from their base distributions (Alpine or Debian), such as
`kmod` and OpenSSL, each under its own license. See those distributions for their terms and
source. A native build needs nothing beyond the Go toolchain at build time.
