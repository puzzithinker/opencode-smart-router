.PHONY: build build-arm64 run docker clean test

build:
	CGO_ENABLED=0 go build -o bin/opencode-router .

build-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o bin/opencode-router-linux-arm64 .

run: build
	./bin/opencode-router

docker:
	docker build -t opencode-router .

clean:
	rm -rf bin/

test:
	go test ./...
