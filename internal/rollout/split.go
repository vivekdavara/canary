// Package rollout holds the rollout's decisions (DESIGN.md, "The rollout"): how a step splits the pods, and what the
// operator does next given what it read. Nothing here talks to the API server, so all of it is unit tested.
package rollout

// Split returns the canary's and the stable's pod counts for a step of weight percent (1 to 100) of replicas pods
// (at least 1). The canary gets ceil(replicas * weight / 100), so every step runs at least one canary pod; the stable
// gets the rest, except that a step below 100% never takes the last stable pod, so canary + stable is then
// replicas + 1.
func Split(replicas, weight int32) (canary, stable int32) {
	canary = int32((int64(replicas)*int64(weight) + 99) / 100)
	stable = replicas - canary
	if weight < 100 && stable < 1 {
		stable = 1
	}
	return canary, stable
}

// Weight is the canary's share of the pods, in percent, rounded down; 0 when there are no pods.
func Weight(canary, stable int32) int32 {
	total := int64(canary) + int64(stable)
	if total <= 0 {
		return 0
	}
	return int32(int64(canary) * 100 / total)
}
