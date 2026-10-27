.PHONY: build test check release
VERSION ?= dev
build:
	go build -trimpath -ldflags "-X main.Version=$(VERSION)" -o pvestand .
test:
	go test -race ./...
check:
	test -z "$$(gofmt -l .)"
	go vet ./...
	go test -race ./...
release:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.Version=$(VERSION)" -o dist/pvestand-linux-amd64 .
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "-s -w -X main.Version=$(VERSION)" -o dist/pvestand-linux-arm64 .
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "-s -w -X main.Version=$(VERSION)" -o dist/pvestand-darwin-arm64 .
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.Version=$(VERSION)" -o dist/pvestand-darwin-amd64 .
