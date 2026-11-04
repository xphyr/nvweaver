# "make install" puts files in place and enables nothing. Set DESTDIR to stage
# them somewhere else.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
PREFIX ?= /usr/local
SYSCONFDIR ?= /etc
BINDIR = $(PREFIX)/bin
DOCDIR = $(PREFIX)/share/doc/nvweaver
UNITDIR = $(SYSCONFDIR)/systemd/system
QUADLETDIR = $(SYSCONFDIR)/containers/systemd
ENVFILE = $(SYSCONFDIR)/nvweaver/nvweaver.env

.PHONY: all build test lint images package install install-quadlet uninstall clean
all: build

build: nvweaverd

nvweaverd: go.mod $(shell find cmd internal -name '*.go')
	CGO_ENABLED=0 go build -trimpath -ldflags='-s -w -X main.version=$(VERSION)' -o $@ ./cmd/nvweaverd

nvweaver.service: dist/systemd/nvweaver.service.in
	sed -e 's|@BINDIR@|$(BINDIR)|g' -e 's|@DOCDIR@|$(DOCDIR)|g' $< > $@

test:
	go vet ./...
	go test ./...

SHELLCHECK_IMAGE = docker.io/koalaman/shellcheck:v0.10.0
ACTIONLINT_IMAGE = docker.io/rhysd/actionlint:1.7.7
REDOCLY_IMAGE = docker.io/redocly/cli:2.8.0

# Formatting, then shell scripts and workflows, using pinned tools in rootless Podman.
lint:
	@files=$$(gofmt -l .); if [ -n "$$files" ]; then echo "Not gofmt-formatted: $$files"; exit 1; fi
	podman run --rm --pull=missing -v "$$PWD:/mnt:ro,Z" --workdir /mnt $(SHELLCHECK_IMAGE) -S warning \
		test/e2e.sh test/bench.sh test/systemd-e2e.sh test/vm/lab.sh test/vm/vm-e2e.sh test/vm/run-all.sh test/vm/scale-bench.sh
	podman run --rm --pull=missing -v "$$PWD:/mnt:ro,Z" --workdir /mnt $(ACTIONLINT_IMAGE)
	podman run --rm --pull=missing -v "$$PWD:/spec:ro,Z" --workdir /spec $(REDOCLY_IMAGE) \
		lint --skip-rule operation-4xx-response api/openapi.yaml

# BUILD_FLAGS can add layer caching, as CI does.
images:
	podman build $(BUILD_FLAGS) -t nvweaver:dev --build-arg VERSION=$(VERSION) -f Containerfile .
	podman build $(BUILD_FLAGS) -t nvweaver:spdk --build-arg VERSION=$(VERSION) -f Containerfile.spdk .
	podman build $(BUILD_FLAGS) -t nvweaver:systemd-test -f test/systemd/Containerfile .

# The release tarball and its checksum, in build/.
PACKAGE = nvweaver-$(VERSION)-linux-amd64
package: nvweaverd nvweaver.service
	rm -rf build
	install -d build/$(PACKAGE)
	install -m 0755 nvweaverd build/$(PACKAGE)/
	install -m 0644 nvweaver.service dist/systemd/nvweaver.env README.md LICENSE THIRD_PARTY_NOTICES.md \
		build/$(PACKAGE)/
	# A release has a published image to use, unlike a build from source.
	sed -e 's|^# Build the image first:.*|# Uses the published image for this release.|' \
		-e 's|^Image=.*|Image=ghcr.io/xphyr/nvweaver:$(VERSION)-spdk|' \
		-e '/^# Built locally, so there is no registry/d' -e '/^Pull=never/d' \
		dist/systemd/nvweaver-spdk.container > build/$(PACKAGE)/nvweaver-spdk.container
	tar --sort=name --owner=0 --group=0 --numeric-owner -C build -czf build/$(PACKAGE).tar.gz $(PACKAGE)
	rm -rf build/$(PACKAGE)
	cd build && sha256sum $(PACKAGE).tar.gz > SHA256SUMS

# The environment file is only created if missing: it may hold a real token.
define install-envfile
	install -d -m 0755 $(DESTDIR)$(dir $(ENVFILE))
	[ -e $(DESTDIR)$(ENVFILE) ] || install -m 0600 dist/systemd/nvweaver.env $(DESTDIR)$(ENVFILE)
endef

install: nvweaverd nvweaver.service
	install -D -m 0755 nvweaverd $(DESTDIR)$(BINDIR)/nvweaverd
	install -D -m 0644 nvweaver.service $(DESTDIR)$(UNITDIR)/nvweaver.service
	install -D -m 0644 README.md $(DESTDIR)$(DOCDIR)/README.md
	install -D -m 0644 LICENSE $(DESTDIR)$(DOCDIR)/LICENSE
	install -D -m 0644 THIRD_PARTY_NOTICES.md $(DESTDIR)$(DOCDIR)/THIRD_PARTY_NOTICES.md
	$(install-envfile)
	@echo "Installed. To start now and at every boot:"
	@echo "  systemctl daemon-reload && systemctl enable --now nvweaver"

install-quadlet:
	install -D -m 0644 dist/systemd/nvweaver-spdk.container $(DESTDIR)$(QUADLETDIR)/nvweaver-spdk.container
	$(install-envfile)
	@echo "Installed. Build the image, then start it (Quadlet units are always enabled):"
	@echo "  podman build -t nvweaver:spdk -f Containerfile.spdk ."
	@echo "  systemctl daemon-reload && systemctl start nvweaver-spdk"

# Leaves the environment file and /var/lib/nvweaver, which hold configuration and
# possibly data. Stop and disable the service first.
uninstall:
	rm -f $(DESTDIR)$(BINDIR)/nvweaverd $(DESTDIR)$(UNITDIR)/nvweaver.service \
		$(DESTDIR)$(QUADLETDIR)/nvweaver-spdk.container $(DESTDIR)$(DOCDIR)/README.md \
		$(DESTDIR)$(DOCDIR)/LICENSE $(DESTDIR)$(DOCDIR)/THIRD_PARTY_NOTICES.md

clean:
	rm -rf nvweaverd nvweaver.service build
