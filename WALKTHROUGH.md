# Walkthrough

Questions an interviewer is likely to ask about Canary, with short answers grounded in the code. Day 1 covers the
design, the API and its validation, revisions and the test setup; later days add the rollout itself.

## The idea

**What does it do, in one sentence?**
It rolls out a new version of an application to a growing share of its pods, checks Prometheus metrics at each step,
and promotes the new version or rolls it back on its own (DESIGN.md, first paragraph).

**Why an operator, not a CI script?**
A script runs once and stops; if it dies halfway, the rollout is stuck. An operator reconciles continuously from what
it reads in the cluster, keeps no state of its own (DESIGN.md, "The state machine", last paragraph), so a restart or
a new leader carries on from the status and the two Deployments.

**How is traffic split without a service mesh?**
By pods. The operator runs two Deployments, `<name>-stable` and `<name>-canary`, and the application's Service
selects a label both carry, so a canary with 1 of 10 pods gets about a tenth of new connections. It's coarse:
kube-proxy balances connections, not requests, and with 3 pods a 10% step is really 33%. The status reports the
actual share (`canaryWeight`). Exact weights would need Gateway API `HTTPRoute` weights or a mesh.

**Why does the rollout hold the template instead of pointing at the user's Deployment?**
Then everything the operator changes is its own: both Deployments have it as their controller owner, and nobody
else's controller edits them. Flagger takes the other approach (it targets an existing Deployment and makes a
`-primary` copy); Argo Rollouts takes this one.

## The API

**How do you stop bad input?**
At the API server, before the operator sees it. `api/v1alpha1/canaryrollout_types.go` carries schema markers
(minimum, maximum, maxItems, patterns, `listType=map` for unique metric names) and CEL rules
(`+kubebuilder:validation:XValidation`): strictly increasing weights, durations in range, `min <= max`, no template
labels under the operator's prefix, names of at most 50 characters. `kubectl apply` of a bad rollout fails with the
rule's message. `TestRulesRefuse` checks each refusal's message and `TestRulesAcceptBoundaries` each boundary, so a
rule that refuses too much fails a test too.

**How do you check "strictly increasing" in CEL?**
`self.map(s, s.weight).isSorted() && self.all(s, self.exists_one(t, t.weight == s.weight))`: sorted, and each weight
appears once. The second part is quadratic, which is fine at `maxItems: 20`; CEL rules have a cost budget, and the
API server estimates it from those bounds when the CRD is created.

**What went wrong with the thresholds?**
They were `resource.Quantity` at first (Kubernetes' exact decimal). The API server refused the CRD: the `min <= max`
rule's estimated cost was 1.26 times its budget, because a quantity is an int-or-string with no length bound, and
controller-gen won't put `maxLength` on an int-or-string. Quantities were awkward anyway (`max: 0.01` unquoted is
refused; `10m` means 0.01). They're `float64` now, like Flagger's `thresholdRange`: Prometheus answers in float64 and
the comparison is in float64. The cost is `crd:allowDangerousTypes=true` and the API conventions' warning that floats
may not re-encode identically in every client.

**Why test `pause: 1d`?**
`1d` isn't a Go duration. If the schema let it in, the stored object couldn't be decoded into `metav1.Duration`; and
since a list is decoded as one JSON document, I'd expect the operator's list of all rollouts to fail with it, not
just that one object (not tested, since the API server now refuses the object). The CEL rule
`duration(self) >= ...` fails to parse it, so the API server refuses it (`TestRulesRefuseRawInput`).

**Why `generateEmbeddedObjectMeta`?**
controller-gen leaves the schema of an embedded `ObjectMeta` empty by default, and the API server prunes fields a
structural schema doesn't declare, so `spec.template.metadata.labels` would vanish on write and the pods would lose
the labels the Service selects. With the option they're declared and kept (`TestDefaultsAndTemplateLabelsSurvive`).

**Why must the CRD be applied with `--server-side`?**
It is 636,521 bytes with the pod schema's descriptions. Client-side apply copies the whole object into the
`kubectl.kubernetes.io/last-applied-configuration` annotation, and annotations are limited to 256 KiB. Server-side
apply keeps no such copy. `crd:maxDescLen=0` would shrink it to 203,435 bytes but leave `kubectl explain` empty.

## Revisions

**How do you tell whether the template changed?**
`internal/revision.Of`: the first 16 hex digits of the SHA-256 of the template's JSON. Go's encoder writes struct
fields in a fixed order and map keys sorted, so map order doesn't matter (`TestEqualTemplatesGetEqualRevisions`
shuffles 50 labels' insertion order 100 times). Quantities are canonical only within a format: `0.5` and `500m` hash
alike, `1Gi` and `1073741824` don't, so rewriting one in the other format starts a rollout (the review found my
first comment claiming otherwise). It goes on pods as a label, so
16 hex digits also fits the 63-character limit.

**Which template do you hash, the one the user sent?**
No, the stored one. The CRD carries the pod schema's defaults, so the API server fills in, for example, a port's
`protocol: TCP`; the sent and stored templates hash differently (`TestServerDefaultsInsideTheTemplate`). The
reconciler only ever sees the stored one.

