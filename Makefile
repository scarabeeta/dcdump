# Static build for any x86-64 (GOAMD64=v1), so it can be built on one machine and
# copied to the machine with the drive. `make deploy HOST=the-other-machine`.
HOST    ?=
REMOTE  ?= projects/dcdump
VERSION := $(shell git describe --always --dirty 2>/dev/null || echo dev)

bin/dcdump: *.go drives.json go.mod
	CGO_ENABLED=0 GOAMD64=v1 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $@ .

.PHONY: test deploy clean
test:
	go vet ./... && go test ./...

# Installs to $(HOST):~/$(REMOTE)/bin and links ~/.local/bin/dcdump to it, so the
# default output directory is ~/$(REMOTE)/work/dc.
deploy: bin/dcdump
	@test -n "$(HOST)" || { echo "set HOST, e.g. make deploy HOST=dumpbox"; exit 1; }
	ssh $(HOST) 'mkdir -p ~/$(REMOTE)/bin ~/$(REMOTE)/work && ln -sfn ~/$(REMOTE)/bin/dcdump ~/.local/bin/dcdump'
	scp -q bin/dcdump $(HOST):$(REMOTE)/bin/dcdump

clean:
	rm -rf bin
