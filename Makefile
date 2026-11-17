BINARY := sitegen
ifeq ($(OS),Windows_NT)
BINARY := sitegen.exe
endif

VERSION ?= dev
LDFLAGS := -s -w -X main.version=$(VERSION)
GOFLAGS := -trimpath

.PHONY: all build test vet fmt clean dist generate-example

all: test build

build:
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/sitegen

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

clean:
	rm -rf dist sitegen sitegen.exe

dist: clean
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o dist/sitegen-linux-amd64 ./cmd/sitegen
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o dist/sitegen-linux-arm64 ./cmd/sitegen
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o dist/sitegen-windows-amd64.exe ./cmd/sitegen

generate-example:
	./$(BINARY) generate --site sites/examples/order-admin-test.yaml
