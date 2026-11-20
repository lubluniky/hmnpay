# hmnpay: one static binary with the web interface built in.
#
#   make build     build the interface, then the binary (./hmnpay)
#   make ui        build only the interface into internal/webui/dist
#   make go        build the binary from whatever is in internal/webui/dist
#                  (without "make ui" first: a binary with the API and a status page)
#   make test      the Go gate
#   make web-test  lint, typecheck and unit tests of the interface
#   make e2e       end-to-end tests: the real binary, a local chain, a real browser
#   make clean     remove the built interface and the binary

VERSION ?= dev
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS  = -s -w -buildid= \
	-X github.com/lubluniky/hmnpay.Version=$(VERSION) \
	-X github.com/lubluniky/hmnpay.Commit=$(COMMIT) \
	-X github.com/lubluniky/hmnpay/internal/webhook.Version=$(VERSION)

.PHONY: build ui go test web-test e2e clean

build: ui go

ui:
	./scripts/build-ui.sh

go:
	CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags "$(LDFLAGS)" -o hmnpay ./cmd/hmnpay

test:
	test -z "$$(gofmt -l .)"
	go vet ./...
	go build ./...
	go test -race -count=1 -p 4 ./...

web-test:
	pnpm --filter web lint
	pnpm --filter web typecheck
	pnpm --filter web test

e2e: build
	pnpm --filter web e2e

clean:
	find internal/webui/dist -mindepth 1 ! -name .placeholder -delete
	find . -maxdepth 1 -name hmnpay -type f -delete
