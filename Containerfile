FROM docker.io/library/golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /nvweaverd ./cmd/nvweaverd

FROM docker.io/library/alpine:3.22
# kmod: nvmet-tcp is loaded from the host's /lib/modules. Its modprobe handles
# xz, zstd and gzip modules, whichever the host distribution uses.
RUN apk add --no-cache kmod
COPY --from=build /nvweaverd /usr/local/bin/nvweaverd
COPY LICENSE THIRD_PARTY_NOTICES.md /usr/share/licenses/nvweaver/
COPY --from=build /usr/local/go/LICENSE /usr/local/go/PATENTS /usr/share/licenses/nvweaver/go/
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/nvweaverd"]
