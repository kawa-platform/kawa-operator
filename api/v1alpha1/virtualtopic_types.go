package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
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

	// valueFormat is the encoding of record values, so content-based (cel) filters and
	// transformations see the decoded value instead of the raw string. Required with transformations.
	// +optional
	ValueFormat *PayloadFormat `json:"valueFormat,omitempty"`

	// transformations are value transformation steps the gateway applies in order to each record
	// value on fetch, after the filter; keys and headers are never changed. A convert step may only
	// be the last. The gateway validates the steps (paths, CEL expressions, output formats) and a
	// refusal is reported as InvalidSpec.
	// +optional
	// +listType=atomic
	Transformations []Transformation `json:"transformations,omitempty"`

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

// TransformationType is the kind of value transformation step.
// +kubebuilder:validation:Enum=dropFields;renameField;defaultField;cel;convert
type TransformationType string

const (
	TransformationDropFields   TransformationType = "dropFields"
	TransformationRenameField  TransformationType = "renameField"
	TransformationDefaultField TransformationType = "defaultField"
	TransformationCEL          TransformationType = "cel"
	TransformationConvert      TransformationType = "convert"
)

// Transformation is one value transformation step. Fields are addressed by dotted paths
// (customer.name); a list on a path applies the step to every element.
// +kubebuilder:validation:XValidation:rule="self.type == 'dropFields' ? has(self.fields) : !has(self.fields)",message="fields is required for dropFields and allowed only there"
// +kubebuilder:validation:XValidation:rule="self.type == 'renameField' ? has(self.from) && has(self.to) : !has(self.from) && !has(self.to)",message="from and to are required for renameField and allowed only there"
// +kubebuilder:validation:XValidation:rule="self.type == 'defaultField' ? has(self.field) : !has(self.field)",message="field is required for defaultField and allowed only there"
// +kubebuilder:validation:XValidation:rule="self.type == 'cel' ? has(self.expression) : !has(self.expression)",message="expression is required for cel and allowed only there"
// +kubebuilder:validation:XValidation:rule="self.type == 'convert' ? has(self.format) : !has(self.format)",message="format is required for convert and allowed only there"
type Transformation struct {
	// type of the step: dropFields removes fields, renameField moves a field, defaultField sets a
	// field when it is absent or null, cel replaces the value with the result of a CEL expression,
	// convert encodes the result in another format.
	// +required
	Type TransformationType `json:"type"`

	// fields are the dotted paths a dropFields step removes; a path the value does not have is ignored.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:items:MinLength=1
	Fields []string `json:"fields,omitempty"`

	// from is the dotted path of the field a renameField step moves.
	// +optional
	// +kubebuilder:validation:MinLength=1
	From string `json:"from,omitempty"`

	// to is the dotted path a renameField step moves the field to.
	// +optional
	// +kubebuilder:validation:MinLength=1
	To string `json:"to,omitempty"`

	// field is the dotted path a defaultField step sets; missing parent objects are created.
	// +optional
	// +kubebuilder:validation:MinLength=1
	Field string `json:"field,omitempty"`

	// value is the default of a defaultField step, any JSON value; omitted (or null, which the API
	// server drops) means null. Only for defaultField: the gateway refuses it on other steps, which
	// is reported as InvalidSpec, since CEL cannot check a schemaless field at admission.
	// +optional
	Value *apiextensionsv1.JSON `json:"value,omitempty"`

	// expression is the CEL expression of a cel step, with the key, value, headers and timestamp
	// bindings; its result replaces the value.
	// +optional
	// +kubebuilder:validation:MinLength=1
	Expression string `json:"expression,omitempty"`

	// format is what a convert step encodes the value as.
	// +optional
	Format *PayloadFormat `json:"format,omitempty"`
}

// PayloadFormatType is the encoding of record values.
// +kubebuilder:validation:Enum=json;avro;xml
type PayloadFormatType string

// DecodeErrorPolicy is what the gateway does with a value it cannot decode, or that the
// transformations cannot transform.
// +kubebuilder:validation:Enum=skip;include;fail
type DecodeErrorPolicy string

// PayloadFormat is the encoding of record values.
// +kubebuilder:validation:XValidation:rule="self.type == 'avro' || !has(self.subject) && !has(self.version)",message="subject and version are allowed only for avro"
// +kubebuilder:validation:XValidation:rule="!has(self.version) || has(self.subject)",message="version requires subject"
// +kubebuilder:validation:XValidation:rule="self.type == 'xml' || !has(self.root)",message="root is allowed only for xml"
type PayloadFormat struct {
	// type: json, avro in the schema registry wire format (the schema registry is configured
	// on the gateway's cluster), or xml.
	// +required
	Type PayloadFormatType `json:"type"`

	// onDecodeError: skip drops the record (the default), include delivers it as stored (unfiltered,
	// untransformed), fail fails the fetch.
	// +optional
	// +kubebuilder:default=skip
	OnDecodeError DecodeErrorPolicy `json:"onDecodeError,omitempty"`

	// subject is the schema registry subject of the schema avro values are encoded with. Required
	// when this format is the output of transformations (the convert format, else valueFormat).
	// +optional
	// +kubebuilder:validation:MinLength=1
	Subject string `json:"subject,omitempty"`

	// version is the subject version avro values are encoded with; omitted means the latest.
	// +optional
	// +kubebuilder:validation:Minimum=1
	Version int32 `json:"version,omitempty"`

	// root is the root element name of xml values. Required when this format is the output of
	// transformations; when set, decoding refuses another root.
	// +optional
	// +kubebuilder:validation:MinLength=1
	Root string `json:"root,omitempty"`
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
