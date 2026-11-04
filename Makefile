# "make install" puts files in place and enables nothing. Set DESTDIR to stage
# them somewhere else.
PREFIX ?= /usr/local
SYSCONFDIR ?= /etc
BINDIR = $(PREFIX)/bin
DOCDIR = $(PREFIX)/share/doc/nvweaver
UNITDIR = $(SYSCONFDIR)/systemd/system
QUADLETDIR = $(SYSCONFDIR)/containers/systemd
ENVFILE = $(SYSCONFDIR)/nvweaver/nvweaver.env

.PHONY: all build test install install-quadlet uninstall clean
all: build

build: nvweaverd

nvweaverd: go.mod $(shell find cmd internal -name '*.go')
	CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o $@ ./cmd/nvweaverd

nvweaver.service: dist/systemd/nvweaver.service.in
	sed -e 's|@BINDIR@|$(BINDIR)|g' -e 's|@DOCDIR@|$(DOCDIR)|g' $< > $@

test:
	go vet ./...
	go test ./...

# The environment file is only created if missing: it may hold a real token.
define install-envfile
	install -d -m 0755 $(DESTDIR)$(dir $(ENVFILE))
	[ -e $(DESTDIR)$(ENVFILE) ] || install -m 0600 dist/systemd/nvweaver.env $(DESTDIR)$(ENVFILE)
endef

install: nvweaverd nvweaver.service
	install -D -m 0755 nvweaverd $(DESTDIR)$(BINDIR)/nvweaverd
	install -D -m 0644 nvweaver.service $(DESTDIR)$(UNITDIR)/nvweaver.service
	install -D -m 0644 README.md $(DESTDIR)$(DOCDIR)/README.md
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
		$(DESTDIR)$(QUADLETDIR)/nvweaver-spdk.container $(DESTDIR)$(DOCDIR)/README.md

clean:
	rm -f nvweaverd nvweaver.service
