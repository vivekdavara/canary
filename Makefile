.PHONY: build check envtest lint test tools

# Tools, pinned and installed into bin/ (gitignored), so a check never depends on what else is on the machine.
CONTROLLER_TOOLS_VERSION ?= v0.21.0
SETUP_ENVTEST_VERSION ?= v0.25.2
# The Kubernetes version envtest runs (its kube-apiserver and etcd), the one k8s.io/api v0.37 is built for.
ENVTEST_K8S_VERSION ?= 1.37.0

BIN := $(CURDIR)/bin
CONTROLLER_GEN := $(BIN)/controller-gen-$(CONTROLLER_TOOLS_VERSION)
SETUP_ENVTEST := $(BIN)/setup-envtest-$(SETUP_ENVTEST_VERSION)

# What CI runs.
check: lint test

tools: $(CONTROLLER_GEN) $(SETUP_ENVTEST)

# go install names the binary after the package, so each is renamed with its version: a version bump installs anew.
$(CONTROLLER_GEN):
	GOBIN=$(BIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)
	mv $(BIN)/controller-gen $@

$(SETUP_ENVTEST):
	GOBIN=$(BIN) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(SETUP_ENVTEST_VERSION)
	mv $(BIN)/setup-envtest $@

# Downloads envtest's binaries into bin/k8s once; prints their directory.
envtest: $(SETUP_ENVTEST)
	@$(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(BIN)/k8s -p path

# gofmt skips .scratch/ (review worktrees) and bin/; go vet's ./... skips dot-directories by itself.
lint:
	@unformatted="$$(find . \( -path ./.scratch -o -path ./bin \) -prune -o -name '*.go' -print | xargs gofmt -l)"; if [ -n "$$unformatted" ]; then echo "not gofmt-formatted:"; echo "$$unformatted"; exit 1; fi
	go vet ./...

# CANARY_REQUIRE_ENVTEST makes the envtest suites fail, not skip, when they can't find the binaries, so this target
# (and CI) can't pass by skipping them; a plain `go test ./...` skips them.
test: $(SETUP_ENVTEST)
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(BIN)/k8s -p path)" \
		CANARY_REQUIRE_ENVTEST=1 go test -race -count=1 -timeout 5m ./...

build:
	go build -o bin/ ./cmd/...
