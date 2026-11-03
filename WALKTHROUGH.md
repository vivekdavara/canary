# Walkthrough

Questions an interviewer is likely to ask about Canary, with short answers grounded in the code. Day 1 covered the
design, the API and its validation, revisions and the test setup; day 2 the rollout itself (the split, the state
machine, the reconciler's reads and writes); day 3 the analysis and rollback.

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

## The rollout

**Walk me through a rollout.**
`kubectl apply` a rollout: the operator creates `web-stable` from the template at `replicas` pods (phase `Healthy`).
Change the image: the template's revision changes, so the operator creates `web-canary` with the new template at
step 0's count (say 1 of 10 pods) while the stable keeps all 10. When the canary's pod is ready, the stable goes down
to 9 and the step's pause starts. When the pause ends, the next step (say 50%: canary to 5, stable still 9 until
those are ready, then 5). After the last step the stable takes the new template at 10 pods (`Promoting`), and once
they're ready the canary goes to 0 and the phase is `Healthy` at the new revision. `TestARolloutEndToEnd` does
exactly this against the API server.

**Why scale the canary up before the stable down?**
So capacity never drops. Scaling both at once would remove stable pods before the canary's replacements are ready;
the order means the rollout runs a few pods more than `replicas` for a moment instead of a few fewer. The same rule
shows up three more times: the canary keeps its pods during a promotion until the stable's new pods are ready, after
a revert until the stable is back at full size, and the Deployments themselves replace pods with
`maxUnavailable: 0`.

**How do you know capacity never drops?**
`TestRandomWalks`: 3,000 seeded walks of random actions (pods becoming ready in one Deployment or the other, time
passing, a new template, the template going back) through `Decide` and a hand-written Deployment controller model,
checking after every decision that the ready pods add up to at least `replicas`, that the status agrees with itself,
and that every walk ends `Healthy` at the template. Removing any of the capacity rules fails it. It found a real hole
while I wrote `Decide`: after a revert, a new template could start a rollout while the old canary still had pods, and
step 0 scaled that canary straight down while the stable was still coming back. Now an oversized canary is held
(old template, old count) until the stable is settled.

**Why hold the old canary rather than give it the new template right away?**
Giving the new version the old canary's pod count would expose it to more than step 0's share; scaling it down first
would drop capacity. Holding the old version for the time the stable takes to come back is the only option that does
neither.

**Why is the state machine a pure function?**
`rollout.Decide` takes the spec, the last status and what was read of the two Deployments, and returns the targets,
the next status, the condition and the Events. With no API server in it, a whole rollout is a unit test, and so are
thousands of random ones. The reconciler (`internal/controller`) only reads, calls `Decide`, and writes.

**What state does the operator keep?**
None of its own. The status holds the phase, the revision under test, the step and when its pause began; the
Deployments hold which revision each runs (a label on the pod template). `TestANewOperatorCarriesOn` stops one
operator mid-pause and starts another with a clock ten minutes later: the pause still ends from the first one's
recorded start.

**What happens when the template changes mid-rollout?**
A new revision restarts the rollout at step 0 with it (`RolloutStarted` says which rollout it replaces). The
template going back to the stable's revision ends the rollout instead (`RolloutReverted`): the stable goes back to all
replicas, and the canary keeps its pods until it has them.

**How is a pause timed?**
From the moment the step's canary pods are ready (`stepStartedAt`), not from when the step began, so a slow image
pull doesn't eat the observation window. The reconciler returns `RequeueAfter` the time left. `TestAPauseEndsOnItsOwn`
(a real 2 s pause) found that a reconcile with nothing to write returned without that requeue: the status write
itself causes one more reconcile, which found nothing to write and dropped the timer, so a paused step never ended
unless something else woke the operator.

**When is a Deployment "ready"?**
When the Deployment controller has seen the latest spec (`status.observedGeneration`), and its updated, ready and
total pods all equal its replicas: what `kubectl rollout status` waits for, with readiness. Total matters: it rules out
old pods still around during a rolling update.

