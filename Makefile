GO ?= go
BINARY := objbox

.PHONY: all build web test vet clean docker docker-run image-size

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

# 构建运行镜像（多阶段：web 构建 → go build → alpine 运行时）。
docker:
	docker build -t objbox:latest .

# 前台运行镜像，端口 18930，数据目录挂到 ./data。
docker-run:
	docker run --rm -it -p 18930:18930 -v "$(PWD)/data:/data" objbox:latest

# 打印镜像体积。
image-size:
	docker images objbox:latest --format '{{.Repository}}:{{.Tag}} {{.Size}}'

clean:
	rm -f $(BINARY)
	rm -rf web/dist
