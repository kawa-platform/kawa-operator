package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// GroupSpec is the desired RBAC group on the operator's gateway.
type GroupSpec struct {
	// clients are the authenticated client names in the group.
	// +optional
	// +listType=set
	Clients []string `json:"clients,omitempty"`

	// roles are the gateway roles whose ACLs the group's clients inherit, by name. A Role manages
	// the gateway role named after it.
	// +optional
	// +listType=set
	Roles []string `json:"roles,omitempty"`

	// deletionPolicy: Delete removes the group from the gateway when the Group is deleted (the
	// default), first taking its clients out of it. Retain leaves it.
	// +optional
	// +kubebuilder:default=Delete
	DeletionPolicy DeletionPolicy `json:"deletionPolicy,omitempty"`
}

// GroupStatus is the observed state of a Group.
type GroupStatus struct {
	// conditions: Ready is True when the gateway has the group as specified.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// observedGeneration is the generation the conditions were computed for.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// group is the group name the operator manages on the gateway. It is recorded before the
	// operator writes the group, so it can be set while the group is still missing on the gateway
	// after a failed write. The operator only deletes a group it manages.
	// +optional
	Group string `json:"group,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Group",type=string,JSONPath=`.status.group`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Group is an RBAC group on a kawa gateway, named after the Group: clients and the roles whose
// ACLs they inherit.
type Group struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec GroupSpec `json:"spec"`

	// +optional
	Status GroupStatus `json:"status,omitzero"`
}

// GatewayName is the name of the Group's group on the gateway: the Group's name.
func (g *Group) GatewayName() string {
	return g.Name
}

// ClaimedName is status.group, the group name the operator manages on the gateway for the Group.
func (g *Group) ClaimedName() *string {
	return &g.Status.Group
}

// ReadyStatus is where the Group reports its Ready condition and the generation it was computed for.
func (g *Group) ReadyStatus() (*[]metav1.Condition, *int64) {
	return &g.Status.Conditions, &g.Status.ObservedGeneration
}

// GetDeletionPolicy is spec.deletionPolicy.
func (g *Group) GetDeletionPolicy() DeletionPolicy {
	return g.Spec.DeletionPolicy
}

// +kubebuilder:object:root=true

// GroupList contains a list of Group.
type GroupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Group `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(scheme *runtime.Scheme) error {
		scheme.AddKnownTypes(SchemeGroupVersion, &Group{}, &GroupList{})
		return nil
	})
}
