.PHONY: build test bench profile lint install clean

GOEXPERIMENT ?= simd

build:
	GOEXPERIMENT=$(GOEXPERIMENT) go build -o bin/agrep ./cmd/agrep

# PCRE-enabled build: supports -P, but pays ~5ms process startup for
# modernc.org/libc's netdb init (parses /etc/services).
build-pcre:
	GOEXPERIMENT=$(GOEXPERIMENT) go build -tags pcre -o bin/agrep-pcre ./cmd/agrep

test:
	GOEXPERIMENT=$(GOEXPERIMENT) AGREP_SKIP_PCRE=1 go test -race ./...
	GOEXPERIMENT=$(GOEXPERIMENT) go test -tags pcre ./internal/matcher/ -run "PCRE"

bench:
	GOEXPERIMENT=$(GOEXPERIMENT) go test -bench=. -benchmem ./internal/matcher/ ./internal/input/ ./internal/simd/

profile:
	./scripts/profile.sh

lint:
	GOEXPERIMENT=$(GOEXPERIMENT) go vet ./...

install:
	GOEXPERIMENT=$(GOEXPERIMENT) go install ./cmd/agrep

clean:
	rm -rf bin/
