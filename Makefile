# BGP operator Makefile

GOBIN ?= $(shell pwd)/bin

.PHONY: build
build:
	go build ./...

.PHONY: test
test:
	go test ./...

.PHONY: lint
lint:
	golangci-lint run ./...

.PHONY: generate
generate:
	controller-gen object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: manifests
manifests:
	controller-gen crd paths="./api/..." output:crd:artifacts:config=config/crd

# --- E2E ---

.PHONY: test-e2e
test-e2e:
	cd test/e2e && task default

# Install chainsaw test runner
.PHONY: chainsaw
chainsaw:
	GOBIN=$(GOBIN) go install github.com/kyverno/chainsaw@v0.2.12
