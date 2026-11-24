NAME = discord-userinfo-proxy
IMAGE = ghcr.io/xonvanetta/${NAME}
TAG ?= dev

.PHONY: help test lint vuln build image run tag

help:
	@echo "test   run unit tests with the race detector"
	@echo "lint   gofmt check and go vet"
	@echo "vuln   govulncheck for reachable vulnerabilities"
	@echo "build  build the binary"
	@echo "image  build the container image (TAG=v0.1.0)"
	@echo "run    run the binary (needs DISCORD_* env, see README)"
	@echo "tag    create and push an annotated tag (TAG=v0.1.0)"

test:
	go test -race ./...

lint:
	@test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)
	go vet ./...

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${TAG}" -o ${NAME} ./cmd/${NAME}

image:
	docker build . -t ${IMAGE}:${TAG} -f build/Containerfile --build-arg VERSION=${TAG}

run: build
	./${NAME}

tag: test
	git tag -a ${TAG} -m ${TAG} && git push --follow-tags
