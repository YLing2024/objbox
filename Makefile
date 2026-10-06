GO ?= go
BINARY := objbox

.PHONY: all build test vet clean

all: build

build:
	$(GO) build -o $(BINARY) ./cmd/objbox

test:
	$(GO) test ./... -count=1

vet:
	$(GO) vet ./...

clean:
	rm -f $(BINARY)
