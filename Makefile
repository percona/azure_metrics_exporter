# Copyright 2015 The Prometheus Authors
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
# http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

GO    := GO15VENDOREXPERIMENT=1 go
PROMU := $(GOPATH)/bin/promu
pkgs   = $(shell $(GO) list ./... | grep -v /vendor/)

PREFIX                  ?= $(shell pwd)
BIN_DIR                 ?= $(shell pwd)
DOCKER_REPO             ?= robustperception
DOCKER_IMAGE_NAME       ?= azure_metrics_exporter
DOCKER_IMAGE_TAG        ?= $(subst /,-,$(shell git rev-parse --abbrev-ref HEAD))

GO_BUILD_LDFLAGS = -X github.com/prometheus/common/version.Version=$(shell cat VERSION) -X github.com/prometheus/common/version.Revision=$(shell git rev-parse HEAD) -X github.com/prometheus/common/version.Branch=$(shell git describe --always --contains --all) -X github.com/prometheus/common/version.BuildUser= -X github.com/prometheus/common/version.BuildDate=$(shell date +%FT%T%z) -s -w

export PMM_RELEASE_PATH?=.

all: format build test

test: build
	@echo ">> running tests"
	@$(GO) test -short $(pkgs)

style:
	@echo ">> checking code style"
	@! gofmt -d $(shell find . -path ./vendor -prune -o -name '*.go' -print) | grep '^'

format:
	@echo ">> formatting code"
	@$(GO) fmt $(pkgs)

vet:
	@echo ">> vetting code"
	@$(GO) vet $(pkgs)

build: promu
	@echo ">> building binaries"
	@$(PROMU) build --prefix $(PREFIX)

tarball: promu
	@echo ">> building release tarball"
	@$(PROMU) tarball --prefix $(PREFIX) $(BIN_DIR)

docker:
	@echo ">> building docker image"
	@docker build -t "$(DOCKER_REPO)/$(DOCKER_IMAGE_NAME):$(DOCKER_IMAGE_TAG)" .

promu:
	@GOOS=$(shell uname -s | tr A-Z a-z) \
		GOARCH=$(subst x86_64,amd64,$(patsubst i%86,386,$(shell uname -m))) \
		$(GO) get -u github.com/prometheus/promu

release:
	go build -ldflags="$(GO_BUILD_LDFLAGS)" -o $(PMM_RELEASE_PATH)/azure_exporter

.PHONY: all style format build test vet tarball docker promu
