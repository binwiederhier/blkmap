MAKEFLAGS := --jobs=1
VERSION := $(shell git describe --tag 2>/dev/null || echo dev)

.PHONY: help build test test-root vet fmt release release-snapshot install-deb clean

help:
	@echo "blkmap"
	@echo "  make build            - Build ./blkmap (dev version)"
	@echo "  make test             - Unit tests (no root needed)"
	@echo "  make test-root        - Also run the ublk integration tests (needs root + ublk_drv)"
	@echo "  make release-snapshot - Build debs/rpms/tarballs into dist/ via goreleaser (no tag needed)"
	@echo "  make release          - Tagged release via goreleaser"
	@echo "  make install-deb      - dpkg -i the amd64 snapshot deb from dist/"

build:
	go build -ldflags "-X main.version=$(VERSION)" -o blkmap .

test:
	go test -race ./...

test-root:
	go test -c -o /tmp/blkmap-device.test ./device/ && sudo /tmp/blkmap-device.test -test.v

vet:
	gofmt -l . && go vet ./...

release:
	goreleaser release --clean

release-snapshot:
	goreleaser release --snapshot --clean

install-deb:
	sudo dpkg -i dist/blkmap_*_linux_amd64.deb

clean:
	rm -rf dist blkmap
