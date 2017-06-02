LDFLAGS := $(shell go run buildscripts/gen-ldflags.go)
PWD := $(shell pwd)
GOPATH := $(shell go env GOPATH)

BUILD_LDFLAGS := '$(LDFLAGS)'
TAG := latest

HOST ?= $(shell uname)
CPU ?= $(shell uname -m)

# if no host is identifed (no uname tool)
# we assume a Linux-64bit build
ifeq ($(HOST),)
  HOST = Linux
endif

# identify CPU
ifeq ($(CPU), x86_64)
  HOST := $(HOST)64
else
ifeq ($(CPU), amd64)
  HOST := $(HOST)64
else
ifeq ($(CPU), i686)
  HOST := $(HOST)32
endif
endif
endif


#############################################
# now we find out the target OS for
# which we are going to compile in case
# the caller didn't yet define OS himself
ifndef (OS)
  ifeq ($(HOST), Linux64)
    arch = gcc
  else
  ifeq ($(HOST), Linux32)
    arch = 32
  else
  ifeq ($(HOST), Darwin64)
    arch = clang
  else
  ifeq ($(HOST), Darwin32)
    arch = clang
  else
  ifeq ($(HOST), FreeBSD64)
    arch = gcc
  endif
  endif
  endif
  endif
  endif
endif

all: install

checks:
	@echo "Check deps"
	@(env bash $(PWD)/buildscripts/checkdeps.sh)
	@echo "Checking project is in GOPATH"
	@(env bash $(PWD)/buildscripts/checkgopath.sh)

getdeps: checks
	@echo "Installing golint" && go get -u github.com/golang/lint/golint
	@echo "Installing gocyclo" && go get -u github.com/fzipp/gocyclo
	@echo "Installing deadcode" && go get -u github.com/remyoudompheng/go-misc/deadcode
	@echo "Installing misspell" && go get -u github.com/client9/misspell/cmd/misspell
	@echo "Installing ineffassign" && go get -u github.com/gordonklaus/ineffassign

verifiers: vet fmt lint cyclo spelling

vet:
	@echo "Running $@"
	@go tool vet -atomic -bool -copylocks -nilfunc -printf -shadow -rangeloops -unreachable -unsafeptr -unusedresult cmd
	@go tool vet -atomic -bool -copylocks -nilfunc -printf -shadow -rangeloops -unreachable -unsafeptr -unusedresult pkg

fmt:
	@echo "Running $@"
	@gofmt -d cmd
	@gofmt -d pkg

lint:
	@echo "Running $@"
	@${GOPATH}/bin/golint -set_exit_status github.com/minio/minio/cmd...
	@${GOPATH}/bin/golint -set_exit_status github.com/minio/minio/pkg...

ineffassign:
	@echo "Running $@"
	@${GOPATH}/bin/ineffassign .

cyclo:
	@echo "Running $@"
	@${GOPATH}/bin/gocyclo -over 100 cmd
	@${GOPATH}/bin/gocyclo -over 100 pkg

build: getdeps verifiers $(UI_ASSETS)

deadcode:
	@${GOPATH}/bin/deadcode

spelling:
	@${GOPATH}/bin/misspell -error `find cmd/`
	@${GOPATH}/bin/misspell -error `find pkg/`
	@${GOPATH}/bin/misspell -error `find docs/`

test: build
	@echo "Running all minio testing"
	@go test $(GOFLAGS) .
	@go test $(GOFLAGS) github.com/minio/minio/cmd...
	@go test $(GOFLAGS) github.com/minio/minio/pkg...

coverage: build
	@echo "Running all coverage for minio"
	@./buildscripts/go-coverage.sh

gomake-all: build
	@echo "Installing minio at $(GOPATH)/bin/minio"
	@go build --ldflags $(BUILD_LDFLAGS) -o $(GOPATH)/bin/minio

pkg-add:
	@echo "Adding new package $(PKG)"
	@${GOPATH}/bin/govendor add $(PKG)

pkg-update:
	@echo "Updating new package $(PKG)"
	@${GOPATH}/bin/govendor update $(PKG)

pkg-remove:
	@echo "Remove new package $(PKG)"
	@${GOPATH}/bin/govendor remove $(PKG)

pkg-list:
	@$(GOPATH)/bin/govendor list

install: gomake-all

release: verifiers
	@MINIO_RELEASE=RELEASE ./buildscripts/build.sh

experimental: verifiers
	@MINIO_RELEASE=EXPERIMENTAL ./buildscripts/build.sh

clean:
	@echo "Cleaning up all the generated files"
	@find . -name '*.test' | xargs rm -fv
	@rm -rf build
	@rm -rf release
