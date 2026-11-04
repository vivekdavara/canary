# Changelog

## v1.0.0 (2026-11-04)

The first release: a Kubernetes operator that rolls a new version of an application out beside the old one in
steps, checks Prometheus metrics at each step, and promotes it or rolls it back, without dropping below the
rollout's `replicas` ready pods. Go 1.27, controller-runtime v0.25, Kubernetes 1.37.

### The API

- `CanaryRollout` (`rollouts.vivekdavara.github.io/v1alpha1`, short name `crl`): a pod template, `replicas`, steps
  (a weight from 1 to 100 and a pause), an optional analysis (Prometheus queries with `min`/`max` thresholds, an
  interval, a failure limit) and a progress deadline. Every rule a user can break is checked by the API server
  (OpenAPI schema and CEL); what the schema can't check inside the pod template, the Deployment's own validation does,
  reported on the rollout (`InvalidSpec`).
- Status: the phase (`Healthy`, `Progressing`, `Promoting`, `RolledBack`), the stable and canary revisions, the step,
  the canary's actual share of the pods, the analysis's last check and failures, and the `Available`, `Progressing`
  and `AnalysisPassing` conditions. `kubectl get crl` shows the phase, step, canary count, share and revision.

### The rollout

- Two Deployments per rollout (`<name>-stable`, `<name>-canary`), both selected by the application's own Service, so
  the canary's share of traffic is its share of the pods.
- Capacity first: each step scales the canary up, waits for its pods to be ready, then scales the stable down; a
  promotion keeps the canary until the stable's new pods are ready; a rollback keeps it until the stable is back.
- The decisions are one pure function (`rollout.Decide`) from the spec, the last status and the two Deployments to
  what each Deployment should become and the next status; the reconciler reads, decides, writes the Deployments,
  then the status (with an optimistic lock), then the Events.
- Restarts, lost status writes, deleted Deployments, edits mid-rollout and template changes during a rollout,
  promotion or rollback are handled from what is stored (no state in the operator).

### Analysis and rollback

- Checks every `interval` during each step's pause, all of a check's queries in parallel, each bounded by
  `--analysis-timeout`. A check fails closed: no samples, more than one, NaN, an error, a timeout, a partial answer or
  no Prometheus all fail.
- More failed checks in a step than `failureLimit`, or a step whose pods aren't ready within
  `progressDeadlineSeconds`, rolls back: the stable back to all its pods on the old version, then the canary drained;
  the rollout then holds until the template changes. Warning Events and conditions say why.

### Running it

- **Helm chart** (`charts/canary`): `helm install canary charts/canary -n canary-system --create-namespace --set
  prometheus.url=...`. The CRD and the ClusterRole's rules are copies of the generated files; values are checked by
  a JSON schema written to agree with the operator's own checks (property-tested); more than one replica requires
  leader election; without it the operator is replaced with `Recreate`. Objects are named after the release.
- **Kustomize** (`config/default`): `kubectl apply --server-side -k config/default`, the same objects.
- The operator: two replicas with leader election (a stopped leader gives the Lease up at once, so an upgrade or
  drain doesn't pause reconciling for the Lease's 15 seconds; a crashed one is replaced when the Lease expires), non-root, read-only root filesystem, no capabilities, liveness and
  readiness probes, Prometheus metrics on `:8111`. The image (`make images`) is a static binary on distroless; no
  image is published.

### Tests

- envtest (a real API server and etcd) for the CRD's validation, the reconciler, the binary, the manifests and the
  chart (`helm install`, upgrade and uninstall); the operator runs as a user holding only the generated permissions.
- Seeded random walks of `Decide` (tens of thousands of rollouts with lost writes, deletions, edits, checks and
  deadlines) checking capacity after every decision.
- End to end on kind in CI, with a real Prometheus and a demo app: a version promoted, one rolled back by measured
  errors, one by the deadline, one promoted across the leader being killed (SIGKILL) and one across an operator
  upgrade (`helm upgrade`), and the ResourceQuota limitation reproduced.

### Known limitations

See README, "Limitations". The ones most likely to matter: replica ratio is a coarse traffic split (connections, not
requests); a tight ResourceQuota can hold a rollback with the rolled-back version serving; reconciles run one at a
time, so a slow Prometheus delays other rollouts; a Prometheus outage rolls rollouts in progress back once the failure
limit is used up.