**How do you compare a Deployment's template with the rollout's?**
By the revision label the operator puts on it, not by deep equality. The API server fills defaults into a stored pod
template (`terminationMessagePath`, `dnsPolicy`, ...), so a field-by-field comparison would always differ and the
operator would rewrite the Deployment on every reconcile. The cost: a hand edit to a Deployment's template isn't
undone.

## The reconciler

**What does one reconcile do?**
Reads the rollout (from the API server, one object), reads `<name>-stable` and `<name>-canary` (from the cache),
calls `Decide`, writes the Deployments, then the status, then records the Events. A reconcile with nothing new writes
nothing (`TestAReconcileWithNothingNewWritesNothing` counts the calls through an interceptor client).

**Why read each rollout from the API server instead of the cache?**
Day 1's review found that the API server stores a rollout whose probe port is beyond int32 (the CRD's copy of the pod
schema doesn't bound int-or-string fields), the typed client can't decode it, and a typed cache decodes the whole list
as one document: one such rollout anywhere stopped the operator for every rollout. Now the watch is metadata-only and
`Reconcile` reads the rollout as unstructured JSON and decodes it itself; a bad one gets `InvalidSpec` on its own
status (`TestOneUndecodableRolloutDoesNotStopTheOthers`). The price is one GET per reconcile.

