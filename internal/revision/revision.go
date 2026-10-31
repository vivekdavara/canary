// Package revision names a version of a pod template (DESIGN.md, "Status").
package revision

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// Length is the number of hex digits in a revision: 64 bits, and well inside a label value's 63 characters.
const Length = 16

// Of is the revision of a template: the first Length hex digits of the SHA-256 of its JSON encoding. encoding/json
// writes struct fields in declaration order and map keys sorted, and quantities in canonical form, so equal templates
// get equal revisions in every process.
//
// Hash the template as the API server stored it, not as a client sent it: the server fills defaults inside the pod
// template (a port's protocol, for one), so the two can differ.
func Of(t *corev1.PodTemplateSpec) string {
	b, err := json.Marshal(t)
	if err != nil {
		// A PodTemplateSpec has no channels, functions or cycles; json.Marshal can't fail on one.
		panic(fmt.Sprintf("revision: encoding a pod template: %v", err))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:Length]
}
