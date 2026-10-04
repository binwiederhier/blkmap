MAKEFLAGS := --jobs=1
VERSION := $(shell git describe --tag 2>/dev/null || echo dev)

.PHONY: help build test test-root stress scenarios test-remote test-machine verify-release powercut soak test-vm examples vet fmt release release-snapshot install-deb clean

help:
	@echo "blkmap"
	@echo "  make build            - Build ./blkmap (dev version)"
	@echo "  make test             - Unit tests (no root needed)"
	@echo "  make test-root        - Also run the ublk integration tests (needs root + ublk_drv)"
	@echo "  make stress           - e2e + fio/mkfs/crash workloads against the installed deb (root, fio)"
	@echo "  make scenarios        - real-life scenarios: crashes, outages, restarts, bad configs (root, deb, fio)"
	@echo "  make test-remote HOST=ip [SUITE=stress|scenarios|all] - root suites (+workloads) on a scratch VM"
	@echo "  make test-machine HOST=ip [POWER=5] [SOAK=120] - everything against one throwaway machine, with a summary"
	@echo "  make verify-release [SOAK=120] - unattended: test-vm then a soak on every kernel template, one at a time"
	@echo "  make powercut HOST=ip [CYCLES=10] [MODE=power|kill]  - power-cut or daemon-kill cycles on a scratch VM"
	@echo "  make soak HOST=ip [MINUTES=120]  - verified I/O under chaos for hours, with leak sampling"
	@echo "  make test-vm          - everything above on a throwaway Proxmox VM (PROXMOX=root@box11 TEMPLATE=9000)"
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
	go build -o dist/rangehttpd ./scripts/rangehttpd
	sudo scripts/e2e.sh && sudo scripts/stress.sh

# Build and test every example (the gRPC one is its own module)
examples:
	go vet ./examples/... && go test ./examples/... && for e in lib-synthetic lib-dircache lib-custom-source lib-tiered lib-ldm-mirror; do go build -o /dev/null ./examples/$$e || exit 1; done
	cd examples/grpc-remote && go vet ./... && go test ./... && go build -o /dev/null .

scenarios:
	go build -o dist/rangehttpd ./scripts/rangehttpd
	sudo scripts/scenarios.sh

# Everything against one throwaway machine (unit tests here, all suites there), summary at the end
test-machine:
	scripts/test-machine.sh $(HOST) $(if $(POWER),--power $(POWER)) $(if $(SOAK),--soak $(SOAK))

# Release verification: for every kernel template a fresh VM runs everything, then soaks (hours)
verify-release:
	scripts/verify-release.sh $(or $(SOAK),120)

# Same suites on a throwaway VM (HOST=... ; SUITE=stress|scenarios|all adds the workloads)
test-remote:
	scripts/remote-test.sh $(HOST) $(SUITE)

# Power-loss (sysrq reboot) or daemon-kill cycles under write load (HOST=... ; reboots it)
powercut:
	scripts/powercut.sh $(HOST) $(or $(CYCLES),10) $(or $(MODE),power)

# Verified I/O under chaos (kills, reloads, origin outages) for MINUTES, with leak sampling
soak:
	scripts/soak.sh $(HOST) $(or $(MINUTES),120)

# The full root-level suite on a VM created for the run and destroyed after it
test-vm:
	scripts/ci-vm.sh

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