**Why write the Deployments before the status?**
So the status describes what really runs: if the API server refuses a Deployment (a template it can't accept), the
status doesn't claim a phase that never happened. The opposite failure, the status write lost after a Deployment was
written, matters in one place: a promotion. The stable then runs the revision under test while the status still says
`Progressing`, which would look like the template going back. `Decide` treats a stable at the revision under test as a
promotion (`TestAPromotionWhoseStatusWasLost`).

**Why are Events recorded after the status write?**
Events come from transitions, and a transition is real once the status records it. If the status write loses a
conflict, the next reconcile decides again from the same old status and produces the same Events; sending them only
after a successful write sends each once.

**Why the optimistic lock on the status patch?**
A merge patch without a `resourceVersion` overwrites whatever is there. If the spec changed between the read and the
write, an unlocked patch could record a status computed from the older spec. With
`client.MergeFromWithOptimisticLock` the write fails with a conflict instead, and the reconciler returns without an
error: the change that caused the conflict is an event of its own. `TestAStatusFromAnOlderSpecIsNotWritten` changes
the spec between read and patch; removing the lock fails it. Deployment updates carry the `resourceVersion` too, so a
stale cache read gets a conflict rather than undoing a newer write.

**What if a Deployment called `web-stable` already exists?**
If the rollout isn't its controller owner, it's someone else's: the operator leaves it alone and reports
`DeploymentConflict` with its name (`TestSomeoneElsesDeploymentWithTheName`).

**What does the API server still refuse that the CRD let through?**
Anything a Deployment's validation catches in the pod template: a label value with `!`, an invalid container name.
The operator reports the refusal as `InvalidSpec` with the API server's message and a Warning Event, and creates
nothing until the template changes (`TestATemplateTheAPIServerRefuses`).

**Why does `status` being a subresource matter?**
Updates to the object can't change the status and status updates can't change the spec, and only spec changes move
`metadata.generation`, which is what `observedGeneration` is compared with (`TestStatusIsASubresource`).

## The analysis

**How does a check decide pass or fail?**
Each metric's query goes to Prometheus as an instant query (`internal/analysis/prometheus.go`), and `Evaluate`
(`analysis.go`) passes it only on exactly one finite number within `[min, max]`, both bounds inclusive. Everything
else fails: an empty vector, two samples, NaN, ±Inf, a matrix, an API error, a proxy's HTML page, a 500, an answer
over 1 MiB, a refused connection, a timeout. A check passes when every metric does (`Check.Passed`). The table in
`TestEvaluate` lists each case, and `TestPrometheusErrors` each malformed answer.

**Why fail closed?**
A check that passes when it can't measure would promote a version nobody measured, which is the one thing a canary
exists to prevent. Failing closed during a Prometheus outage costs a rollback of a possibly good version: a retry,
with the Events and the `AnalysisPassing` condition naming the metric and the error. `failureLimit` is there to ride
out short outages. Without `--prometheus-url` every check fails with a message naming the flag
(`TestWithoutPrometheusAnAnalysisFailsClosed`).

**`Decide` is pure, but a check is I/O. How do they fit?**
Two calls. A decision whose check is due returns `AnalysisDue` and otherwise holds the step; the reconciler runs the
check and calls `Decide` again with the same input (the same clock reading) plus the result in `Input.Check`. The
second decision records the check, updates the counts, sets `AnalysisPassing`, and rolls back if the failures pass
the limit, all in one decision, so the status write that follows covers it (`reconcile` in
`internal/controller/canaryrollout_controller.go`). The pure tests do the same in `world.run`, so every test of
`Decide` exercises the same two-call path.

**When do checks run?**
One interval after the step's pause starts, then every interval after the last. Not at the start: the canary's pods
have just become ready, a `rate(...[1m])` over them is empty, and empty fails closed. A step with analysis passes only
when its pause is over and it has had at least one check, so a 0s pause lasts one interval
(`TestAPauseShorterThanTheIntervalWaitsForACheck`); with a 2m pause and 30s interval the checks are at 30, 60, 90 and
120 s and the step passes on the last (`TestChecksRunEveryIntervalDuringThePause`).

**Why is `lastCheckAt` a MicroTime when the other times are seconds?**
The API server stores a `metav1.Time` to the second, and for check times both roundings were wrong. Rounded up (my
first try), each next check came up to a second late, which compounds: a 1s interval checked every 2s (the envtest
test saw 2 queries in a 3 s pause). Rounded down (my second), a check that ran 0.9 s late was recorded 0.9 s early,
so a reconcile 100 ms later ran another one, counting one bad moment twice (the review's test). A `MicroTime` is
stored to the microsecond, so the next check is exactly an interval after the last. Pause starts and the deadline's
start stay seconds, rounded up, because there a second late is harmless and a second early isn't.

**What stops drift from skipping the last check of a pause?**
A step with analysis passes only on a check made at or after its pause's end. Checks drift a few milliseconds per
interval (each is an interval after the actual last one), so "pause over and at least one check" let a 3 s pause at
1 s end at 3.000 s with checks at 1.005 and 2.010, the last interval unmeasured. Now it waits for the check at 3.015 s
(`TestAStepEndsOnACheckAtOrAfterItsPausesEnd`).

**What happens to the checks when someone edits the rollout mid-step?**
They belong to the pause. An edit that changes the step's pods (`replicas`, the steps) restarts the pause and clears
the checks and failures with it (`TestChecksStartOverWithThePause`). The failure limit is read at every decision, so
lowering it below the failures so far rolls back at once (`TestALoweredFailureLimitRollsBack`; before that fix, a
step could pass with more failures than the spec allowed).

**What if a check's status write is lost?**
The check is due again on the next reconcile and runs again; the lost result was never counted, and its
`AnalysisFailed` Event never went out (Events are recorded only after the status write). The random walk loses writes
in 2,000+ decisions that ran a check.

**Doesn't a slow Prometheus block the operator?**
Up to `--analysis-timeout` (10 s) per check: reconciles run one at a time and the check runs inside one, so other
rollouts wait that long at worst. The metrics of one check are queried in parallel (`TestRunChecksEveryMetricInParallel`),
and a hung server fails the check at the timeout (`TestASlowPrometheusFailsTheCheckAtTheTimeout`). Raising
`MaxConcurrentReconciles` would isolate rollouts from each other; it isn't done.

**Could a user make the operator query an arbitrary URL?**
No: the Prometheus address is the operator's flag, not a rollout field. A rollout controls only the PromQL text, which
goes to that one server as a form field.

## Rollback

**What triggers a rollback?**
Two things (`Decide` in `internal/rollout/decide.go`): a step whose canary pods aren't ready within
`progressDeadlineSeconds` of the step starting to wait (`waitForCanary`), and more failed checks in a step than
`failureLimit`. Both go through `rollBack`.

**What does a rollback do?**
Phase `RolledBack`, `rolledBackRevision` set, a Warning `RolledBack` Event with the cause, `Progressing` False with
the cause. The stable goes back to `replicas` pods with the template it already has (the old version); the canary
keeps its pods until those are all ready, then goes to 0 (`holdBack`), the same capacity rule as a promotion
(`TestARollbackKeepsTheCanaryUntilTheStableIsBack`). Then it holds: while the template is still the failed revision,
nothing is retried, and the next reconciles change nothing and write nothing (`TestARolledBackRolloutsWrites` checks two
quiet seconds).

**How does the deadline fire if nothing changes?**
`waitForCanary` returns a requeue at the deadline, so the operator wakes itself. `TestTheProgressDeadlineRollsBack` uses
a 2 s deadline and a canary that never gets ready: nothing else happens, and the rollout rolls back on time.

**How does a user find out why it rolled back?**
The `Progressing` condition's message is written at the rollback and kept while the rollout holds (`KeepMessage`; an
envtest test caught the hold overwriting it with a generic one). The `AnalysisPassing` condition has the last
check's values against their bounds, and the Events (`AnalysisFailed`, `RolledBack`) have the history while they last.

**Why can't you roll back during a promotion?**
Promotion gives the stable Deployment the new template, and that template was the only copy of the old version: the
operator keeps no other. A version only gets there after passing every step and check.

**What if someone deletes the stable Deployment after a rollback?**
Normally a deleted stable is recreated from the template. After a rollback the template is the failed version, so
that would run it at all replicas with no canary and no analysis. The operator creates nothing and says so; the next
template change recreates it (`TestADeletedStableAfterARollback`).

**How do I retry a rolled-back version?**
Change the template: the hold ends at any template change. To retry the exact same version, change something
harmless in the template (an annotation), which makes it a new revision. A rolled-back revision set again after
another rollout has started is rolled out again (`TestAfterARollbackTheTemplateChanges`).

**How did you test rollback beyond the examples?**
`TestRandomWalksWithAnalysis`: 4,000 walks with checks passing or failing at random (per walk, a 100%, 97%, 80% or 30%
pass rate), the deadline, slow and lost pods, lost status writes, new templates, `replicas` and `failureLimit` edits.
After every decision it checks capacity, that a rollback never touches the stable's template, that a held rollout
never grows the canary or promotes the failed revision, and that a step only passes after a check and within the
limit; every walk must end `Healthy` or held. A census makes sure each case happens (about 1,300 rollbacks by
analysis, 4,100 by the deadline, 2,800 tolerated failures). Its first run found a capacity drop ("What went wrong on
day 3").

## Testing

**What is envtest, and what doesn't it give you?**
A real kube-apiserver and etcd (downloaded by `setup-envtest` into `bin/k8s`) with the CRD installed, so validation,
defaulting, pruning, RBAC and status subresources are the real ones. It runs none of Kubernetes' own controllers: a
Deployment never gets ReplicaSets or pods, and a deleted namespace never finishes terminating (so each test gets a
fresh namespace, `testenv.Namespace`). The rollout tests play the Deployment controller's part: `markReady` writes
the status a Deployment would have once its pods are ready. Day 4 adds kind for a real cluster.

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

**How do you test the analysis without a Prometheus?**
Three layers. `Evaluate` and `Run` are unit-tested with fake queriers (every verdict, parallel queries, timeouts).
The HTTP client is tested against `httptest` servers serving responses in the API's documented format: a scalar, a
vector, an empty one, a matrix, NaN and ±Inf, and each kind of error (`prometheus_test.go`). The envtest tests run the
operator with the real client against a fake API server that answers every query with one value
(`newFakePrometheus` in `internal/controller/analysis_test.go`), so the whole path, from the reconcile to the HTTP
request and back to the status, runs. No real Prometheus runs here (none is installed); the responses follow its
documented format, which a real server in CI would confirm.

## End to end (day 4)

**What do the end-to-end tests show that envtest couldn't?** envtest is an API server and etcd with no controllers,
so a Deployment never gets pods, and days 2 and 3 wrote Deployment status by hand. On kind a real Deployment
controller starts real pods, readiness comes from real probes, a real Prometheus scrapes real counters, and the
operator runs from its image with the generated permissions (`config/default`). Three rollouts: one promoted, one
rolled back by its analysis, one rolled back by the progress deadline, and one promoted across a leader crash
(`test/e2e/e2e_test.go`; DESIGN "End to end").

**How does the bad version fail its analysis?** The demo app with `ERROR_RATIO=0.5` fails every second request. The
metric is the canary's rate of 500s over the rate of all its requests, over 20 seconds, at most 0.05; Prometheus
measures about 0.5, so each check fails, and the third failure (the limit is 2) rolls back. The test also requires at
least one failed check to have *measured* a value above the max, so it can't pass because a query found nothing.

**Why does the demo app export its 500 counter at 0 from the start?** Without the series, the numerator is an empty
vector, the quotient is empty, and an empty answer fails the check: a version that never failed a request would be
rolled back. Real applications need the same care (initialise the label values you divide by).

**Why does the load generator open a new connection per request?** kube-proxy balances connections, not requests.
With keep-alive a client stays on the pod it first reached, and the replica ratio says nothing about the share of
requests (README "Limitations"). One connection per request makes the split follow the pod count, and the test
measures what share the canary actually got from the pods' own counters.

**Why tolerate two failed checks in the good rollout?** The first check comes 5 seconds into a pause, possibly before
Prometheus has scraped the new pod twice, and a rate needs two samples: the answer is empty and the check fails closed,
as it should. `failureLimit` is the knob for exactly this; a real rollout with 30-second intervals has the same issue
at its first check.

**How do you check capacity on a real cluster?** A goroutine reads both Deployments every 100 ms and keeps the fewest
ready pods it saw; each test fails if that went below `replicas` after the first version was up. It is a sample, not
a proof (a dip shorter than 100 ms could be missed); the random walks are the proof-like part, this checks them
against a real Deployment controller.

**How do the tests reach Prometheus?** Its Service is a NodePort (30090), and `test/e2e/kind.yaml` maps that port of
the kind node to the runner's `127.0.0.1:9090`.

**Why test the Prometheus client against a real Prometheus?** The unit tests' answers were written from the API's
documentation. A real one confirms the encodings (`"NaN"`, `"+Inf"`, `"-Inf"` as strings), that an instant query of a
range is a matrix, that a parse error is `bad_data`, and where Prometheus 3 puts info annotations and warnings (the
client ignores the first and fails on the second).

**Why build the binaries outside Docker?** The Go build is cached on the runner and takes seconds; the image only
copies a static binary onto distroless (pinned by digest). A multi-stage build would download a Go image and every
module inside Docker on each run.

**Why does the e2e run two operator replicas?** That is how `config/default` installs it, so leader election runs with
the Role in its own namespace, as in a real install.

**What happens when the leader crashes mid-rollout?** `TestARolloutSurvivesTheLeaderCrashing` deletes the leader's
pod with no grace period early in a step's pause. Nothing reconciles until the Lease expires (15 seconds after the
last renewal, controller-runtime's default); then the other replica takes it, reads the status (phase, step, the
pause's start, the checks) and the two Deployments, and carries on: the pause keeps the start the old leader wrote,
a check that came due meanwhile runs at once, and the rollout is promoted with one `RolloutStarted` and one `Promoted`
Event. That works because the operator keeps no state of its own; everything is in the status and the Deployments.
The pods keep serving throughout: a dead operator stops decisions, not traffic.

**What did the e2e find that envtest couldn't?** The ResourceQuota limitation. A rolled-back canary keeps its pods
until the stable is back, so capacity never drops; with a pod quota that fits every forward step (`replicas + 2`), a
rollback from the 50% step needs two stable pods back while the canary still holds two, the quota refuses one, and the
rollback never finishes: 30 s later the stable had 3 of 4 pods and the rolled-back version still served 36.4% of the
requests. The test asserts that hold and fails if it ever ends, so whoever fixes it updates the docs. The fix is a
trade: shrink the canary a pod at a time when the stable reports a quota failure, one ready pod below `replicas`
meanwhile. I'd make it the default, since serving a version that failed its analysis is worse than one pod short.

**What isn't covered?** Several nodes, a leader that hangs instead of crashing (it keeps the Lease until its renewals
fail, 10 seconds by default), and traffic through a mesh or Gateway API.

## The review (day 1)

**What did the review find?**
One agent, at `2694208`, 11 minutes, in its own worktree, findings as failing tests. Reproduced:

- A template with no pod spec was accepted (`containers` was only required once `spec` existed): fixed with a CEL
  rule, tested.
- **A probe port over int32 is accepted, and then the operator can't decode the object**, and since a list decodes
  as one document, one such rollout in any namespace stopped the operator from reconciling every other one. Fixed on
  day 2: rollouts are read and decoded one at a time ("Why read each rollout from the API server", above).
- `--leader-elect` couldn't work with the generated role (I had found and fixed it meanwhile, `f89ec6e`).
- Template label syntax isn't checked; the CEL rule for it is over budget (DESIGN.md, "What the API server does not
  catch").
- `weight: 10.0` from a raw-JSON client fails a rule with an evaluation error.
- Equal quantities in different formats give different revisions (now documented and tested as the behaviour).

It also listed six mechanisms the tests didn't notice removed; the four in the CRD (minItems on both lists, the
metric name's maxLength, the label rule's guards) now have tests, each re-run as a mutant. The three in the
reconciler (the optimistic lock, the no-change early return, the conflict branch) got tests on day 2, each run as a
mutant.

