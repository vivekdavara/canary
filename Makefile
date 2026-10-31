.PHONY: build check envtest generate lint manifests test tools

# Tools, pinned and installed into bin/ (gitignored), so a check never depends on what else is on the machine.
CONTROLLER_TOOLS_VERSION ?= v0.21.0
SETUP_ENVTEST_VERSION ?= v0.25.2
# The Kubernetes version envtest runs (its kube-apiserver and etcd), the one k8s.io/api v0.37 is built for.
ENVTEST_K8S_VERSION ?= 1.37.0

BIN := $(CURDIR)/bin
CONTROLLER_GEN := $(BIN)/controller-gen-$(CONTROLLER_TOOLS_VERSION)
SETUP_ENVTEST := $(BIN)/setup-envtest-$(SETUP_ENVTEST_VERSION)

# What CI runs. generate and manifests rewrite the generated files first, so the tests always run against the CRD the
# types describe; CI then fails if that changed anything (the generated files weren't committed).
check: generate manifests lint test

# The deep-copy methods every API type needs (api/v1alpha1/zz_generated.deepcopy.go).
generate: $(CONTROLLER_GEN)
	$(CONTROLLER_GEN) object paths=./api/...

# The CRD from the types and their markers. generateEmbeddedObjectMeta keeps the schema of spec.template.metadata, which
# controller-gen otherwise drops, so the API server would prune the template's labels (DESIGN.md, "Why these choices").
manifests: $(CONTROLLER_GEN)
	$(CONTROLLER_GEN) crd:generateEmbeddedObjectMeta=true paths=./api/... output:crd:artifacts:config=config/crd/bases

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
