# Canary

A Kubernetes operator for canary rollouts. A `CanaryRollout` holds a pod template; when the template changes, the
operator runs the new version beside the old one in steps (10% of the pods, then 50%, ...), checks Prometheus metrics
against thresholds at each step, and either promotes the new version or rolls it back.

Status: **v1.0.0** (2026-11-04; [CHANGELOG.md](CHANGELOG.md)). On a template change the operator starts the new
version in a canary Deployment beside the stable one, moves pods to it step by step (each step's pause running once
its pods are ready), checks Prometheus metrics during each pause, and promotes the new version after the last step or
rolls it back (a failed analysis, or a step whose pods aren't ready by the progress deadline), without ever dropping
below `replicas` ready pods. It installs with a Helm chart or a kustomization. CI runs it on kind from its image,
installed by the chart, with a real Deployment controller and a real Prometheus measuring a demo app's real errors:
one version promoted, one rolled back by its analysis, one by the deadline, one promoted across the leader being
killed ("Results at v1.0.0").

Reading order: this README; [DESIGN.md](DESIGN.md), the specification (the API, the state machine, analysis,
rollback, end to end, installing); [WALKTHROUGH.md](WALKTHROUGH.md), the questions an interviewer is likely to ask,
answered from the code; then `internal/rollout/decide.go` (every decision) and `internal/controller/` (the reconcile).

Go 1.27, controller-runtime v0.25, Kubernetes 1.37 (envtest locally, kind v0.33 in CI), Prometheus 3.15 in the e2e,
Helm 4.3. MIT licensed.

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
| `config/rbac/service_account.yaml`, `bindings.yaml` | the operator's service account in `canary-system`, bound to both roles |
| `config/manager/` | the operator's Deployment (two replicas, leader election, non-root, read-only root filesystem) |
| `config/default/` | the kustomization installing all of it: `kubectl apply --server-side -k config/default` |
| `charts/canary/` | the Helm chart installing the same objects; `crds/` and `files/role.yaml` are copies of the generated files (`make manifests`) |
| `charts/chart_test.go` | the chart's tests: rendered against `config/default`, the values it refuses, `helm install`/upgrade/uninstall on envtest |
| `charts/review_d5_*_test.go` | the day 5 review's findings, as it wrote them (two assertions adapted to the fixes: the chart refuses a selector label in `podLabels`; the CRD upgrade test runs the corrected command and an install after it), and the schema checked against the real binary both ways: the values it refuses exit the operator, ten it accepts start it |
| `charts/review_d5b_values_test.go`, `charts/schema_property_test.go` | the second review round's findings (an upgrade switching leader election off, pull-secret names, IPv6 literals, sub-nanosecond timeouts, a URL ending in `:`), and the schema's own patterns, compiled in Go, against the operator's checks: 200,000 random URLs and 200,000 random timeouts, every accepted one accepted by `analysis.NewPrometheus` and `time.ParseDuration` (> 0), samples cross-checked through helm |
| `config/samples/` | example rollouts |
| `internal/revision/` | a template's revision: 16 hex digits of the SHA-256 of its JSON |
| `internal/rollout/` | the pod split and the state machine (`Decide`: steps, pauses, checks, the deadline, rollback), pure functions |
| `internal/analysis/` | a metric's verdict (fails closed), a check of all metrics in parallel, the Prometheus instant-query client |
| `internal/controller/` | the reconciler: reads, `Decide`, writes the Deployments, the status and the Events |
| `internal/testenv/` | starts envtest (a real kube-apiserver and etcd) for a package's tests |
| `cmd/canary/` | the operator's binary |
| `Dockerfile` | an image of one of the binaries (the operator, or the demo app), on distroless static |
| `test/e2e/` | the end-to-end tests on kind (build tag `e2e`), the kind config, the overlay adding a Prometheus, the CI diagnostics |
| `test/e2e/demoapp/` | the application the e2e tests roll out: fails a configured share of requests, counts them for Prometheus; and a load generator |

## Run it

```bash
make check
```

That installs the pinned tools into `bin/` on first use (controller-gen v0.21.0, setup-envtest v0.25.2, Helm v4.3.1,
and envtest's kube-apiserver and etcd for Kubernetes 1.37.0), regenerates the deep copies, CRD and ClusterRole (and
the chart's copies of them), and runs gofmt, go vet, `helm lint --strict` and every test. A plain `go test ./...`
skips the envtest and Helm suites; `make test` makes them fail instead if their binaries are missing.

## Install it in a cluster

There is no published image: build it (`make images` builds `canary:dev`; `kind load docker-image canary:dev` puts it
in a kind cluster) or push it to a registry of yours and set `image.repository` and `image.tag`.

With Helm (tested with 4.3.1, the version the repository pins; `$(make -s helm)` prints its path):

```bash
helm install canary charts/canary -n canary-system --create-namespace --set prometheus.url=http://prometheus.monitoring.svc:9090
```

That installs the CRD, a `canary` service account (objects are named after the release) with the generated
ClusterRole bound cluster-wide and the leader-election Role in `canary-system`, and the operator (two replicas, leader
election). The values ([charts/canary/values.yaml](charts/canary/values.yaml)) are checked against what the operator
accepts before anything is installed: an unknown key, a URL or timeout the operator would refuse at start, equal
ports, a `podLabels` key the chart sets, or two replicas without leader election are refused. Without
`prometheus.url` every check fails closed and a rollout with analysis rolls back; the install notes say so. `helm
uninstall` removes the operator and keeps the CRD, so the rollouts and their Deployments stay (unmanaged); Helm never
upgrades a CRD either, so a release that changes it needs `kubectl apply --server-side --field-manager=helm -f
charts/canary/crds/` first (DESIGN.md, "Installing it"). Install one operator per cluster.

With kustomize, the same objects (a test compares the two):

```bash
kubectl apply --server-side -k config/default
```

Add `--prometheus-url` to the operator's arguments with an overlay. Or install only the CRD and run the operator from
here:

```bash
kubectl apply --server-side -f config/crd/bases
```

```bash
go run ./cmd/canary --kubeconfig ~/.kube/config --prometheus-url http://localhost:9090
```

Then a rollout:

```bash
kubectl apply -f config/samples/web.yaml && kubectl get canaryrollouts
```

The CRD needs `--server-side` with kubectl: with the pod template's schema and descriptions it is 638,009 bytes, and a
client-side apply would store all of it in an annotation limited to 256 KiB (DESIGN.md, "Why these choices"; Helm
creates it without one). The sample creates `web-stable` at 10 pods; change its image and `kubectl get crl -w` shows
the phase, the step and the canary's actual share as the pods become ready (`kubectl get deploy -l
rollouts.vivekdavara.github.io/rollout=web` shows the two Deployments, `kubectl events --for canaryrollout/web` the
Events). The operator serves metrics on `:8111` and `/healthz` and `/readyz` on `:8121`. With `--leader-elect` (for
more than one replica) it also needs `--leader-election-namespace` outside a cluster, and
`config/rbac/leader_election_role.yaml` bound in that namespace (the Lease and its Events). The sample's queries assume
the application exports `http_requests_total` and a latency histogram, and that Prometheus keeps the operator's
`track` pod label as `track`. `--analysis-timeout` (10s) bounds each query.

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
| `cmd/canary/main_test.go` | the binary from its flags and a kubeconfig: probes answer, a rollout is reconciled, the reconcile is counted in its metrics, a clean stop; with `--leader-elect` it takes the Lease using only the leader-election Role, and gives it up when stopped (a second operator leads within 5 s; 15.9 s before day 5's fix); `--help`; bad `--prometheus-url` and `--analysis-timeout` values refused with the flag named |
| `config/install_test.go` | the README's install steps with envtest's kubectl: client-side apply of the CRD fails ("Too long"), `--server-side` works, the sample applies, `kubectl get crl` shows the columns; `config/default` applies, and `kubectl auth can-i` as its service account shows what it may do and what not (delete Deployments, read Secrets, take a Lease outside `canary-system`); the e2e tests' Prometheus applies |
| `charts/chart_test.go` | the Helm chart with the pinned helm: a release named `canary` renders `config/default`'s objects (same names, CRD spec, rules, role references, subjects, replicas and pod spec); its `crds/` and `files/role.yaml` are byte copies of the generated files; names per release; one replica without leader election has no Lease role and is replaced with `Recreate`, two with a one-at-a-time rolling update; 23 values refused, each with its message; one port set alone; every pod value where it lands; every namespaced object in the release's namespace; the notes warn without `prometheus.url`; `helm install` on an API server without the CRD (the chart's served, the account's permissions as `config/default`'s), an upgrade, an uninstall keeping the CRD and a rollout |
| `test/e2e/demoapp/main_test.go` (unit) | the demo app: the failure schedule (any n consecutive requests fail floor or ceil of n * ratio), the exposition (both codes from the start), the probes, the config refusing bad values, a clean stop, the load generator's rate and one connection per request |
| `test/e2e/e2e_test.go` (kind, CI; the operator installed by the chart) | DESIGN "End to end": a good version promoted, a bad one rolled back on measured error rates, one that never becomes ready rolled back at the deadline, one promoted across the leader's process being killed (SIGKILL) mid-pause and one across a `helm upgrade` of the operator mid-pause (the pause kept in both), capacity sampled every 100 ms throughout; the Prometheus client against a real Prometheus |

Checks that the tests can fail, each run as a mutant. Day 5: a writable root filesystem in the chart's Deployment, an
extra verb in its Lease role, and its ClusterRoleBinding's subject in another namespace each fail
`TestTheChartInstallsWhatConfigDefaultDoes` (the last also `TestHelmInstallUpgradeUninstall`). Day 3: letting a step held for its first check scale an
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

## Results (day 4)

The end-to-end tests on kind in GitHub Actions (ubuntu-24.04, kind v0.33.0, Kubernetes 1.37.0, Prometheus 3.15.1),
from the `RESULT` lines of the `e2e` job's log (`gh api repos/vivekdavara/canary/actions/jobs/<job>/logs`) in CI runs
41665332928 (at `6302a1f`) and 41666193349 (at `9c9ab68`, which added the leader crash). Each rollout has 4 pods (the
deadline's 2), a load generator at 40 requests a second, and checks every 5 s of the canary's error rate over 20 s,
two failures allowed:

| Rollout | Outcome | From the template change, run 1 / run 2 |
|---|---|---|
| fails no requests; steps 25% and 50%, 15 s pauses | promoted; 0 and 1 failed checks tolerated | 34.3 s / 35.0 s |
| fails half its requests | rolled back after 3 failed checks; 3 and 2 of them measured an error rate of about 0.5 (the others had no samples yet) | 17.0 s / 16.5 s |
| never becomes ready; deadline 20 s | rolled back at the deadline, no checks | 20.3 s / 20.6 s |
| fails no requests; the leader's pod killed 2.6 s into a 40 s pause | the other replica took the Lease 16.3 s after the kill; promoted, the pause kept | - / 56.1 s |

A third run (41669329436 at `c57b822`, which added the quota test) gave 35.1 s, 16.5 s, 20.6 s and 56.7 s (the Lease
taken 17.4 s after the kill), and the shares 30.2% and 50.9%. Its new test reproduced the ResourceQuota limitation:
with a pod quota of `replicas + 2`, 30 s after a rollback from the 50% step the stable had 3 of its 4 pods (its
ReplicaSet: `exceeded quota: pods, requested: pods=1, used: pods=6, limited: pods=6`), and the rolled-back canary's 2
ready pods still served 36.4% of the requests ("Limitations").

- **The replica ratio as a traffic split**, from the pods' own request counters over each step's last 10 s: at the
  25% step (1 canary pod of 4) the canary served 27.3% and 21.6% of the requests, at the 50% step (2 of 4) 51.1% and
  52.6%. That holds with one connection per request; with keep-alive connections it wouldn't ("Limitations").
- **Capacity**: the two Deployments' ready pods, sampled every 100 ms from the first version up to the end, never went
  below `replicas` in any rollout of either run (the leader crash included).
- **The failure limit is used**: in run 2 the good version's first check, 5 s into the pause, failed before Prometheus
  had two scrapes of the new pod (fail closed), and the next ones passed.
- **The Prometheus client against Prometheus 3.15.1**: all 15 answers as the unit tests' canned ones assumed (NaN and
  both infinities as strings, a matrix for a range, `bad_data` for a parse error, an info annotation ignored, a
  warning refused).
- The `e2e` job took 3 min 26 s and 4 min 21 s; the tests 37.3 s and 98.2 s (the leader crash runs alone first, then
  the other three in parallel). The first run (41664098318, before the two fixes in `6302a1f`) gave 16.0 s for the
  rollback and 20.6 s for the deadline.
- Locally (no Docker here) `make check` covers the manifests: `config/default` and the e2e overlay applied to envtest,
  and the service account's permissions checked with `kubectl auth can-i`.

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

## Review of day 4 (round 4 of day 3's fixes)

One agent on `b6b6df8`, round 3's fixes, which nobody had reviewed (about 14 minutes, its own worktree, findings as
tests). No capacity drop, loop or lost instant on a normal path; two low-severity disagreements between the
stable-gone branch of `Decide` and the normal path, fixed in `089f0b4`:

- A `failureLimit` lowered below the failures so far, with a check due and the stable gone: the branch ran and counted
  a check the normal path doesn't (2 checks and 2 failures instead of 1 and 1, an extra `AnalysisFailed`).
- A pause the normal path would restart (`replicas` raised mid-pause) with the stable gone: the branch still checked
  the old pause, added its failure and rolled back, where the normal path clears it.

Removing half of the second fix survived its test, so a third test covers that half. Three survivors of the repair's
tests (the refused-rewrite fallback, falling back on any error, upper-casing in the early check) each have a test now
(`7d8096a`). Each test fails before its fix or on its mutant; I re-ran them all here. Not tested, by the agent's
reading: a status-write conflict right after the stable is recreated loses that check's Event and condition for good.

## Review of day 5

One review agent at `0cca340` (the chart, about 11 minutes, its own worktree, findings as failing tests, kept as
`charts/review_d5_*_test.go`) reproduced six defects, all fixed in `983e52c`:

- **Medium: the schema let through values the operator exits on at start** (a crash loop in a cluster): Prometheus
  URLs `url.Parse` refuses (`:9O90`, a space in the host, an unclosed IPv6 bracket, `%zz`), `analysisTimeout` `0s` and
  `9999999h`, and the same port for metrics and probes. The patterns now follow the operator's own checks, equal
  ports fail the render, and `TestValuesTheSchemaAcceptsStartTheOperator` runs the real binary with what ten accepted
  values render.
- **Medium: the documented CRD upgrade command failed** (`conflict with "helm": .spec.versions`): Helm 4 applies
  `crds/` server-side. My first fix, `--force-conflicts`, made the next `helm install` conflict with `kubectl`; the
  command is now `--field-manager=helm`, and the test runs an install after it.
- **Medium: one replica without leader election had a rolling update**, which runs the old and new operator at once,
  both reconciling: `strategy: Recreate` now.
- Low: `podLabels` could override the selector's labels (refused by the chart now); `imagePullSecrets` took strings
  the API server refuses (both forms accepted now); releases `foo` and `foo-canary` shared a ClusterRole (objects are
  named exactly after the release now).

It also listed ten mutants every test let through (no token on the account, a paused Deployment, hard-coded
namespaces, ignored tolerations and `podLabels`, the naming rule, `config/manager` drifting): the comparison with
`config/default` now covers the whole Deployment spec and ServiceAccount, a render in another namespace checks every
namespaced object and subject, and every pod value is checked where it lands. Each fix and each survivor was re-run
as a mutant here and fails a test.

A second agent, on the fixes (`5586d5f`, about 10 minutes, `charts/review_d5b_values_test.go`), found five defects
in them, all fixed in `ca8f1ef`: **`helm upgrade` to no leader election was refused** (Helm 4 applies server-side,
and the API server's defaulted `rollingUpdate` stayed next to `Recreate`; the strategy is now always written out);
**pull-secret names were rendered unquoted** (`123`, `yes`, `1e3` broke the install); the IPv6 branch accepted
literals `url.Parse` refuses (`[1]`, `[:]`, nine groups); sub-nanosecond timeouts (`0.5ns`) parse to 0; a URL ending
in `:` became a YAML mapping (arguments are quoted now). Its four surviving mutants have tests, and
`schema_property_test.go` now checks the schema's patterns against the operator's own checks on 200,000 random
values each. Each fix was re-run as a mutant and fails a test. It read the Lease release as safe: reconciles get a
cancelled context at the stop, and a check cut short is no verdict, so the next leader checks again. Unverified, from
its reading: Events queued at the stop may be dropped (the recorder is stopped with an expired context).

Found by me the same day: a stopped leader kept its Lease for its full 15 s, so every upgrade or drain paused all
rollouts that long (fixed in `98636c3`: 15.9 s to 54-55 ms on envtest, 15.1 s to 0.7 s on kind); that fix made the
e2e crash test a clean stop (the kubelet sends SIGTERM even for a pod deleted with grace 0), so it now SIGKILLs the
container (`5586d5f`); and `TestAFailingAnalysisRollsBack` raced the asynchronous Event recorder once in CI
(`10b3cc2`).

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

`.github/workflows/ci.yml`, two jobs on every push: `test` runs `make check` on Ubuntu (the chart's tests and `helm
lint` included), then a check that the generated files (and the chart's copies of them) were committed, then the
build; `e2e` installs kind v0.33.0 (checksum verified), creates a Kubernetes 1.37.0 cluster (node image pinned by
digest), builds and loads both images, applies the tests' Prometheus and installs the operator with the chart, runs
`make e2e-test`, and on failure prints the rollouts, Deployments, pods, Events, logs and Prometheus's targets (`make
e2e-diagnose`).

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

- The end-to-end tests run on one kind node in CI only (there is no Docker on the machine it was built on); several
  nodes, a hung (not crashed) leader and traffic through a mesh aren't covered.
- No published image or chart repository: the chart installs from the repository, with an image you build.
- Helm installs the CRD but never upgrades it: a release that changes the CRD needs it applied by hand first (as
  Helm's field manager, or the next `helm install` conflicts).
- One operator per cluster isn't enforced: a second release of another name, in another namespace, would reconcile
  every rollout too (the Lease is per namespace).
- If a reconcile is still running at the manager's 30 s shutdown timeout, the stopping leader gives the Lease up
  anyway (DESIGN.md, "Leader election").
- A check fails closed, so a Prometheus outage rolls rollouts in progress back once `failureLimit` is used up.
- Reconciles run one at a time and a check runs inside one: a slow Prometheus delays other rollouts by up to
  `--analysis-timeout` per check.
- The progress deadline runs per step from when the step began waiting; editing `replicas` during the wait doesn't
  restart it.
- `AnalysisPassing` shows the last check run, which after a new rollout starts may be the previous rollout's until the
  first check (its message names the step and check number).
- After a rollback, a deleted stable isn't recreated until the template changes (it would run the failed version).
- **A tight ResourceQuota can hold a rollback forever, with the rolled-back version serving.** A rolled-back canary
  keeps its pods until the stable is back (capacity), and a quota with room for the forward steps may have none for
  that. Reproduced on kind (`TestATightQuotaHoldsARollbackWithTheCanaryServing`, a pod quota of `replicas + 2`, a
  rollback from the 50% step): 30 s after the rollback the stable had 3 of 4 pods, and the canary's 2 still served
  36.4% of the requests (DESIGN.md, "Limitations").
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
