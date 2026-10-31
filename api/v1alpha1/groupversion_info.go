// Package v1alpha1 holds the CanaryRollout API (rollouts.vivekdavara.github.io/v1alpha1). DESIGN.md, "The API",
// explains each field and rule.
//
// +kubebuilder:object:generate=true
// +groupName=rollouts.vivekdavara.github.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the API's group and version.
	GroupVersion = schema.GroupVersion{Group: "rollouts.vivekdavara.github.io", Version: "v1alpha1"}

	// SchemeBuilder registers the API's types.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the API's types to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
