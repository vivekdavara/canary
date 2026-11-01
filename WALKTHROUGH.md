# Walkthrough

Questions an interviewer is likely to ask about Canary, with short answers grounded in the code. Day 1 covered the
design, the API and its validation, revisions and the test setup; day 2 the rollout itself (the split, the state
machine, the reconciler's reads and writes); the analysis and rollback are day 3's.

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