## What went wrong on day 1

- The `resource.Quantity` thresholds and the CEL cost budget (above): found the first time envtest installed the CRD.
- Five expected refusal messages in the first test run were my guesses of the API server's wording ("should be
  greater than or equal to 1" is really "spec.replicas in body should be greater than or equal to 1"); the rules were
  right, the expectations weren't.
- A test run that "passed" in 0.4 s had skipped everything: the shell's `export KUBEBUILDER_ASSETS` doesn't carry
  over between tool calls. That's the case `CANARY_REQUIRE_ENVTEST` exists for.
- `--leader-elect` was a flag with nothing behind it (above).
- The public GitHub repository couldn't be created from the scheduled session, so CI hasn't run.

## The review (day 2)

**What did the day 2 review find?**
One agent at `3334dc0`, 25 minutes, findings as failing tests (`review_d2_*`). Ten defects, two already fixed by
then. The important ones were all in moves my random walk never made: editing the steps or replicas during a pause
applied the new split to both Deployments at once (7 of 10 pods ready); cutting the steps kept the old step's start
time, so the rollout promoted immediately; a lost status write while the template changed and changed back left a
pause running for a canary on the other revision; a deleted stable took the canary to 0 at once. Also: pauses ended
up to a second early, a rollout being deleted got its Deployments recreated, and the role wasn't enough on clusters
with `OwnerReferencesPermissionEnforcement`.

