# Octo — Go build & test orchestration
#
# Usage:
#   make            same as `make test`
#   make build      build the octo binary at repo root (./octo)
#   make install    install to $GOPATH/bin
#   make test       go test -race ./...
#   make cover      generate coverage.out + coverage.html
#   make vet        go vet ./...
#   make fmt        gofmt -w on the tree
#   make fmt-check  fail if anything would be reformatted
#   make tidy       go mod tidy
#   make clean      remove build artefacts

GOTAGS ?=
GOFLAGS ?=

# Inject version + commit at build time so `octo version` reports a real SHA.
#
# Auto-detection via `git describe` is intentionally avoided here because the
# repo still carries Ruby-era tags (v0.11.2, v0.11.2-final-ruby) that would
# pollute the reported version. Dev builds say "<next>-dev"; release builds
# should set VERSION explicitly:
#
#   VERSION=0.2.0 make build
#
VERSION ?= 0.2.0-dev
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS := -X github.com/Leihb/octo-agent/internal/version.Version=$(VERSION) \
           -X github.com/Leihb/octo-agent/internal/version.Commit=$(COMMIT)

.PHONY: all build install test cover vet fmt fmt-check tidy clean

all: test

build:
	go build $(GOFLAGS) -tags='$(GOTAGS)' -ldflags='$(LDFLAGS)' -o octo ./cmd/octo

install:
	go install $(GOFLAGS) -tags='$(GOTAGS)' -ldflags='$(LDFLAGS)' ./cmd/octo

test:
	go test -race $(GOFLAGS) -tags='$(GOTAGS)' ./...

cover:
	go test $(GOFLAGS) -tags='$(GOTAGS)' -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html
	@echo "Open coverage.html in a browser to see line coverage."

vet:
	go vet ./...

fmt:
	gofmt -w .

fmt-check:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt found unformatted files:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi

tidy:
	go mod tidy

clean:
	rm -f octo octo.exe coverage.out coverage.html
	rm -rf dist/
