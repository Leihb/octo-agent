# Octo — Go build & test orchestration (Go 1.22+)
#
# Usage:
#   make            same as `make test`
#   make build      build the octo binary at repo root (./octo)
#   make install    install to $GOPATH/bin
#   make test       go test -race ./...
#   make cover      generate coverage.out + coverage.html
#   make vet        go vet ./...
#   make fmt        gofmt -w on all .go files
#   make fmt-check  fail if anything would be reformatted
#   make tidy       go mod tidy
#   make clean      remove build artefacts
#
# octo-eval — lightweight eval (manual; needs a model key, NOT in CI):
#   make eval-build           build the ./octo-eval tool
#   make eval-list            list the task suite
#   make eval EVAL_FLAGS=...  run the suite (pass --provider/--model/… via EVAL_FLAGS)
# See dev-docs/octo-eval.md.

GOTAGS ?=
GOFLAGS ?=

# Build tag that enables embedding the ripgrep binary. CI builds without
# this tag so go:embed does not require binaries/rg to be present.
RG_TAGS := embedrg

# Inject version + commit at build time so `octo version` reports a real SHA.
#
# Auto-detection via `git describe` is intentionally avoided because the repo
# still carries Ruby-era tags (v0.11.2, v0.11.2-final-ruby) that would pollute
# the reported version. The single source of truth for the version number is
# internal/version/version.go (what release bumps already edit); dev builds
# derive "<that>-dev" from it, so the two never drift. Release builds set
# VERSION explicitly:
#
#   VERSION=0.12.0 make build
#
BASE_VERSION := $(shell sed -n 's/^var Version = "\(.*\)"/\1/p' internal/version/version.go)
VERSION ?= $(BASE_VERSION)-dev
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS := -X github.com/Leihb/octo-agent/internal/version.Version=$(VERSION) \
           -X github.com/Leihb/octo-agent/internal/version.Commit=$(COMMIT) \
           -X github.com/Leihb/octo-agent/internal/tools/rgembed.version=$(RG_VERSION)

GOFILES := $(shell find . -name '*.go' -not -path './vendor/*' -not -path '*/_vendor/*')

RG_EMBED_DIR := internal/tools/rgembed/binaries
RG_EMBED_BIN := $(RG_EMBED_DIR)/rg

.PHONY: all build install test cover vet fmt fmt-check tidy clean \
        eval-build eval-list eval \
        rg-embed rg-embed-clean

all: test

# Download and embed ripgrep for the current GOOS/GOARCH before building.
build: rg-embed
	go build $(GOFLAGS) -tags='$(GOTAGS) $(RG_TAGS)' -ldflags='$(LDFLAGS)' -o octo ./cmd/octo

install: rg-embed
	go install $(GOFLAGS) -tags='$(GOTAGS) $(RG_TAGS)' -ldflags='$(LDFLAGS)' ./cmd/octo

test:
	go test -race $(GOFLAGS) -tags='$(GOTAGS)' ./...

cover:
	go test $(GOFLAGS) -tags='$(GOTAGS)' -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html
	@echo "Open coverage.html in a browser to see line coverage."

vet:
	go vet ./...

fmt:
	gofmt -w $(GOFILES)

fmt-check:
	@unformatted=$$(gofmt -l $(GOFILES)); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt found unformatted files:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi

tidy:
	go mod tidy

clean:
	rm -f octo octo.exe octo-eval coverage.out coverage.html
	rm -rf dist/
	rm -f $(RG_EMBED_BIN) $(RG_EMBED_BIN).exe

# ── ripgrep embed (build-time only) ──────────────────────────────────────────
# Downloads the matching rg release for GOOS/GOARCH, extracts the binary,
# and places it where go:embed will pick it up. No-op if already present.

RG_VERSION := 15.1.0

# Default to the host platform when GOOS/GOARCH are not set explicitly.
_RG_GOOS   := $(or $(GOOS),$(shell go env GOOS))
_RG_GOARCH := $(or $(GOARCH),$(shell go env GOARCH))

rg-embed: $(RG_EMBED_BIN)

$(RG_EMBED_BIN):
	@echo "Downloading ripgrep $(RG_VERSION) for $(_RG_GOOS)/$(_RG_GOARCH)..."
	@mkdir -p $(RG_EMBED_DIR)
	@bash -c ' \
		GOOS="$(_RG_GOOS)"; GOARCH="$(_RG_GOARCH)"; RG_VERSION="$(RG_VERSION)"; \
		case "$${GOOS}_$${GOARCH}" in \
			darwin_amd64)   asset="ripgrep-$${RG_VERSION}-x86_64-apple-darwin.tar.gz" ;; \
			darwin_arm64)   asset="ripgrep-$${RG_VERSION}-aarch64-apple-darwin.tar.gz" ;; \
			linux_amd64)    asset="ripgrep-$${RG_VERSION}-x86_64-unknown-linux-musl.tar.gz" ;; \
			linux_arm64)    asset="ripgrep-$${RG_VERSION}-aarch64-unknown-linux-gnu.tar.gz" ;; \
			windows_amd64)  asset="ripgrep-$${RG_VERSION}-x86_64-pc-windows-msvc.zip" ;; \
			*) echo "Unsupported platform: $${GOOS}/$${GOARCH} — rg embed skipped"; touch '"$(RG_EMBED_BIN)"'; exit 0 ;; \
		esac; \
		url="https://github.com/BurntSushi/ripgrep/releases/download/$${RG_VERSION}/$${asset}"; \
		if [ "$${GOOS}" = "windows" ]; then \
			curl -sL "$$url" -o /tmp/rg-embed.zip; \
			unzip -q -o /tmp/rg-embed.zip -d /tmp/rg-embed; \
			cp /tmp/rg-embed/ripgrep-$${RG_VERSION}-*/rg.exe $(RG_EMBED_BIN).exe; \
			rm -rf /tmp/rg-embed.zip /tmp/rg-embed; \
		else \
			curl -sL "$$url" | tar -xzf - -C /tmp; \
			cp /tmp/ripgrep-$${RG_VERSION}-*/rg $(RG_EMBED_BIN); \
			rm -rf /tmp/ripgrep-$${RG_VERSION}-*; \
		fi; \
		echo "Embedded rg ready for $${GOOS}/$${GOARCH}" \
	'

rg-embed-clean:
	rm -f $(RG_EMBED_BIN) $(RG_EMBED_BIN).exe

# ── octo-eval — lightweight eval (manual; see dev-docs/octo-eval.md) ─────────
# Each task under evals/tasks/ is a local fixture; no Docker, no clone. Calls a
# real model, so never run in CI. Pass provider/model/etc through EVAL_FLAGS,
# e.g. make eval EVAL_FLAGS='--provider openai --model deepseek-v4-flash --allow-net'.
EVAL_FLAGS ?=

eval-build:
	go build $(GOFLAGS) -o octo-eval ./cmd/octo-eval

eval-list: eval-build
	./octo-eval list

eval: eval-build build
	./octo-eval run --octo ./octo $(EVAL_FLAGS)