**What was the fix for the pause problems?**
One rule: a pause belongs to the pods it started with. If the canary Deployment isn't set to the step's count and
the template's revision any more, the step waits for its pods again (under the capacity rules) and the pause starts
over. That covers edited steps, edited replicas, cut steps and the lost write, rather than four special cases. A pod
lost mid-pause doesn't restart it: the Deployment is still *set* to the step's count, it just isn't all ready.

**Why did the random walk miss them?**
Because it never made those moves: new templates only from `Healthy`, no edits, no deletions, no lost writes. The
review said exactly that. `TestRandomWalksWithEdits` makes each of those moves thousands of times (and counts them,
so a generator change that stops making one fails the test), and fails with each fix removed. The lesson is the same
as Quarry's: ask which mutants a generator would never distinguish.

**And the second review round?**
It looked only at the fixes, and found a regression in mine: my first "pause belongs to its pods" rule cleared a
pause whenever the canary's count differed from the step's, which is also what a lost status write at a step advance
looks like (the canary already at the next step's count), so the step ran again. The rule now distinguishes a pause
still running (its pods changed: start over) from one already over (the step passed). It also found the stable
being scaled down mid-pause while a canary pod was down, and showed my walk couldn't see any of it: its Deployments
never lost a ready pod, and its pause check ran after the plan was applied, so it could never fail. This is the
fourth project in a row where round two found a bug in round one's fix.

**And the third?**
Two more regressions, in round two's fixes. Keeping a finished pause "whatever the canary's count" let a steps edit
skip a raised weight, because the edit itself can make a running pause finished. And I had guessed "lost promotion"
from the step index, which was wrong both ways. The fixes: a finished pause is kept only when the canary has *more*
pods than the step's (weights strictly increase, so that's exactly a lost advance), and a promotion now marks the
stable with an annotation in the same write as its template, so the next decision knows rather than guesses. Three
rounds, and each of the last two found bugs in the previous round's fixes.

