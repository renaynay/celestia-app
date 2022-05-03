#!/usr/bin/make -f

PACKAGES=$(shell go list ./... | grep -v '/simulation')
VERSION := $(shell echo $(shell git describe --tags) | sed 's/^v//')
COMMIT := $(shell git log -1 --format='%H')
DOCKER := $(shell which docker)
DOCKER_BUF := $(DOCKER) run --rm -v $(CURDIR):/workspace --workdir /workspace bufbuild/buf
IMAGE := ghcr.io/tendermint/docker-build-proto:latest
DOCKER_PROTO_BUILDER := docker run -v $(shell pwd):/workspace --workdir /workspace $(IMAGE)

# process linker flags

ldflags = -X github.com/cosmos/cosmos-sdk/version.Name=celestia-app \
		  -X github.com/cosmos/cosmos-sdk/version.AppName=celestia-appd \
		  -X github.com/cosmos/cosmos-sdk/version.Version=$(VERSION) \
		  -X github.com/cosmos/cosmos-sdk/version.Commit=$(COMMIT) \
		  -X "github.com/cosmos/cosmos-sdk/version.BuildTags=$(build_tags_comma_sep)"
ldflags += $(LDFLAGS)

BUILD_FLAGS := -ldflags '$(ldflags)'

all: install

mod:
	@go mod tidy

pre-build:
	@echo "Fetching latest tags"
	@git fetch --tags

build: mod
	@go install github.com/gobuffalo/packr/v2/packr2@latest
	@cd ./cmd/celestia-appd
	@mkdir -p build/
	@go build -o build/ ./cmd/celestia-appd
	@go mod tidy

install: go.sum
		@echo "--> Installing celestia-appd"
		@go install -mod=readonly $(BUILD_FLAGS) ./cmd/celestia-appd

go.sum: mod
		@echo "--> Ensure dependencies have not been modified"
		GO111MODULE=on go mod verify

test:
	@go test -mod=readonly $(PACKAGES)

proto-gen:
	$(DOCKER) run --rm -v $(CURDIR):/workspace --workdir /workspace tendermintdev/sdk-proto-gen:v0.2 sh ./scripts/protocgen.sh

proto-lint:
	@$(DOCKER_BUF) lint --error-format=json

proto-format:
	@echo "Formatting Protobuf files"
	@$(DOCKER_PROTO_BUILDER) find . -name '*.proto' -path "./proto/*" -exec clang-format -i {} \;
.PHONY: proto-format

build-docker:
	$(DOCKER) build -t celestiaorg/celestia-app -f docker/Dockerfile .


###############################################################################
###                           Tests & Simulation                            ###
###############################################################################
# The below include contains the tools target.
include contrib/devtools/Makefile
include contrib/devtools/sims.mk

test: test-unit test-build

test-all: check test-race test-cover

test-unit:
	@VERSION=$(VERSION) go test -mod=readonly -tags='ledger test_ledger_mock' ./...

test-race:
	@VERSION=$(VERSION) go test -mod=readonly -race -tags='ledger test_ledger_mock' ./...

benchmark:
	@go test -mod=readonly -bench=. ./...

test-cover:
	@export VERSION=$(VERSION); bash -x contrib/test_cover.sh
.PHONY: test-cover



