FROM docker.io/library/golang:1.26.10-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY vendor ./vendor
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /nvweaverd ./cmd/nvweaverd \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /nvweaver ./cmd/nvweaver

FROM docker.io/library/alpine:3.22
# kmod: nvmet-tcp is loaded from the host's /lib/modules. Its modprobe handles
# xz, zstd and gzip modules, whichever the host distribution uses.
RUN apk add --no-cache kmod
COPY --from=build /nvweaverd /nvweaver /usr/local/bin/
COPY LICENSE THIRD_PARTY_NOTICES.md /usr/share/licenses/nvweaver/
COPY --from=build /usr/local/go/LICENSE /usr/local/go/PATENTS /usr/share/licenses/nvweaver/go/
ARG VERSION=dev
LABEL org.opencontainers.image.title="nvweaver" \
      org.opencontainers.image.description="NVMe over TCP target with a REST API" \
      org.opencontainers.image.source="https://github.com/xphyr/nvweaver" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${VERSION}"
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/nvweaverd"]
