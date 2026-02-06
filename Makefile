BINARY := debux
IMAGE  := ghcr.io/ctourriere/debux:latest

.PHONY: build build-dev install image-build image-push dev clean test lint

build:
	CGO_ENABLED=0 go build -o bin/$(BINARY) ./cmd/debux

build-dev:
	go build -o bin/$(BINARY) ./cmd/debux

install: build
	cp bin/$(BINARY) /usr/local/bin/$(BINARY)

image-build:
	docker build -t $(IMAGE) -f images/debug/Dockerfile .

image-push:
	docker push $(IMAGE)

dev: build image-build

clean:
	rm -rf bin/

test:
	go test ./...

lint:
	go vet ./...