**What could change every revision at once?**
A `k8s.io/api` upgrade that adds a field without `omitempty`: every unchanged template would get a new revision and
every application would start a rollout. `TestRevisionIsPinned` pins one revision (I checked it with `shasum` on the
encoded JSON), so such an upgrade fails a test instead of shipping.

## The reconciler

**What does it do on day 1?**
It reads the rollout and writes `status.observedGeneration` and `status.updateRevision`, leaving the rest of the
status alone (`TestReconcileKeepsTheRestOfTheStatus`). If nothing changed, it writes nothing.

**Why the optimistic lock on the status patch?**
A merge patch without a `resourceVersion` overwrites whatever is there. If the spec changed between the read and the
write, an unlocked patch could record a status computed from the older spec. With
`client.MergeFromWithOptimisticLock` the write fails with a conflict instead, and the reconciler returns without an
error: the change that caused the conflict is an event of its own and brings another reconcile.

**Why does `status` being a subresource matter?**
Updates to the object can't change the status and status updates can't change the spec, and only spec changes move
`metadata.generation`, which is what `observedGeneration` is compared with (`TestStatusIsASubresource`).

## Testing

**What is envtest, and what doesn't it give you?**
A real kube-apiserver and etcd (downloaded by `setup-envtest` into `bin/k8s`) with the CRD installed, so validation,
defaulting, pruning, RBAC and status subresources are the real ones. It runs none of Kubernetes' own controllers: a
Deployment never gets ReplicaSets or pods, and a deleted namespace never finishes terminating (so each test gets a
fresh namespace, `testenv.Namespace`). From day 2 tests set Deployment status themselves; day 4 adds kind for a real
cluster.

**How do you know the operator's RBAC is enough?**
The tests run the operator as a user holding exactly `config/rbac/role.yaml`, which controller-gen generates from the
reconciler's `+kubebuilder:rbac` markers (`testenv.OperatorConfig`). Removing the Deployments rule from it fails
`TestStatusFollowsTheSpec` in 12 seconds: the Deployment informer can't list, so its cache never syncs.

**And with more than one replica?**
`--leader-elect` makes the replicas elect a leader through a Lease, and only the leader reconciles. The Lease lives in
one namespace, so its permissions are a namespaced Role (`config/rbac/leader_election_role.yaml`), not part of the
cluster-wide one. Day 1 first shipped the flag without either the Role or a way to name the namespace outside a
cluster; `TestRunElectsALeader` now runs the binary with only that Role and checks it takes the Lease and reconciles.

**How can CI not pass by skipping the envtest suites?**
Without `KUBEBUILDER_ASSETS` a plain `go test ./...` skips them; `make test` sets `CANARY_REQUIRE_ENVTEST`, which makes
`testenv.Main` fail the package instead. CI runs `make check`.

**What does the binary's test add?**
`cmd/canary/main_test.go` runs `run()` from its flags with a kubeconfig file for the operator's user: the probes
answer, a rollout is reconciled, `controller_runtime_reconcile_total{controller="canaryrollout",result="success"}`
appears on `/metrics`, and a cancel stops it cleanly. The controller tests can't see a missing scheme registration
or a controller never set up in `main`; this one can.

## The review (day 1)

**What did the review find?**
One agent, at `2694208`, 11 minutes, in its own worktree, findings as failing tests. Reproduced:

- A template with no pod spec was accepted (`containers` was only required once `spec` existed): fixed with a CEL
  rule, tested.
- **A probe port over int32 is accepted, and then the operator can't decode the object**, and since a list decodes
  as one document, one such rollout in any namespace stopped the operator from reconciling every other one. Not
  fixed on day 1: the fix is in how the operator reads rollouts (each object decoded on its own), day 2's first job.
- `--leader-elect` couldn't work with the generated role (I had found and fixed it meanwhile, `f89ec6e`).
- Template label syntax isn't checked; the CEL rule for it is over budget (DESIGN.md, "What the API server does not
  catch").
- `weight: 10.0` from a raw-JSON client fails a rule with an evaluation error.
- Equal quantities in different formats give different revisions (now documented and tested as the behaviour).

It also listed six mechanisms the tests didn't notice removed; the four in the CRD (minItems on both lists, the
metric name's maxLength, the label rule's guards) now have tests, each re-run as a mutant. The three in the
reconciler (the optimistic lock, the no-change early return, the conflict branch) still have none.

## What went wrong on day 1

- The `resource.Quantity` thresholds and the CEL cost budget (above): found the first time envtest installed the CRD.
- Five expected refusal messages in the first test run were my guesses of the API server's wording ("should be
  greater than or equal to 1" is really "spec.replicas in body should be greater than or equal to 1"); the rules were
  right, the expectations weren't.
- A test run that "passed" in 0.4 s had skipped everything: the shell's `export KUBEBUILDER_ASSETS` doesn't carry
  over between tool calls. That's the case `CANARY_REQUIRE_ENVTEST` exists for.
- `--leader-elect` was a flag with nothing behind it (above).
- The public GitHub repository couldn't be created from the scheduled session, so CI hasn't run.
