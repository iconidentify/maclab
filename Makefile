GO ?= go
LDFLAGS = -s -w

all:
	mkdir -p bin
	CGO_ENABLED=0 $(GO) build -ldflags '$(LDFLAGS)' -o bin/labd ./cmd/labd
	CGO_ENABLED=0 $(GO) build -ldflags '$(LDFLAGS)' -o bin/lab ./cmd/lab
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -ldflags '$(LDFLAGS)' -o bin/lab-agent-linux-arm64 ./cmd/lab-agent
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -ldflags '$(LDFLAGS)' -o bin/lab-linux-arm64 ./cmd/lab
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GO) build -ldflags '$(LDFLAGS)' -o bin/oobd-darwin-arm64 ./cmd/oobd
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GO) build -ldflags '$(LDFLAGS)' -o bin/lab-builder-darwin-arm64 ./cmd/lab-builder

test:
	$(GO) vet ./...
	$(GO) test -race ./...

.PHONY: all test