**Why an annotation rather than more status?**
The status write is the one that can be lost; the mark has to travel with the Deployment write it describes. The
operator writes the Deployments first, so whatever it needs to recognise later about a Deployment write belongs on
that Deployment. The same idea would fix the one known case below (the step recorded on the canary Deployment).

**Is there anything the state machine still gets wrong?**
One known case: a step with a 0s pause, where one decision sees the pods ready and moves to the next step, and that
decision's status write is lost. The next decision doesn't know the step passed, so it runs it again: the canary
goes back one step once the stable is settled, then forward. Capacity holds and exposure only shrinks. The fix would
be to record the step on the canary Deployment itself, written in the same update as its replicas; it's in the
README's limitations.

**Why does the operator need `update` on `canaryrollouts/finalizers`?**
Its Deployments carry a controller reference with `blockOwnerDeletion: true` (`SetControllerReference` sets it), so a
foreground delete of the rollout waits for them. The `OwnerReferencesPermissionEnforcement` admission plugin
(on in OpenShift, off in kind) only lets a user set that if they may update the owner's finalizers. The review's
test starts a second API server with the plugin; with the old role no Deployment was ever created.

**Why round pause starts up, not down?**
The API server stores status times to the second. Written as 10:00:00.9 and read back as 10:00:00, a 2-minute pause
ended at 10:02:00, 0.9 s early. Rounding down would do the same; rounding up to 10:00:01 makes it run 2m0.1s, never
less than asked.

