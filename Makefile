GO ?= go
BINARY := objbox

.PHONY: all build web test vet clean

all: build

# build 先构建前端再 go build：//go:embed all:dist 依赖 web/dist 构建产物。
build: web
	$(GO) build -o $(BINARY) ./cmd/objbox

# 依赖缺失时先 npm ci；随后 npm run build 产出 web/dist。
web:
	@if [ ! -d web/node_modules ]; then cd web && npm ci; fi
	cd web && npm run build

test:
	$(GO) test ./... -count=1

vet:
	$(GO) vet ./...

clean:
	rm -f $(BINARY)
	rm -rf web/dist
