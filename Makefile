.PHONY: check test selftest-bt build clean

VERSION ?= dev
GENERATED_AT ?=

check:
	go run ./cmd/nagare-source validate

test:
	go test ./...

selftest-bt:
	go run ./cmd/nagare-source crawl-bt --source sources/bt/example-rss.yaml --response-file fixtures/responses/example-rss.xml --selftest --out /tmp/nagare-source-bt-records.jsonl

build:
	go run ./cmd/nagare-source build --version "$(VERSION)" --bt-records fixtures/bt-index/releases.jsonl $(if $(GENERATED_AT),--generated-at "$(GENERATED_AT)",)

clean:
	rm -rf dist
