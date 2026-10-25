package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Operation is a Kafka operation an ACL allows or denies.
// +kubebuilder:validation:Enum=READ;WRITE;CREATE;DELETE;ALTER;DESCRIBE;ALTER_CONFIGS;DESCRIBE_CONFIGS;ALL
type Operation string

// Permission is whether an ACL allows or denies its operation.
// +kubebuilder:validation:Enum=ALLOW;DENY
type Permission string

// ResourceType is the kind of Kafka resource an ACL applies to.
// +kubebuilder:validation:Enum=TOPIC;GROUP;TRANSACTIONAL_ID;CLUSTER
type ResourceType string

// PatternType is how an ACL's pattern matches resource names.
// +kubebuilder:validation:Enum=LITERAL;PREFIXED
type PatternType string

const PatternTypeLiteral PatternType = "LITERAL"

// Resource is the resource an ACL applies to. Pattern is unset for CLUSTER, which matches the
// cluster regardless of name; it may be empty for PREFIXED, which matches every resource of Type.
// +kubebuilder:validation:XValidation:rule="self.type == 'CLUSTER' ? !has(self.pattern) : has(self.pattern)",message="pattern must be unset for CLUSTER and set for every other type"
// +kubebuilder:validation:XValidation:rule="!has(self.pattern) || size(self.pattern) > 0 || self.patternType == 'PREFIXED'",message="an empty pattern needs patternType PREFIXED"
type Resource struct {
	// type of the resource.
	// +required
	Type ResourceType `json:"type"`

	// pattern is the resource name, or with patternType PREFIXED its prefix.
	// +optional
	Pattern *string `json:"pattern,omitempty"`

	// patternType: LITERAL matches the name exactly (the default), PREFIXED every name that starts
	// with pattern.
	// +optional
	// +kubebuilder:default=LITERAL
	PatternType PatternType `json:"patternType,omitempty"`
}

// ACL allows or denies one operation on one resource.
type ACL struct {
	// resource the ACL applies to.
	// +required
	Resource Resource `json:"resource"`

	// operation the ACL allows or denies on resource.
	// +required
	Operation Operation `json:"operation"`

	// permission: ALLOW grants the operation, DENY refuses it. A matching DENY wins over any ALLOW.
	// +required
	Permission Permission `json:"permission"`
}

// RoleSpec is the desired RBAC role on the operator's gateway.
type RoleSpec struct {
	// acls of the role, as the gateway stores them.
	// +required
	// +kubebuilder:validation:MinItems=1
	// +listType=atomic
	ACLs []ACL `json:"acls"`

	// deletionPolicy: Delete removes the role from the gateway when the Role is deleted (the
	// default), which also removes it from the gateway groups that list it. Retain leaves it.
	// +optional
	// +kubebuilder:default=Delete
	DeletionPolicy DeletionPolicy `json:"deletionPolicy,omitempty"`
}

// RoleStatus is the observed state of a Role.
type RoleStatus struct {
	// conditions: Ready is True when the gateway has the role as specified.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// observedGeneration is the generation the conditions were computed for.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// role is the role name the operator manages on the gateway. It is recorded before the operator
	// writes the role, so it can be set while the role is still missing on the gateway after a failed
	// write. The operator only deletes a role it manages.
	// +optional
	Role string `json:"role,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Role",type=string,JSONPath=`.status.role`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Role is an RBAC role on a kawa gateway, named after the Role: ACLs that gateway groups grant
// to their clients.
type Role struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec RoleSpec `json:"spec"`

	// +optional
	Status RoleStatus `json:"status,omitzero"`
}

// GatewayName is the name of the Role's role on the gateway: the Role's name.
func (r *Role) GatewayName() string {
	return r.Name
}

// ClaimedName is status.role, the role name the operator manages on the gateway for the Role.
func (r *Role) ClaimedName() *string {
	return &r.Status.Role
}

// ReadyStatus is where the Role reports its Ready condition and the generation it was computed for.
func (r *Role) ReadyStatus() (*[]metav1.Condition, *int64) {
	return &r.Status.Conditions, &r.Status.ObservedGeneration
}

// GetDeletionPolicy is spec.deletionPolicy.
func (r *Role) GetDeletionPolicy() DeletionPolicy {
	return r.Spec.DeletionPolicy
}

// +kubebuilder:object:root=true

// RoleList contains a list of Role.
type RoleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Role `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(scheme *runtime.Scheme) error {
		scheme.AddKnownTypes(SchemeGroupVersion, &Role{}, &RoleList{})
		return nil
	})
}
