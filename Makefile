.PHONY: all build build-dispatcher build-machined build-all-platforms test test-coverage lint fmt clean

GO ?= go
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

all: build

build: build-dispatcher build-machined

build-dispatcher:
	@mkdir -p bin
	$(GO) build -ldflags="-s -w" -o bin/orbit-dispatcher ./cmd/orbit-dispatcher

build-machined:
	@mkdir -p bin
	$(GO) build -ldflags="-s -w" -o bin/orbit-machined ./cmd/orbit-machined

build-all-platforms:
	@mkdir -p dist
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; \
		arch=$${platform#*/}; \
		for bin in orbit-dispatcher orbit-machined; do \
			out="dist/$${bin}-$${os}-$${arch}"; \
			echo "Building $${out}..."; \
			CGO_ENABLED=0 GOOS=$${os} GOARCH=$${arch} $(GO) build -ldflags="-s -w" -o "$${out}" "./cmd/$${bin}" || exit 1; \
		done; \
	done
	@cd dist && (command -v sha256sum >/dev/null 2>&1 && sha256sum orbit-dispatcher-* orbit-machined-* || shasum -a 256 orbit-dispatcher-* orbit-machined-*) > checksums.txt

test:
	$(GO) test -v -race ./...

test-coverage:
	$(GO) test -race -covermode=atomic -coverprofile=coverage.out ./...
	@$(GO) tool cover -func=coverage.out | tail -n 1 | awk '{ \
		cov = $$3; sub(/%/, "", cov); \
		printf "Total statement coverage: %s%% (threshold: 95.0%%)\n", cov; \
		if (cov + 0 < 95.0) { \
			printf "FAIL: coverage %s%% is below 95.0%%\n", cov; \
			exit 1; \
		} \
	}'

lint:
	$(GO) vet ./...
	gofmt -s -l .

fmt:
	gofmt -s -w .

clean:
	rm -rf bin/ dist/ coverage.out coverage.txt coverage.html
