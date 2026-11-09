# Third-party notices

nvweaver itself is under the [MIT license](LICENSE). What it is distributed with has its
own licenses, and those notices are included where the code is:

| Component | Where | License | Notice in the image |
|---|---|---|---|
| Go runtime and standard library | `nvweaverd`, in every image and native build, and `nvweaver-csi` | BSD-3-Clause, with a patent grant | `go/` |
| [golang.org/x/crypto](https://pkg.go.dev/golang.org/x/crypto) (`bcrypt`, `blowfish`) | `nvweaverd` and `nvweaver`, in the service's images and native build. Its source is in `vendor/` | BSD-3-Clause, with a patent grant | `vendor/golang.org/x/crypto/LICENSE`, in this repository |
| [SPDK](https://spdk.io) | `nvmf_tgt`, in the SPDK image | BSD-3-Clause | `spdk/` |
| [DPDK](https://www.dpdk.org) | `nvmf_tgt`, in the SPDK image | BSD-3-Clause; some files have other permissive licenses, as described in its `README` | `dpdk/` |
| [ISA-L](https://github.com/intel/isa-l) | `nvmf_tgt`, in the SPDK image | BSD-3-Clause | `isa-l/` |
| [google.golang.org/grpc](https://pkg.go.dev/google.golang.org/grpc) | `nvweaver-csi`, in the CSI driver image | Apache-2.0, with a `NOTICE` | `vendor/google.golang.org/grpc/` |
| [google.golang.org/protobuf](https://pkg.go.dev/google.golang.org/protobuf) | `nvweaver-csi`, in the CSI driver image | BSD-3-Clause, with a patent grant | `vendor/google.golang.org/protobuf/` |
| [github.com/container-storage-interface/spec](https://github.com/container-storage-interface/spec) | `nvweaver-csi`, in the CSI driver image | Apache-2.0 | `vendor/github.com/container-storage-interface/spec/` |
| [google.golang.org/genproto/googleapis/rpc](https://pkg.go.dev/google.golang.org/genproto/googleapis/rpc) | `nvweaver-csi`, in the CSI driver image | Apache-2.0 | `vendor/google.golang.org/genproto/googleapis/rpc/` |
| [golang.org/x/net](https://pkg.go.dev/golang.org/x/net) | `nvweaver-csi`, in the CSI driver image | BSD-3-Clause, with a patent grant | `vendor/golang.org/x/net/` |
| [golang.org/x/sys](https://pkg.go.dev/golang.org/x/sys) | `nvweaver-csi`, in the CSI driver image | BSD-3-Clause, with a patent grant | `vendor/golang.org/x/sys/` |
| [golang.org/x/text](https://pkg.go.dev/golang.org/x/text) | `nvweaver-csi`, in the CSI driver image | BSD-3-Clause, with a patent grant | `vendor/golang.org/x/text/` |

Those paths are under `/usr/share/licenses/nvweaver/` in each of the service's images, and under
`/usr/share/licenses/nvweaver-csi/` in the CSI driver's. The driver's modules are vendored, with their notices, in `csi/vendor/`.

SPDK's other optional parts are not built into the image, including the ones that bring in
further licenses. `Containerfile.spdk` lists what is switched off.

The images also contain packages from their base distributions (Alpine or Debian), such as
`kmod` and OpenSSL, each under its own license. See those distributions for their terms and
source. A native build needs nothing beyond the Go toolchain at build time.
