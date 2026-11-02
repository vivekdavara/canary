# Canary

A Kubernetes operator for canary rollouts. A `CanaryRollout` holds a pod template; when the template changes, the
operator runs the new version beside the old one in steps (10% of the pods, then 50%, ...), checks Prometheus metrics
against thresholds at each step, and either promotes the new version or rolls it back.

Status: **day 3 of 5**: the operator runs rollouts and decides them. On a template change it starts the new version
in a canary Deployment beside the stable one, moves pods to it step by step (each step's pause running once its pods
are ready), checks Prometheus metrics during each pause, and promotes the new version after the last step or rolls it
back (a failed analysis, or a step whose pods aren't ready by the progress deadline), without ever dropping below
`replicas` ready pods. A run on a real cluster (kind, in CI) is day 4's. [DESIGN.md](DESIGN.md) is the specification;
[WALKTHROUGH.md](WALKTHROUGH.md) has the questions an interviewer is likely to ask.

Go 1.27, controller-runtime v0.25, Kubernetes 1.37 (envtest locally, kind in CI from day 4). MIT licensed.

## Why

Deploying a new version to every pod at once makes every user a tester. A canary sends a small share of traffic to
the new version first and checks it on real traffic (error rate, latency) before going further, and an operator
makes that a property of the cluster instead of a script someone has to run: the steps, thresholds and the decision
to promote or roll back live in one object, and the operator keeps acting on it through restarts.

## How it works

```mermaid
flowchart LR
  user["kubectl apply<br/>CanaryRollout"] --> api[(API server)]
  api -- watch --> op["canary operator"]
  op -- "create / scale" --> stable["Deployment web-stable"]
  op -- "create / scale" --> canary["Deployment web-canary"]
  op -- "status, Events" --> api
  op -- "instant queries" --> prom[(Prometheus)]
  svc["Service web"] --> stable
  svc --> canary
```

The operator owns two Deployments per rollout, and the application's own Service selects the pods of both, so the
canary's share of the traffic is its share of the pods. Each step scales the canary up first, waits for its pods to
be ready, then scales the stable down and holds for its pause while the analysis checks the metrics every `interval`;
after the last step the stable takes the new version and the canary keeps its pods until the stable's are ready. A
check that can't measure fails (an empty answer, NaN, an error, a timeout), and more failed checks than
`failureLimit`, or a step whose pods aren't ready within `progressDeadlineSeconds`, roll back: the stable goes back to
all its pods with the old version, the canary keeps its pods until those are ready, and the rollout holds until the
template changes.

The decisions are one pure function, `rollout.Decide`: from the spec, the last status and the two Deployments it
returns what each Deployment should become and the next status. The reconciler reads, calls it, writes the
Deployments, then the status, then the Events. DESIGN.md has the API, the state machine, the pod split, the order of
writes and what each failure leaves.

```yaml
apiVersion: rollouts.vivekdavara.github.io/v1alpha1
kind: CanaryRollout
metadata:
  name: web
spec:
  replicas: 10
  template: {...}            # a pod template, as in a Deployment
  steps:
    - weight: 10
      pause: 2m
    - weight: 50
      pause: 5m
  analysis:
    interval: 30s
    failureLimit: 1
    metrics:
      - name: error-rate
        query: sum(rate(http_requests_total{app="web",track="canary",code=~"5.."}[1m])) / sum(rate(http_requests_total{app="web",track="canary"}[1m]))
        max: 0.01
```

The full example is [config/samples/web.yaml](config/samples/web.yaml) (a test applies it).

## Layout

| Path | What |
|---|---|
| `api/v1alpha1/` | the `CanaryRollout` types, their validation markers and CEL rules; `zz_generated.deepcopy.go` is generated |
| `config/crd/bases/` | the CRD, generated from the types (`make manifests`) |
| `config/rbac/role.yaml` | the operator's ClusterRole, generated from the reconciler's rbac markers |
| `config/rbac/leader_election_role.yaml` | the Role for `--leader-elect`, in the Lease's namespace (written by hand) |
| `config/samples/` | example rollouts |
| `internal/revision/` | a template's revision: 16 hex digits of the SHA-256 of its JSON |
| `internal/rollout/` | the pod split and the state machine (`Decide`: steps, pauses, checks, the deadline, rollback), pure functions |
| `internal/analysis/` | a metric's verdict (fails closed), a check of all metrics in parallel, the Prometheus instant-query client |
| `internal/controller/` | the reconciler: reads, `Decide`, writes the Deployments, the status and the Events |
| `internal/testenv/` | starts envtest (a real kube-apiserver and etcd) for a package's tests |
| `cmd/canary/` | the operator's binary |

## Run it

```bash
make check
```

That installs the pinned tools into `bin/` on first use (controller-gen v0.21.0, setup-envtest v0.25.2, and envtest's
kube-apiserver and etcd for Kubernetes 1.37.0), regenerates the deep copies, CRD and ClusterRole, and runs gofmt, go
vet and every test. A plain `go test ./...` skips the envtest suites; `make test` makes them fail instead if their
binaries are missing.

Against a cluster (kind, minikube, or any cluster your kubeconfig points at):

```bash
kubectl apply --server-side -f config/crd/bases
```

```bash
go run ./cmd/canary --kubeconfig ~/.kube/config --prometheus-url http://localhost:9090
```

```bash
kubectl apply -f config/samples/web.yaml && kubectl get canaryrollouts
```

The CRD needs `--server-side`: with the pod template's schema and descriptions it is 636,521 bytes, and a client-side
apply would store all of it in an annotation limited to 256 KiB (DESIGN.md, "Why these choices"). The sample
creates `web-stable` at 10 pods; change its image and `kubectl get crl -w` shows the phase, the step and the canary's
actual share as the pods become ready (`kubectl get deploy -l rollouts.vivekdavara.github.io/rollout=web` shows the
two Deployments, `kubectl events --for canaryrollout/web` the Events). The operator serves metrics on `:8111` and `/healthz` and `/readyz` on `:8121`. With
`--leader-elect` (for more than one replica) it also needs `--leader-election-namespace` outside a cluster, and
`config/rbac/leader_election_role.yaml` bound in that namespace (the Lease and its Events). Without
`--prometheus-url`, a rollout with analysis fails its first check and rolls back (it says so in its `AnalysisPassing`
condition); the sample's queries assume the application exports `http_requests_total` and a latency histogram, and
that Prometheus keeps the operator's `track` pod label as `track`. `--analysis-timeout` (10s) bounds each query.

## Tests

All against envtest's real API server unless marked unit. envtest runs no Deployment controller, so the rollout tests
play its part: they write a Deployment's status (`markReady`) when its pods would be ready.

| File | What it checks |
|---|---|
| `api/v1alpha1/validation_test.go` | each CRD rule refuses with its message, each boundary value is accepted, raw input the typed struct can't express (`pause: 1d`, a quoted threshold, missing required fields), the defaults, template labels surviving pruning, defaults filled inside the pod template, status as a subresource, the samples |
| `internal/revision/revision_test.go` (unit) | a pinned revision (checked against `shasum` of the JSON), a valid label value, every kind of change moves it, map order and quantity spelling don't |
| `internal/rollout/split_test.go` (unit) | the split against DESIGN's rules for all 100,000 allowed inputs (replicas 1-1000, weight 1-100), and worked examples |
| `internal/rollout/review_d2_*_test.go` (unit) | the day 2 review's state-machine findings, as it wrote them |
| `internal/controller/review_d2_*_test.go` | the day 2 review's operator findings: a pause's full length (fixed clocks), a rollout being deleted, the role under `OwnerReferencesPermissionEnforcement` (a second API server with that plugin) |
| `internal/rollout/decide_test.go` (unit) | whole rollouts through a hand-simulated Deployment controller: first apply, steps, pauses timed from readiness, promotion, a restart mid-rollout, a revert, a template change during promotion, a deleted stable, steps shrinking, one pod, a promotion whose status write was lost; and 3,000 seeded random walks (pods settling one Deployment at a time, time passing, new templates, reverts) checking after every decision that ready pods never drop below `replicas`, that the status agrees with itself, and that every walk ends `Healthy` at the template; and 5,000 walks that also edit steps and replicas, change the template mid-rollout, delete either Deployment and lose status writes (each move counted, at least 1,000 times), checking that no decision takes ready pods below `min(replicas, what was ready)` |
| `internal/controller/rollout_test.go` | a whole rollout against the API server as the operator's own user: the stable's labels, selector, owner and strategy, the stable kept at 4 until the canary's pod is ready, the canary kept during the promotion, the four Events; a 2 s pause ending on its own requeue; a template the API server refuses as a Deployment, then fixed; someone else's Deployment with the rollout's name, reported, and the conflict clearing itself when it's deleted; Deployments deleted by hand coming back |
| `internal/analysis/analysis_test.go` (unit) | every verdict: inside, on and outside the bounds, no samples, two, NaN, ±Inf, a matrix, a string, an error; the messages (failed metrics first, an error cut at a rune); a check's queries run in parallel (three 50 ms queries in under 140 ms), reported in the metrics' order, each bounded by the timeout; no Prometheus fails them all |
| `internal/analysis/prometheus_test.go` (unit) | the client against canned API responses: a scalar, one sample, two, none, a matrix, warnings, NaN and ±Inf; each error (`bad_data`, `timeout`, a proxy's HTML page, an empty envelope, a success with a 500, a histogram sample, a bad value, a value that isn't a string, a short sample, an answer over 1 MiB); a hung server and a closed one; the URL's validation |
| `internal/rollout/rollback_test.go` (unit) | the deadline (rolls back on its exact second; per step), a rollback with the stable down (the canary kept until the stable is back), checks every interval (four in a 2m pause at 30s), a pause shorter than the interval, the failure limit (failures tolerated, then the rollback, each a Warning), checks starting over with the pause, a deleted stable after a rollback (not recreated from the failed template), template changes after a rollback, a lost rollback write, a due check holding the step, a 1s interval with late reconciles, the hold keeping the rollback's message, a lowered limit, the walk's capacity case |
| `internal/rollout/walk_analysis_test.go` (unit) | 4,000 random walks with checks passing or failing at random, the deadline, slow and lost pods, lost status writes, new templates, `replicas` and `failureLimit` edits; after every decision: capacity, a rollback never touching the stable's template, a held rollout never growing the canary or promoting the rolled-back revision, a step passing only after a check and within the limit, the status consistent; every walk ends `Healthy` or held after its rollback; a census makes sure each case happens at least 300 times |
| `internal/rollout/review_d3_*_test.go`, `internal/analysis/review_d3_prometheus_test.go` (unit); `review_d3b_*_test.go` and `review_d3c_*_test.go` (the second and third rounds', some envtest) | the day 3 reviews' findings, as they wrote them, and the first round's walk (`TestReviewD3Walks`, 6,000 walks with deletions, edits of every analysis field, analysis removed and added, and status times stored to the second) |
| `internal/controller/analysis_test.go` | through the real HTTP client and a fake Prometheus API: a passing analysis promotes (3 checks in a 3 s pause at 1 s), a failing one rolls back (Warning Events, both conditions, the stable back, the canary drained, no more queries), the progress deadline rolls back on its requeue, no Prometheus fails closed, a hung Prometheus fails at the timeout; a rolled-back rollout's writes counted, and none in the hold |
| `internal/controller/undecodable_test.go` | a rollout with a probe port beyond int32 (accepted, undecodable) gets `InvalidSpec` on its own status while another rollout keeps being reconciled |
| `internal/controller/rollout_test.go` (`TestARolloutsWrites`) | a whole rollout's writes counted at the operator's own client, and none in the two seconds after it ends: a guard against a reconcile that writes in a loop (a status carrying the time wrote 984 patches in those two seconds) |
| `internal/controller/reconcile_test.go` | `Reconcile` called by hand through an interceptor client: a status computed from a spec that changed before the patch is refused (the optimistic lock); a reconcile with nothing new writes nothing; a second operator (fixed clocks, ten minutes apart) keeps a paused step's start time |
| `internal/controller/canaryrollout_controller_test.go` | status follows the spec through template and replica changes; fields the reconciler doesn't own are kept; a missing rollout is no error |
| `cmd/canary/main_test.go` | the binary from its flags and a kubeconfig: probes answer, a rollout is reconciled, the reconcile is counted in its metrics, a clean stop; with `--leader-elect` it takes the Lease using only the leader-election Role; `--help`; bad `--prometheus-url` and `--analysis-timeout` values refused with the flag named |
| `config/install_test.go` | the README's install steps with envtest's kubectl: client-side apply of the CRD fails ("Too long"), `--server-side` works, the sample applies, `kubectl get crl` shows the columns |

Checks that the tests can fail, each run as a mutant. Day 3: letting a step held for its first check scale an
oversized canary down fails `TestRandomWalksWithAnalysis` and `TestAStepHeldForItsCheckKeepsTheCanaryWhileTheStableIsShort`;
comparing the failure limit only after a failed check fails the walk and `TestALoweredFailureLimitRollsBack`. Day 2:
removing any one of the three capacity rules in `Decide` fails
`TestRandomWalks`; the old typed watch fails `TestOneUndecodableRolloutDoesNotStopTheOthers`; a patch without the
optimistic lock fails `TestAStatusFromAnOlderSpecIsNotWritten`; dropping either "nothing changed" early return fails
`TestAReconcileWithNothingNewWritesNothing`; not passing `stepStartedAt` to `Decide` fails
`TestANewOperatorCarriesOn` (it didn't at first; see "What went wrong"); turning off the watch of same-named
Deployments fails `TestSomeoneElsesDeploymentWithTheName`. From day 1: removing the Deployments rule
from `config/rbac/role.yaml` fails `TestStatusFollowsTheSpec`; removing the Lease rule fails `TestRunElectsALeader`;
misspelling `failureLimit` in the sample fails `TestSamplesAreAccepted`.

## Results (day 3)

Still no cluster (day 4 runs kind in CI) and no real Prometheus here; the analysis runs against a fake Prometheus API
in envtest. Measured on an Apple M3, on AC, with nothing else running, at `b6b6df8`:

- `make check`: 53.79 s real (`/usr/bin/time -p make check`): 125 top-level tests (`go test -list '.*' ./... | grep -c
  ^Test`; 70 at day 2's end), all seven packages `ok`, none skipped. The controller package alone takes about 50 s:
  the analysis tests run real 1 s check intervals and a 2 s deadline.
- **What a rolled-back rollout costs**: 2 creates, 3 Deployment updates, 11 status patches and 3 Prometheus queries
  (first apply, one 50% step, three failed checks a second apart with two allowed, the rollback, the drain), the same
  in three runs at `036aff0` and again at `b6b6df8`; then two quiet seconds with no write and no query
  (`go test -run 'TestARolloutsWrites|TestARolledBackRolloutsWrites' -v ./internal/controller/` with
  `KUBEBUILDER_ASSETS` set). Day 2's rollout with no analysis still costs 2 creates, 3 updates and 9 patches.
- `TestRandomWalksWithAnalysis`: 4,000 walks, 0.32 s; census at `b6b6df8` (`-v`): 1,353 rollbacks by the analysis,
  4,114 by the deadline, 41 by a lowered failure limit, 2,817 failed checks tolerated, 17,663 steps passed after
  checks, 7,491 promotions, 49,950 lost status writes (2,307 of them in a decision that ran a check), 70,049 decisions
  during a hold; 3,307 walks ended `Healthy` and 693 held after a rollback.
- `TestReviewD3Walks` (the review's): 6,000 walks, 0.50 s; 120,063 lost status writes, 4,543 stable and 4,460 canary
  deletions, 7,209 rollbacks, 6,151 templates set back to a rolled-back revision, 95 walks ending held with the
  stable deleted.
- Day 2's walks with no deadline: `TestRandomWalks` 0.13 s, `TestRandomWalksWithEdits` 0.21 s.
- The CRD is 638,009 bytes (`wc -c config/crd/bases/*`; 636,521 on day 2: the new status fields).

## Results (day 2)

No cluster runs here (no kind locally; day 4 runs one in CI), so there are no rollout timings yet. What was measured
(Apple M3, on battery):

- `make check` warm: 28.61 s real at `fa27f39` on AC with nothing else running (`/usr/bin/time -p make check`): 70
  top-level tests (`go test -list '.*' ./... | grep -c ^Test`), all six packages `ok`, none skipped. It includes a
  second API server started for the `OwnerReferencesPermissionEnforcement` test. Earlier, at `d1cec47` (44 tests, on
  battery, a review agent's tests running at the same time), it was 17.51 s.
- `TestRandomWalks`: 3,000 walks of 60 random actions plus a settling tail, 0.12 s
  (`go test -run TestRandomWalks -v ./internal/rollout/`). `TestRandomWalksWithEdits`: 5,000 walks of 80 moves,
0.22 s; its census (printed with `-v`) at `bdc3c33`: 49,778 lost status writes, 17,265 lost ready pods, about 15,000
step edits and 15,000 replicas changes, 7,630 stable and 3,440 canary deletions, 3,668 template changes mid-rollout,
and 6,014 pause starts checked in rollouts nobody edited.
- A whole rollout of 4 pods (first apply, steps of 25% and 50% with no pauses, promotion) costs the operator **2
  creates, 3 Deployment updates and 9 status patches**, the same in three runs, and no write in the two seconds after
  it ends (`go test -run TestARolloutsWrites -v ./internal/controller/` with `KUBEBUILDER_ASSETS` set, at `951022f`
  and again at `940742f`; the test counts at the operator's own client). The stable never got a separate 4 -> 3
  update: with no pause, step 2 began as soon as step 1's pod was ready. Between `eeb67e3` and `940742f` the same
  rollout cost 5 updates and 11 patches, because the rounded pause start made 0s pauses wait (see the review).
- The CRD is unchanged from day 1: 636,521 bytes with descriptions.

## Review of day 1

One review agent at `2694208` reproduced 6 defects. Fixed on day 1: a template with no pod spec was accepted;
`--leader-elect` had no Lease permission. **Fixed on day 2: one rollout with a probe port over int32 stopped the
operator for every rollout** (rollouts are now read and decoded one at a time). Template label syntax is now checked
by the API server when the operator creates a Deployment, and a refusal is reported on the rollout. Still as
documented: `weight: 10.0` from a raw-JSON client fails with an evaluation error; equal quantities in different
formats get different revisions. The three reconciler mechanisms it listed as untested (the optimistic lock, the
no-change return, the conflict branch) have tests now, each run as a mutant (the conflict branch returning its error
fails `TestAStatusFromAnOlderSpecIsNotWritten` too).

## Review of day 2

One review agent at `3334dc0` (25 minutes, its own worktree, findings as failing tests, kept as `review_d2_*`)
reproduced 10 defects; two were already fixed by then (`d825984`, `4f6aa43`). Fixed:

- **Capacity, in the moves the random walks never made**: editing the steps or `replicas` during a pause applied the
  new split to both Deployments at once (7 of 10 pods ready); cutting the steps kept the old step's start time, so the
  rollout promoted at once and shrank the canary; a deleted stable took the canary to 0 before any new stable pod was
  ready. A pause now belongs to the pods it started with (DESIGN.md, "A step"), and a missing stable keeps the canary.
- **A lost status write while the template changed and changed back** left a pause running for a canary on the
  other revision, which then promoted.
- **Pauses ended up to a second early** (status times are stored to the second): starts are rounded up.
- **A rollout being deleted got its Deployments recreated** during a foreground delete.
- **The role wasn't enough under `OwnerReferencesPermissionEnforcement`** (OpenShift): no Deployment was ever
  created. `update` on `canaryrollouts/finalizers` added; `get` on Deployments (unused) removed.
- The Promoting Event was lost with a lost status write; `currentStep` could leave the steps' range in a promotion.
- Fixed meanwhile: a `DeploymentConflict` never cleared on its own; two tests raced the operator's status writes.

It also listed mechanisms it could remove with every test passing (the readiness rule's parts, Events only after a
status write, `Available` going False, `canaryWeight`, the undecodable report's lock and skip, Warning vs Normal):
each has a test now, run again as a mutant. And it named what the random walk never did (edit steps or replicas,
delete a Deployment, lose a write, change the template mid-rollout): `TestRandomWalksWithEdits` does each thousands
of times, and fails with the pause fix, the deleted-stable fix or the step clamp removed.

A second agent, on the fixes only (`199d92a`, 10 minutes, `review_d2b_*`), found **a regression in my fix**: my
first "pause belongs to its pods" rule re-ran a finished step when a step advance's status write was lost (an hour,
in its test). Also a `replicas` cut while a canary pod was down mid-pause took the stable straight down (8 of 9
ready), a recreated stable with a lost write looked like a lost promotion (a false `Promoting` Event), and the name
watch missed a Deployment controlled by a rollout of another name. All fixed (`bdc3c33`, `d21c9fb`). It also showed
why my new walk missed them: its Deployments never lost a ready pod, its pause check ran after the plan was applied
(so it could never fail), a lost write was a move of its own, and nearly every walked rollout got edited, so nothing
checked untouched ones. Each of those is fixed in the walk, which now fails with either of those two fixes reverted.

A third agent, on round 2's fixes only (`d21c9fb`, 6 minutes, `review_d2c_*`), found **two more regressions in
them**: keeping a finished pause whatever the canary's count let a steps edit skip a raised weight (the 50% share
never seen), and guessing a lost promotion from the step index was wrong both ways (a stable recreated at the last
step looked promoted; a lost promotion followed by an added step was never reported). Fixed in `940742f`: a finished
pause is kept only when the canary has more pods than the step's, and the stable a promotion writes is marked with an
annotation (`rollouts.vivekdavara.github.io/promoted-revision`) in the same update, so a lost promotion is known, not
guessed. While fixing it I found that rounding pause starts up had made a 0s pause wait up to a second (a whole
rollout went from 3 to 5 Deployment updates); a 0s pause is over at once again.

A fourth agent, on `940742f` only (8 minutes), found the promotion mark and the 0s pause sound, and one case where
my new comment was wrong: a lost advance followed by a `replicas` increase leaves the canary with *fewer* pods than
the step now needs, so the step's pause runs again. Low severity (a lost write and an edit together; capacity
holds); documented under limitations with the 0s case, which has the same fix, and its test parked for day 3.

## What went wrong on day 2

- The random walks found a capacity drop in my first `Decide`: a new rollout started while the last one's canary
  was still draining scaled that canary down before the stable was back at full size. The fix holds an oversized
  canary until the stable is settled.
- The 2 s pause test found that a reconcile with nothing to write returned without the pause's requeue, so a paused
  step only ended if something else happened to wake the operator.
- My first restart test passed with the mechanism it tests removed: status times are stored to the second and the
  test took less than one, so a reset start time looked the same. I committed it chained after the mutant run
  anyway (`cdd6226`, whose message says the mutant fails); `b4ed977` gives each operator a fixed clock and says so.

## Review of day 3

One review agent at `b6c6b35` (18 minutes, its own worktree, findings as failing tests, kept as `review_d3_*`) found
no capacity defect. It wrote a stronger walk (`TestReviewD3Walks`: 6,000 walks that also delete either Deployment,
during the hold too, edit the steps, interval, deadline and failure limit, remove and add the analysis, set the
template back to a rolled-back revision, and store status times to the second), which passed. It reproduced:

- **Medium: a failed version could become the stable at all replicas.** Day 3 stops a deleted stable from being
  recreated from a rolled-back template, but only once `RolledBack` was written. With the rollback's status write lost
  (or the deadline passing while the operator was down), deleting the stable recreated it from the failed template,
  phase `Healthy`. Fixed (`a005015`): when the status alone says the rollout failed and the stable is gone, the
  rollback is recorded and nothing created.
- **Low: two checks 100 ms apart.** My rounded-down check times let a reconcile right after a late check run another
  (one bad moment counted twice). Fixed (`a005015`): `lastCheckAt` is a `MicroTime`, and a step with analysis passes on
  a check made at or after its pause's end, so checks stay an interval apart and drift can't skip the last one.
- **Low: `--prometheus-url http://p:9090?` was accepted**, and every query then asked Prometheus's UI. Refused now.
- **Low (arguable): a partial answer passed.** A "success" with warnings (a store down behind Thanos, Cortex or Mimir)
  may leave out the failing series. It fails the query now, except `PromQL info:` annotations.

A second agent, on those fixes (`2aa772f`, 17 minutes, `review_d3b_*`), found **a regression in my fix and a gap in
another**: `lastCheckAt` as a `MicroTime` decodes only with six fractional digits, so a rollout mid-pause written by
the operator before the change would have been undecodable after an upgrade, `InvalidSpec` for good (fixed,
`69325da`: the reconciler rewrites such a time to microseconds before decoding); and the deleted-stable fix covered
the deadline but not a rollback by a failed check whose write was lost, which leaves only a check due again (fixed,
`237ba4f`: with the stable gone mid-pause, a due check runs first and decides). Also an answer marked `isPartial`
(VictoriaMetrics) without warnings passed (fixed), and a day 1 test raced the operator's status writes (fixed,
`d3b4ae2`). Five mutants of my round-1 branch survived; each fails a test now.

A third agent, on round 2's two fixes (`237ba4f`, 21 minutes, `review_d3c_*`), found four defects in them, all fixed
in `b6b6df8`: a check run with the stable gone that passed or failed within the limit was dropped when day 2's rule
recreated the stable (no `AnalysisFailed`, no `AnalysisPassing`); a rollback by such a check counted the failure but
not the check; the repair converted times to UTC, so `9999-12-31T23:00:00-05:00` became the year 10000, which the API
server refuses, and every reconcile failed; and a lower-case `t` or `z` (RFC 3339 allows them) dropped the instant.
Its survivors (the repair's optimistic lock and conflict path, the plan asking for a check with the stable gone) each
have a test now. Round 3's fixes have not been reviewed.

The first agent also listed five mechanisms it could break with every test passing: the wait's start rounded up, `AnalysisFailed`'s
action, the Event note and the `AnalysisPassing` message cut to 1 KiB, and the canary kept in a hold with the stable
gone. Each has a test now and each mutant fails it (`2aa772f`); without the cut, the API server refuses the Event and
it is lost. Its unreproduced suspicion, that a rolled-back canary's unready pods could hold a namespace's quota, is
under limitations.

## What went wrong on day 3

Found by my own tests and reading, before the review:

- **A 1s interval checked every 2s.** I rounded each check's time up to the second, as day 2 rounds pause starts;
  each next check then came up to a second late. The envtest test counted 2 queries in a 3 s pause instead of 3. I
  rounded down instead, which the review then showed wrong the other way (below); check times are microseconds now.
- **The hold after a rollback overwrote why it rolled back.** The `Progressing` message was rewritten on the next
  reconcile with a generic one; the envtest deadline test caught it. The hold keeps the rollback's message now.
- **A capacity drop**, found by the analysis random walk's first run: a step held past its 0s pause for its first
  check, after a `replicas` decrease left the canary above the step's count, scaled the canary down while a stable pod
  was down (10 ready to 9). The hold keeps an oversized canary until the stable is settled, as `waitForCanary` does.
- **A lowered `failureLimit` was ignored** until the next failed check, so a step could pass with more failures than
  the spec allowed. The limit is compared at every decision in a pause now.
- **A shutdown mid-check counted as a failed check**: every query fails with the cancelled context, and with no
  failure allowed that is a rollback. A check cut short that way is no verdict now.

## CI

`.github/workflows/ci.yml`: `make check` on Ubuntu, then a check that the generated files were committed, then the
build.

## Design decisions

- The rollout holds the template and owns both Deployments, rather than pointing at a Deployment the user owns.
- Traffic moves by replica ratio through the application's own Service: no mesh needed, at the price of precision.
- Every rule a user can break is enforced by the API server (schema and CEL), not discovered later by the operator;
  what the schema can't check inside the pod template, the Deployment's own validation does, reported on the rollout.
- The state machine is a pure function; the reconciler only reads and writes. It keeps no state of its own:
  everything is in the status and the Deployments.
- Capacity first: scale up before down, keep the old pods until the new ones are ready, `maxUnavailable: 0`.
- Deployments are written before the status, Events after it.
- Thresholds are `float64`, as Prometheus answers; the operator's Prometheus address is its flag, not the rollout's.
- The analysis fails closed: a check that can't measure (no samples, NaN, an error, a timeout, no Prometheus) fails.
- `Decide` stays pure with I/O in the loop: a decision says a check is due, the reconciler runs it and decides again
  from the same input with the result.
- A rollback keeps capacity like a promotion (the canary drains only once the stable is back), then holds until the
  template changes; there is no rollback once promotion has started (the old template is gone by then).

## Limitations

- No run on a real cluster yet (day 4: kind in CI), and no real Prometheus here: the client is tested against
  responses in the API's documented format.
- A check fails closed, so a Prometheus outage rolls rollouts in progress back once `failureLimit` is used up.
- Reconciles run one at a time and a check runs inside one: a slow Prometheus delays other rollouts by up to
  `--analysis-timeout` per check.
- The progress deadline runs per step from when the step began waiting; editing `replicas` during the wait doesn't
  restart it.
- `AnalysisPassing` shows the last check run, which after a new rollout starts may be the previous rollout's until the
  first check (its message names the step and check number).
- After a rollback, a deleted stable isn't recreated until the template changes (it would run the failed version).
- A rolled-back canary keeps all its pods, ready or not, until the stable is back; with a tight ResourceQuota its
  unready pods could hold the quota the stable needs (not reproduced; DESIGN.md, "Limitations").
- Editing the steps or `replicas` mid-pause restarts the step's pause (its pods change).
- A status write lost at a step advance can make the step run again in two cases: the step's pause was 0s (the one
  decision that passed it is the lost one), or `replicas` went up before the next reconcile (the canary then has
  fewer pods than the step now needs, which looks like an edit). Capacity holds; it costs a pod, or a second pause.
  Recording the step on the canary Deployment, written with its replicas, would fix both (not done).
- A restart from a later step, or a new rollout right after a revert, keeps the old canary's pods (old version) until
  the stable is back at its count: capacity over speed.
- A hand edit to a Deployment's pod template isn't undone (templates are compared by their revision label).
- If the stable Deployment is deleted, the version it ran is gone: the operator recreates it from the template.
- One GET per reconcile for the rollout itself (the price of decoding rollouts one at a time).
- Replica ratio is a coarse split (connections, not requests; one pod of three is 33%).
- `replicas` is at least 1; no HPA integration, no manual pause, approval or abort.
- A `k8s.io/api` upgrade that changes the template's encoding would change every revision; a pinned test catches it.
