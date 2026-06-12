.PHONY: build build-arm64 run docker clean test version

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")

build:
	CGO_ENABLED=0 go build -ldflags="-X main.version=$(VERSION)" -o bin/opencode-router .

build-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="-X main.version=$(VERSION)" -o bin/opencode-router-linux-arm64 .

run: build
	./bin/opencode-router

docker:
	docker build -t opencode-router .

clean:
	rm -rf bin/

test:
	go test ./...

version:
	@echo $(VERSION)