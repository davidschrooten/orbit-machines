.PHONY: all build test lint helm-lint clean

BINARY_DIR := bin
GO_FLAGS   := -trimpath
LDFLAGS    := -s -w

all: test build

build:
	@mkdir -p $(BINARY_DIR)
	go build $(GO_FLAGS) -ldflags "$(LDFLAGS)" -o $(BINARY_DIR)/orbit-dispatcher ./cmd/orbit-dispatcher
	go build $(GO_FLAGS) -ldflags "$(LDFLAGS)" -o $(BINARY_DIR)/orbit-machined ./cmd/orbit-machined

test:
	go test -race -coverprofile=coverage.txt ./...
	@go tool cover -func=coverage.txt | tail -n 1

lint:
	golangci-lint run ./...

helm-lint:
	@if [ -d charts/orbit-machines ]; then helm lint charts/orbit-machines; fi

clean:
	rm -rf $(BINARY_DIR) coverage.txt coverage.html
