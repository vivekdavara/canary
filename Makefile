.PHONY: build check e2e e2e-cluster e2e-diagnose e2e-down e2e-install e2e-test envtest generate helm images lint manifests test tools

# Tools, pinned and installed into bin/ (gitignored), so a check never depends on what else is on the machine.
CONTROLLER_TOOLS_VERSION ?= v0.21.0
SETUP_ENVTEST_VERSION ?= v0.25.2
# Helm, for the chart (charts/canary): built from source by go install, like the other tools.
HELM_VERSION ?= v4.3.1
# The Kubernetes version envtest runs (its kube-apiserver and etcd), the one k8s.io/api v0.37 is built for.
ENVTEST_K8S_VERSION ?= 1.37.0

BIN := $(CURDIR)/bin
CONTROLLER_GEN := $(BIN)/controller-gen-$(CONTROLLER_TOOLS_VERSION)
SETUP_ENVTEST := $(BIN)/setup-envtest-$(SETUP_ENVTEST_VERSION)
HELM := $(BIN)/helm-$(HELM_VERSION)

# What CI runs. generate and manifests rewrite the generated files first, so the tests always run against the CRD the
# types describe; CI then fails if that changed anything (the generated files weren't committed).
check: generate manifests lint test

# The deep-copy methods every API type needs (api/v1alpha1/zz_generated.deepcopy.go).
generate: $(CONTROLLER_GEN)
	$(CONTROLLER_GEN) object paths=./api/...

# The CRD from the types and their markers. generateEmbeddedObjectMeta keeps the schema of spec.template.metadata, which
# controller-gen otherwise drops, so the API server would prune the template's labels; allowDangerousTypes allows the
# float64 thresholds (DESIGN.md, "Why these choices", says why both).
# The operator's ClusterRole comes from the reconciler's rbac markers. The chart gets copies of both (Helm reads only
# files inside the chart), so it installs the same CRD and grants the same rules.
manifests: $(CONTROLLER_GEN)
	$(CONTROLLER_GEN) crd:generateEmbeddedObjectMeta=true,allowDangerousTypes=true paths=./api/... output:crd:artifacts:config=config/crd/bases
	$(CONTROLLER_GEN) rbac:roleName=canary-manager paths=./internal/controller/... output:rbac:artifacts:config=config/rbac
	cp config/crd/bases/rollouts.vivekdavara.github.io_canaryrollouts.yaml charts/canary/crds/
	cp config/rbac/role.yaml charts/canary/files/role.yaml

tools: $(CONTROLLER_GEN) $(SETUP_ENVTEST) $(HELM)

# go install names the binary after the package, so each is renamed with its version: a version bump installs anew.
$(CONTROLLER_GEN):
	GOBIN=$(BIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)
	mv $(BIN)/controller-gen $@

$(SETUP_ENVTEST):
	GOBIN=$(BIN) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(SETUP_ENVTEST_VERSION)
	mv $(BIN)/setup-envtest $@

$(HELM):
	GOBIN=$(BIN) go install helm.sh/helm/v4/cmd/helm@$(HELM_VERSION)
	mv $(BIN)/helm $@

# Prints the pinned helm's path, installing it on first use: $$(make -s helm) template canary charts/canary
helm: $(HELM)
	@echo $(HELM)

# Downloads envtest's binaries into bin/k8s once; prints their directory.
envtest: $(SETUP_ENVTEST)
	@$(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(BIN)/k8s -p path

# gofmt skips .scratch/ (review worktrees) and bin/; go vet's ./... skips dot-directories by itself.
lint: $(HELM)
	@unformatted="$$(find . \( -path ./.scratch -o -path ./bin \) -prune -o -name '*.go' -print | xargs gofmt -l)"; if [ -n "$$unformatted" ]; then echo "not gofmt-formatted:"; echo "$$unformatted"; exit 1; fi
	go vet ./...
	go vet -tags e2e ./test/e2e/
	$(HELM) lint --strict charts/canary

# CANARY_REQUIRE_ENVTEST makes the envtest suites fail, not skip, when they can't find the binaries, so this target
# (and CI) can't pass by skipping them; a plain `go test ./...` skips them. The chart's tests run the pinned helm
# (CANARY_HELM), and skip without it, the same way.
test: $(SETUP_ENVTEST) $(HELM)
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(BIN)/k8s -p path)" \
		CANARY_HELM=$(HELM) CANARY_REQUIRE_ENVTEST=1 go test -race -count=1 -timeout 5m ./...

build:
	go build -o bin/ ./cmd/...

# The images: the operator (canary:dev) and the end-to-end tests' demo app (canary-demoapp:dev), each a static Linux
# binary for this machine's architecture copied onto distroless (Dockerfile).
images:
	CGO_ENABLED=0 GOOS=linux go build -trimpath -o bin/linux/ ./cmd/canary ./test/e2e/demoapp
	docker build --build-arg BINARY=canary -t canary:dev .
	docker build --build-arg BINARY=demoapp -t canary-demoapp:dev .

# The end-to-end tests (test/e2e; DESIGN.md, "End to end") on a kind cluster: make e2e creates the cluster, builds and
# loads the images, installs the operator and a Prometheus, and runs the tests; make e2e-down deletes the cluster. CI
# runs them on every push. They need Docker and kind.
KIND ?= kind
KIND_CLUSTER ?= canary-e2e
# kind v0.33.0's node image for Kubernetes 1.37.0, the version envtest runs.
KIND_NODE_IMAGE ?= kindest/node:v1.37.0@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5
# envtest's kubectl (the one the install tests use), against the kind cluster.
KUBECTL = $(shell $(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(BIN)/k8s -p path)/kubectl --context kind-$(KIND_CLUSTER)

e2e: e2e-cluster e2e-install e2e-test

e2e-cluster:
	$(KIND) create cluster --name $(KIND_CLUSTER) --image $(KIND_NODE_IMAGE) --config test/e2e/kind.yaml --wait 2m

# The operator is installed by the chart, as a user would (its default image is canary:dev, the one loaded here).
e2e-install: images $(SETUP_ENVTEST) $(HELM)
	$(KIND) load docker-image --name $(KIND_CLUSTER) canary:dev canary-demoapp:dev
	$(KUBECTL) apply --server-side -k test/e2e/config
	$(HELM) install canary charts/canary --kube-context kind-$(KIND_CLUSTER) -n canary-system --create-namespace \
		--set prometheus.url=http://prometheus.canary-e2e.svc:9090
	$(KUBECTL) -n canary-system rollout status deployment/canary --timeout 2m
	$(KUBECTL) -n canary-e2e rollout status deployment/prometheus --timeout 2m

# CANARY_HELM: the operator-upgrade test runs helm upgrade on the release e2e-install made. CANARY_E2E_NODE: the crash
# test kills the leader's container through the node's crictl.
e2e-test: $(HELM)
	CANARY_HELM=$(HELM) CANARY_E2E_NODE=$(KIND_CLUSTER)-control-plane go test -tags e2e -count=1 -v -timeout 15m ./test/e2e/

# What CI prints when the end-to-end tests fail.
e2e-diagnose: $(SETUP_ENVTEST)
	KUBECTL="$(KUBECTL)" test/e2e/diagnose.sh

e2e-down:
	$(KIND) delete cluster --name $(KIND_CLUSTER)
