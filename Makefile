.PHONY: check test build clean

VERSION ?= dev
GENERATED_AT ?=

check:
	go run ./cmd/nagare-source validate

test:
	go test ./...

build:
	go run ./cmd/nagare-source build --version "$(VERSION)" $(if $(GENERATED_AT),--generated-at "$(GENERATED_AT)",)

clean:
	rm -rf dist
