MAKEFLAGS := --jobs=1
VERSION := $(shell git describe --tag 2>/dev/null || echo dev)

.PHONY: help build test test-root stress test-remote examples vet fmt release release-snapshot install-deb clean

help:
	@echo "blkmap"
	@echo "  make build            - Build ./blkmap (dev version)"
	@echo "  make test             - Unit tests (no root needed)"
	@echo "  make test-root        - Also run the ublk integration tests (needs root + ublk_drv)"
	@echo "  make stress           - e2e + fio/mkfs/crash workloads against the installed deb (root, fio)"
	@echo "  make test-remote HOST=ip [STRESS=stress] - root suites (+stress) on a scratch VM"
	@echo "  make examples         - vet, test and build everything under examples/"
	@echo "  make release-snapshot - Build debs/rpms/tarballs into dist/ via goreleaser (no tag needed)"
	@echo "  make release          - Tagged release via goreleaser"
	@echo "  make install-deb      - dpkg -i the amd64 snapshot deb from dist/"

build:
	go build -ldflags "-X main.version=$(VERSION)" -o blkmap .

test:
	go test -race ./...

test-root:
	go test -c -o /tmp/blkmap-ublk.test ./ublk/ && sudo /tmp/blkmap-ublk.test -test.v
	go test -c -o /tmp/blkmap-device.test ./device/ && sudo /tmp/blkmap-device.test -test.v

stress:
	sudo scripts/e2e.sh && sudo scripts/stress.sh

# Build and test every example (the gRPC one is its own module)
examples:
	go vet ./examples/... && go test ./examples/... && go build -o /dev/null ./examples/lib-synthetic && go build -o /dev/null ./examples/lib-dircache
	cd examples/grpc-remote && go vet ./... && go test ./... && go build -o /dev/null .

# Same suites on a throwaway VM (HOST=... ; add STRESS=stress for the workloads)
test-remote:
	scripts/remote-test.sh $(HOST) $(STRESS)

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
