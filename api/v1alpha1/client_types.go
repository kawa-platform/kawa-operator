package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Mechanism is the SASL mechanism a client authenticates with.
// +kubebuilder:validation:Enum=PLAIN;SCRAM-SHA-256;SCRAM-SHA-512
type Mechanism string

// SecretKeyReference is a key of a Secret in the referring object's namespace.
type SecretKeyReference struct {
	// name of the Secret.
	// +required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// key in the Secret's data.
	// +required
	// +kubebuilder:validation:MinLength=1
	Key string `json:"key"`
}

// ClientSpec is the desired SASL client on the operator's gateway. Which groups the client is in is
// set on the Groups, not here.
type ClientSpec struct {
	// mechanism the client authenticates with.
	// +required
	Mechanism Mechanism `json:"mechanism"`

	// passwordSecretRef is the Secret key holding the client's password. The operator writes the
	// password to the gateway again when the Secret changes.
	// +required
	PasswordSecretRef SecretKeyReference `json:"passwordSecretRef"`

	// deletionPolicy: Delete removes the client from the gateway when the Client is deleted (the
	// default), which also removes it from the gateway groups that list it. Retain leaves it.
	// +optional
	// +kubebuilder:default=Delete
	DeletionPolicy DeletionPolicy `json:"deletionPolicy,omitempty"`
}

// ClientStatus is the observed state of a Client.
type ClientStatus struct {
	// conditions: Ready is True when the gateway has the client as specified.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// observedGeneration is the generation the conditions were computed for.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// client is the client name the operator manages on the gateway. It is recorded before the
	// operator writes the client, so it can be set while the client is still missing on the gateway
	// after a failed write. The operator only deletes a client it manages.
	// +optional
	Client string `json:"client,omitempty"`

	// passwordVersion identifies the Secret version and key of the password the gateway has. The
	// gateway never returns a password, so this is how the operator knows when to write it again.
	// +optional
	PasswordVersion string `json:"passwordVersion,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Client",type=string,JSONPath=`.status.client`
// +kubebuilder:printcolumn:name="Mechanism",type=string,JSONPath=`.spec.mechanism`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Client is a SASL client on a kawa gateway, named after the Client: the username a Kafka client
// authenticates as, and the name Groups list it by.
type Client struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec ClientSpec `json:"spec"`

	// +optional
	Status ClientStatus `json:"status,omitzero"`
}

// GatewayName is the name of the Client's client on the gateway: the Client's name.
func (c *Client) GatewayName() string {
	return c.Name
}

// ClaimedName is status.client, the client name the operator manages on the gateway for the Client.
func (c *Client) ClaimedName() *string {
	return &c.Status.Client
}

// ReadyStatus is where the Client reports its Ready condition and the generation it was computed for.
func (c *Client) ReadyStatus() (*[]metav1.Condition, *int64) {
	return &c.Status.Conditions, &c.Status.ObservedGeneration
}

// GetDeletionPolicy is spec.deletionPolicy.
func (c *Client) GetDeletionPolicy() DeletionPolicy {
	return c.Spec.DeletionPolicy
}

// +kubebuilder:object:root=true

// ClientList contains a list of Client.
type ClientList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Client `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(scheme *runtime.Scheme) error {
		scheme.AddKnownTypes(SchemeGroupVersion, &Client{}, &ClientList{})
		return nil
	})
}