## The review (day 3)

**What did the review find?**
No capacity defect, and a 6,000-walk stress test of its own that passed. One medium defect: day 3 stops a deleted
stable from being recreated from a rolled-back template, but only once `RolledBack` is in the status; with the
rollback's status write lost, or the deadline passing while the operator was down, deleting the stable put the failed
version at all replicas. Now, if the status alone says the rollout failed (deadline past with pods not ready, or too
many failures) and the stable is gone, the rollback is recorded and nothing is created (`alreadyFailed` in
`decide.go`). Three low ones: checks 100 ms apart (fixed with `MicroTime`), a Prometheus URL ending in `?` accepted and
then failing every query, and a partial answer (`success` with warnings) passing. And five mechanisms no test caught,
each with a test now.

**And the second round?**
It reviewed my fixes only, and found two problems in them. Switching `lastCheckAt` to a `MicroTime` broke decoding
of the times the operator wrote before (to the second): a rollout mid-pause would have become undecodable after an
upgrade, stuck as `InvalidSpec`. The reconciler now rewrites that field to microseconds before decoding
(`repairCheckTime`). And my deleted-stable fix only covered failures the status shows; a rollback by a failed check
whose write was lost shows only as a check due again, so with the stable gone a due check now runs first. Lesson: a
change of a stored field's type is a migration, even on an unreleased field, if anything already wrote it.

