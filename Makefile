GO ?= go
VERSION ?= dev
ARCHES ?= amd64 arm64

.PHONY: build test vet release dist smoke
build:
	$(GO) build -trimpath -o bin/simd ./cmd/simd
test:
	$(GO) test -race ./...
vet:
	$(GO) vet ./...
release:
	python3 scripts/package_release.py --go "$(GO)" --version "$(VERSION)" --arch $(ARCHES) --binaries-only
dist:
	python3 scripts/package_release.py --go "$(GO)" --version "$(VERSION)" --arch $(ARCHES)
smoke: build
	python3 scripts/smoke_test.py --binary bin/simd
