# Copyright 2026 LeadTestKit Authors
# SPDX-License-Identifier: Apache-2.0

.PHONY: all tidy audit test build install vendor

GOFLAGS := -mod=vendor
export CGO_ENABLED := 0

all: tidy audit test build

tidy:
	go fmt ./...
	go mod tidy

audit:
	go vet -all $(GOFLAGS) ./...

test:
	go test $(GOFLAGS) ./...

build:
	go build $(GOFLAGS) -o leadtest ./cmd/leadtest

install:
	go install $(GOFLAGS) ./cmd/leadtest

vendor:
	go mod vendor
