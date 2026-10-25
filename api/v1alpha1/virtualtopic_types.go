package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// AdoptAnnotation set to "true" on a VirtualTopic, Role, Group or Client, lets the operator take
// over a virtual topic, role, group or client that already exists on the gateway (created through kawa studio or
// the admin API).
// Without it the operator refuses, so two resources, or a resource and a hand-made one, never
// silently overwrite each other.
const AdoptAnnotation = "kawa-platform.github.io/adopt"

// VirtualTopicSpec is the desired virtual topic on the operator's gateway.
// +kubebuilder:validation:XValidation:rule="has(self.virtualTopic) == has(oldSelf.virtualTopic) && (!has(self.virtualTopic) || self.virtualTopic == oldSelf.virtualTopic)",message="virtualTopic is immutable"
type VirtualTopicSpec struct {
	// virtualTopic is the virtual topic name clients use. Defaults to the VirtualTopic's
	// metadata.name; set it for a name Kubernetes does not allow, such as one with upper case
	// letters or underscores. Immutable.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=249
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9._-]+$`
	VirtualTopic string `json:"virtualTopic,omitempty"`

	// physicalTopic is the topic on the Kafka cluster that backs the virtual topic.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=249
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9._-]+$`
	PhysicalTopic string `json:"physicalTopic"`

	// exposePhysicalTopic keeps the physical topic listed next to its virtual name in Metadata
	// responses instead of hiding it.
	// +optional
	ExposePhysicalTopic bool `json:"exposePhysicalTopic,omitempty"`

	// filter is a server-side consume filter: records that do not match are dropped at the
	// gateway during Fetch.
	// +optional
	Filter *VirtualTopicFilter `json:"filter,omitempty"`

	// valueFormat is the encoding of record values, so content-based (cel) filters see the
	// decoded value instead of the raw string.
	// +optional
	ValueFormat *PayloadFormat `json:"valueFormat,omitempty"`

	// deletionPolicy: Delete removes the virtual topic from the gateway when the VirtualTopic is
	// deleted (the default), Retain leaves it. The physical topic is never deleted.
	// +optional
	// +kubebuilder:default=Delete
	DeletionPolicy DeletionPolicy `json:"deletionPolicy,omitempty"`
}

// DeletionPolicy is what happens on the gateway when a VirtualTopic, Role, Group or Client is deleted.
// +kubebuilder:validation:Enum=Delete;Retain
type DeletionPolicy string

const (
	DeletionPolicyRetain DeletionPolicy = "Retain"
)

// FilterType is the kind of consume filter.
// +kubebuilder:validation:Enum=headerEquals;headerContains;headerStartsWith;headerMatches;cel
type FilterType string

const (
	FilterHeaderEquals     FilterType = "headerEquals"
	FilterHeaderContains   FilterType = "headerContains"
	FilterHeaderStartsWith FilterType = "headerStartsWith"
	FilterHeaderMatches    FilterType = "headerMatches"
	FilterCEL              FilterType = "cel"
)

// VirtualTopicFilter is a consume filter. The header filters compare the record header `header`
// with `value`; cel evaluates `expression` against key, value, headers and timestamp.
// +kubebuilder:validation:XValidation:rule="self.type == 'cel' ? has(self.expression) && !has(self.header) && !has(self.value) : has(self.header) && has(self.value) && !has(self.expression)",message="cel filters need expression only; header filters need header and value only"
type VirtualTopicFilter struct {
	// type of the filter.
	// +required
	Type FilterType `json:"type"`

	// header is the record header a header filter compares.
	// +optional
	// +kubebuilder:validation:MinLength=1
	Header string `json:"header,omitempty"`

	// value is what a header filter compares the header with: equal to, contained in, a prefix
	// of, or (headerMatches) a regular expression the whole header value must match.
	// +optional
	Value *string `json:"value,omitempty"`

	// expression is the CEL expression of a cel filter; it must evaluate to a boolean.
	// +optional
	// +kubebuilder:validation:MinLength=1
	Expression string `json:"expression,omitempty"`
}

// PayloadFormatType is the encoding of record values.
// +kubebuilder:validation:Enum=json;avro
type PayloadFormatType string

// DecodeErrorPolicy is what a content-based filter does with a value it cannot decode.
// +kubebuilder:validation:Enum=skip;include;fail
type DecodeErrorPolicy string

// PayloadFormat is the encoding of record values.
type PayloadFormat struct {
	// type: json, or avro in the schema registry wire format (the schema registry is configured
	// on the gateway's cluster).
	// +required
	Type PayloadFormatType `json:"type"`

	// onDecodeError: skip drops the record (the default), include delivers it, fail fails the fetch.
	// +optional
	// +kubebuilder:default=skip
	OnDecodeError DecodeErrorPolicy `json:"onDecodeError,omitempty"`
}

// VirtualTopicStatus is the observed state of a VirtualTopic.
type VirtualTopicStatus struct {
	// conditions: Ready is True when the gateway serves the virtual topic as specified.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// observedGeneration is the generation the conditions were computed for.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// virtualTopic is the virtual topic name the operator manages on the gateway. It is recorded
	// before the operator writes the topic, so it can be set while the topic is still missing on the
	// gateway after a failed write. The operator only deletes a topic it manages.
	// +optional
	VirtualTopic string `json:"virtualTopic,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=vt
// +kubebuilder:printcolumn:name="Topic",type=string,JSONPath=`.status.virtualTopic`
// +kubebuilder:printcolumn:name="Physical",type=string,JSONPath=`.spec.physicalTopic`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// VirtualTopic is a virtual topic on a kawa gateway: a name clients use that the gateway maps to
// a physical topic, optionally with a consume filter.
type VirtualTopic struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec VirtualTopicSpec `json:"spec"`

	// +optional
	Status VirtualTopicStatus `json:"status,omitzero"`
}

// VirtualTopicName is the virtual topic name on the gateway: spec.virtualTopic, or metadata.name.
func (v *VirtualTopic) VirtualTopicName() string {
	if v.Spec.VirtualTopic != "" {
		return v.Spec.VirtualTopic
	}
	return v.Name
}

// GatewayName is the name of the VirtualTopic's virtual topic on the gateway.
func (v *VirtualTopic) GatewayName() string {
	return v.VirtualTopicName()
}

// ClaimedName is status.virtualTopic, the virtual topic name the operator manages on the gateway
// for the VirtualTopic.
func (v *VirtualTopic) ClaimedName() *string {
	return &v.Status.VirtualTopic
}

// ReadyStatus is where the VirtualTopic reports its Ready condition and the generation it was
// computed for.
func (v *VirtualTopic) ReadyStatus() (*[]metav1.Condition, *int64) {
	return &v.Status.Conditions, &v.Status.ObservedGeneration
}

// GetDeletionPolicy is spec.deletionPolicy.
func (v *VirtualTopic) GetDeletionPolicy() DeletionPolicy {
	return v.Spec.DeletionPolicy
}

// +kubebuilder:object:root=true

// VirtualTopicList contains a list of VirtualTopic.
type VirtualTopicList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []VirtualTopic `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(scheme *runtime.Scheme) error {
		scheme.AddKnownTypes(SchemeGroupVersion, &VirtualTopic{}, &VirtualTopicList{})
		return nil
	})
}
