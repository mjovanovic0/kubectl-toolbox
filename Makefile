BINARY  := kubectl-vmsnap
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/mjovanovic0/kubectl-vmsnap/cmd.Version=$(VERSION)
PREFIX  ?= $(HOME)/bin

.PHONY: build test vet fmt install clean cross text text-all

build:
	go build -ldflags '$(LDFLAGS)' -o $(BINARY) .

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

# kubectl discovers the plugin by finding an executable named kubectl-vmsnap on PATH.
install: build
	install -d $(PREFIX)
	install -m 0755 $(BINARY) $(PREFIX)/$(BINARY)
	@echo "installed $(PREFIX)/$(BINARY) -- verify with: kubectl plugin list"

cross:
	@for target in darwin/amd64 darwin/arm64 linux/amd64 linux/arm64; do \
		os=$${target%/*}; arch=$${target#*/}; \
		echo "building dist/$(BINARY)-$$os-$$arch"; \
		GOOS=$$os GOARCH=$$arch go build -ldflags '$(LDFLAGS)' \
			-o dist/$(BINARY)-$$os-$$arch . || exit 1; \
	done

# Package as pasteable text for hosts you can only reach by copying text into a
# terminal. Produces a self-extracting, checksum-verified shell script.
#   make text                          -> dist/kubectl-vmsnap-linux-amd64.sh
#   make text TEXT_ARCH=arm64
#   make text COMP=xz                  -> ~33% less text, needs unxz on the host
#   make text CHUNK=4                  -> also split the payload into 4MiB parts
text:
	@VERSION='$(VERSION)' ./hack/textpack.sh

# Both architectures we are likely to meet on a cluster host.
text-all:
	@VERSION='$(VERSION)' TEXT_ARCH=amd64 ./hack/textpack.sh
	@VERSION='$(VERSION)' TEXT_ARCH=arm64 ./hack/textpack.sh

clean:
	rm -rf $(BINARY) dist