**And the third?**
It reviewed round 2's two fixes and found four defects in them: a check run with the stable gone was dropped unless it
rolled back; it wasn't counted when it did; the repair converted times to UTC, which can push one past the year 9999
(the API server then refuses the rewrite, and every reconcile failed); and it dropped times written with a lower-case
`t` or `z`. Each round so far found something in the round before it, which is why each fix gets its own test and its
own mutant run, and why the README says round 3's fixes are unreviewed.

**Why didn't your own walk find the deleted-stable case?**
It never deleted a Deployment: I wrote it for analysis and the deadline, and left deletions to day 2's walk, which has
no analysis. The review's walk combines them, and it's in the repository now (`review_d3_walk_test.go`).

## The review (day 4)

**What did the fourth review find?** It reviewed round 3's fixes, which nobody had. Nothing on the normal path; two
cases where the branch for a deleted stable decided differently from the normal path: a lowered `failureLimit` made
it run and count a check the normal path skips, and a pause the normal path would restart (after a `replicas` edit)
was still checked and could roll back. Both now go through the normal path's own rule (`pauseCurrent` in
`internal/rollout/decide.go`), and `alreadyFailed` is asked before any check. It also found three mutants of the
stored-time repair that no test killed; each has a test now.

**Why do the reviews keep finding bugs in the fixes?** Each fix adds a branch that must agree with the rest of the
state machine on rare inputs (a deleted stable, an edit, a lost write). The rounds narrowed: round 1 found a medium
issue, rounds 2 and 3 bugs in the fixes, round 4 two low-severity disagreements. The lesson I'd state: a special case
should reuse the normal path's predicates (`pauseCurrent`) rather than restate them, since restating is where they
drifted.

## What went wrong on day 4

- My first e2e run: the promotion test read `status.analysis` after the promotion, which clears it (nil), and
  panicked; and the Prometheus contract test waited for `workqueue_depth`, which controller-runtime v0.25's priority
  queue exports only once something has been queued (none had: the contract test runs before any rollout). It now
  uses Prometheus's own `up` series. Both fixed in `6302a1f`; the rollbacks and the promotion itself worked on the
  first run.


- My first check-time rounding (up) made a 1s interval check every 2s; my second (down) let two checks run 100 ms
  apart. Both came from forcing check times into seconds; a `MicroTime` was the fix.
- The hold after a rollback overwrote the message saying why (an envtest test caught it).
- The analysis walk's first run found a capacity drop in a step held for its first check (fixed, with a regression
  test).
- I compared the failure limit only after a failed check, so lowering it mid-step didn't count; and a shutdown
  mid-check counted as a failed check. Both found while reading the code, both fixed with tests.
- A deleted stable after a lost rollback write (the review's medium finding): my walks never combined deletions with
  analysis. My fix then covered only the deadline, and changing `lastCheckAt`'s type broke decoding of times already
  stored (the second round's findings).

## What went wrong on day 2

- The first `Decide` could drop capacity after a revert followed by a new template; the random walks found it
  ("How do you know capacity never drops?").
- A paused step never ended on its own: the reconcile that found nothing to write dropped the requeue ("How is a
  pause timed?").
- Two of my test expectations in `decide_test.go` were wrong, not the code: with a 0s pause a step passes straight
  through, so the stable never goes down to that step's count.
- My first restart test passed with its mechanism removed. Status times are stored to the second and the test took
  under one, so a start time reset by the new operator looked identical. Worse, I had chained the commit after the
  mutant run, so it went in (`cdd6226`) with a message saying the mutant fails; `b4ed977` fixes the test (a fixed
  clock per operator) and its message says the earlier one was wrong.
