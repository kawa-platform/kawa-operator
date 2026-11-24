package controller

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kawav1alpha1 "github.com/kawa-platform/kawa-operator/api/v1alpha1"
	"github.com/kawa-platform/kawa-operator/internal/kawa"
	"github.com/kawa-platform/kawa-operator/internal/kawa/kawatest"
)

var _ = Describe("VirtualTopic controller", func() {
	var ns string

	BeforeEach(func() {
		ns = newNamespace()
	})

	// VirtualTopic names are unique cluster-wide, and every spec uses "orders": remove the spec's
	// VirtualTopics, without their finalizers so that this does not depend on the spec's gateway.
	AfterEach(func() {
		removeAll(&kawav1alpha1.VirtualTopicList{})
	})

	It("creates the virtual topic on the gateway and reports it synced", func() {
		// given
		gateway := newGateway()
		vt := &kawav1alpha1.VirtualTopic{
			ObjectMeta: v1.ObjectMeta{Namespace: ns, Name: "orders-eu"},
			Spec:       kawav1alpha1.VirtualTopicSpec{PhysicalTopic: "orders-v2"},
		}
		vt.Spec.VirtualTopic = "orders.eu"
		vt.Spec.Filter = &kawav1alpha1.VirtualTopicFilter{Type: kawav1alpha1.FilterHeaderEquals, Header: "region", Value: ptr.To("eu")}
		vt.Spec.ValueFormat = &kawav1alpha1.PayloadFormat{Type: "json"}

		// when
		Expect(k8sClient.Create(ctx, vt)).To(Succeed())

		// then
		Eventually(readyReason(vt), eventually, poll).Should(Equal(ReasonSynced))
		topic, ok := gateway.Virtual("orders.eu")
		Expect(ok).To(BeTrue(), "the virtual topic was not created under spec.virtualTopic")
		Expect(topic).To(Equal(kawa.VirtualTopic{
			Topic:       "orders-v2",
			Filter:      &kawa.Filter{Type: "headerEquals", Header: "region", Value: "eu"},
			ValueFormat: &kawa.PayloadFormat{Type: "json", OnDecodeError: "skip"},
		}))
		Expect(vt.Status.VirtualTopic).To(Equal("orders.eu"), "status.virtualTopic does not name the managed topic")
		Expect(vt.Finalizers).To(ContainElement(VirtualTopicFinalizer))
	})

	It("updates the virtual topic when the spec changes", func() {
		// given
		gateway := newGateway()
		vt := &kawav1alpha1.VirtualTopic{
			ObjectMeta: v1.ObjectMeta{Namespace: ns, Name: "orders"},
			Spec:       kawav1alpha1.VirtualTopicSpec{PhysicalTopic: "orders-v1"},
		}
		Expect(k8sClient.Create(ctx, vt)).To(Succeed())
		Eventually(ordersPhysicalTopic(gateway), eventually, poll).Should(Equal("orders-v1"))

		// when
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(vt), vt)).To(Succeed())
		vt.Spec.PhysicalTopic = "orders-v2"
		vt.Spec.Filter = &kawav1alpha1.VirtualTopicFilter{Type: kawav1alpha1.FilterCEL, Expression: `headers.region == "eu"`}
		Expect(k8sClient.Update(ctx, vt)).To(Succeed())

		// then
		Eventually(ordersPhysicalTopic(gateway), eventually, poll).Should(Equal("orders-v2"))
		topic, _ := gateway.Virtual("orders")
		Expect(topic.Filter).To(Equal(&kawa.Filter{Type: "cel", Expression: `headers.region == "eu"`}))
	})

	It("undoes changes made to the virtual topic outside the operator", func() {
		// given
		gateway := newGateway()
		vt := &kawav1alpha1.VirtualTopic{
			ObjectMeta: v1.ObjectMeta{Namespace: ns, Name: "orders"},
			Spec:       kawav1alpha1.VirtualTopicSpec{PhysicalTopic: "orders-v2"},
		}
		Expect(k8sClient.Create(ctx, vt)).To(Succeed())
		Eventually(ordersPhysicalTopic(gateway), eventually, poll).Should(Equal("orders-v2"))

		// when: someone repoints it in kawa studio
		gateway.SetVirtual("orders", kawa.VirtualTopic{Topic: "something-else"})

		// then
		Eventually(ordersPhysicalTopic(gateway), eventually, poll).Should(Equal("orders-v2"))
	})

	It("does not rewrite a virtual topic that is already in sync", func() {
		// given
		gateway := newGateway()
		vt := &kawav1alpha1.VirtualTopic{
			ObjectMeta: v1.ObjectMeta{Namespace: ns, Name: "orders"},
			Spec:       kawav1alpha1.VirtualTopicSpec{PhysicalTopic: "orders-v2"},
		}
		vt.Spec.Filter = &kawav1alpha1.VirtualTopicFilter{Type: kawav1alpha1.FilterHeaderMatches, Header: "region", Value: ptr.To("eu-.*")}
		Expect(k8sClient.Create(ctx, vt)).To(Succeed())
		Eventually(readyReason(vt), eventually, poll).Should(Equal(ReasonSynced))

		// when: several resyncs pass
		time.Sleep(3 * testResyncInterval)

		// then
		Expect(gateway.Writes("orders")).To(Equal(1), "an in-sync virtual topic was written again on resync")
	})

	It("writes an unmarked virtual topic once so that the gateway marks it, and then leaves it alone", func() {
		// given: a synced virtual topic the gateway lists unmarked, as one written before it marked resources
		gateway := newGateway()
		vt := &kawav1alpha1.VirtualTopic{
			ObjectMeta: v1.ObjectMeta{Namespace: ns, Name: "orders"},
			Spec:       kawav1alpha1.VirtualTopicSpec{PhysicalTopic: "orders-v2"},
		}
		Expect(k8sClient.Create(ctx, vt)).To(Succeed())
		Eventually(readyReason(vt), eventually, poll).Should(Equal(ReasonSynced))
		stored, _ := gateway.Virtual("orders")
		gateway.SetVirtual("orders", stored)

		// when: several resyncs pass
		Eventually(func() string { return gateway.ManagedBy("orders") }, eventually, poll).
			Should(Equal(kawatest.OperatorPrincipal), "the unmarked virtual topic was not written again")
		time.Sleep(3 * testResyncInterval)

		// then
		Expect(gateway.Writes("orders")).To(Equal(2), "the marked virtual topic was written again on resync")
	})

	It("lets nobody but the operator change the virtual topic it manages", func() {
		// given
		gateway := newGateway()
		vt := &kawav1alpha1.VirtualTopic{
			ObjectMeta: v1.ObjectMeta{Namespace: ns, Name: "orders"},
			Spec:       kawav1alpha1.VirtualTopicSpec{PhysicalTopic: "orders-v2"},
		}
		Expect(k8sClient.Create(ctx, vt)).To(Succeed())
		Eventually(readyReason(vt), eventually, poll).Should(Equal(ReasonSynced))

		// when: someone edits it in kawa studio
		err := studio(gateway).PutVirtualTopic(ctx, "orders", kawa.VirtualTopic{Topic: "payments"})

		// then
		managedConflict(err, "topic", "orders")
		Expect(ordersPhysicalTopic(gateway)()).To(Equal("orders-v2"))
	})

	It("refuses a virtual topic that already exists on the gateway, and adopts it when annotated", func() {
		// given
		gateway := newGateway()
		gateway.SetVirtual("orders", kawa.VirtualTopic{Topic: "made-in-studio"})
		vt := &kawav1alpha1.VirtualTopic{
			ObjectMeta: v1.ObjectMeta{Namespace: ns, Name: "orders"},
			Spec:       kawav1alpha1.VirtualTopicSpec{PhysicalTopic: "orders-v2"},
		}
		Expect(k8sClient.Create(ctx, vt)).To(Succeed())
		Eventually(readyReason(vt), eventually, poll).Should(Equal(ReasonAlreadyExists))
		Expect(ordersPhysicalTopic(gateway)()).To(Equal("made-in-studio"), "an existing topic was overwritten without kawa-platform.github.io/adopt")

		// when
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(vt), vt)).To(Succeed())
		vt.Annotations = map[string]string{kawav1alpha1.AdoptAnnotation: "true"}
		Expect(k8sClient.Update(ctx, vt)).To(Succeed())

		// then
		Eventually(ordersPhysicalTopic(gateway), eventually, poll).Should(Equal("orders-v2"))
		Eventually(readyReason(vt), eventually, poll).Should(Equal(ReasonSynced))
	})

	It("lets only the oldest of two VirtualTopics for the same name manage it", func() {
		// given
		gateway := newGateway()
		first := &kawav1alpha1.VirtualTopic{
			ObjectMeta: v1.ObjectMeta{Namespace: ns, Name: "orders-a"},
			Spec:       kawav1alpha1.VirtualTopicSpec{PhysicalTopic: "orders-a"},
		}
		first.Spec.VirtualTopic = "orders"
		Expect(k8sClient.Create(ctx, first)).To(Succeed())
		Eventually(readyReason(first), eventually, poll).Should(Equal(ReasonSynced))

		// when
		time.Sleep(time.Second) // creation timestamps have second precision
		second := &kawav1alpha1.VirtualTopic{
			ObjectMeta: v1.ObjectMeta{Namespace: ns, Name: "orders-b"},
			Spec:       kawav1alpha1.VirtualTopicSpec{PhysicalTopic: "orders-b"},
		}
		second.Spec.VirtualTopic = "orders"
		Expect(k8sClient.Create(ctx, second)).To(Succeed())

		// then
		Eventually(readyReason(second), eventually, poll).Should(Equal(ReasonConflict))
		Consistently(ordersPhysicalTopic(gateway), 2*testResyncInterval, poll).Should(Equal("orders-a"))
	})

	It("reports a governance rejection and syncs once the rule allows it", func() {
		// given
		gateway := newGateway()
		gateway.Reject("orders", http.StatusForbidden, "topic 'orders' rejected by governance: [naming] topics need a team prefix")
		vt := &kawav1alpha1.VirtualTopic{
			ObjectMeta: v1.ObjectMeta{Namespace: ns, Name: "orders"},
			Spec:       kawav1alpha1.VirtualTopicSpec{PhysicalTopic: "orders-v2"},
		}

		// when
		Expect(k8sClient.Create(ctx, vt)).To(Succeed())

		// then
		Eventually(readyReason(vt), eventually, poll).Should(Equal(ReasonGovernanceRejected))
		Expect(readyMessage(vt)).To(ContainSubstring("topics need a team prefix"))

		// and when the rule changes
		gateway.Reject("orders", 0, "")
		Eventually(readyReason(vt), eventually, poll).Should(Equal(ReasonSynced))
	})

	It("deletes the virtual topic from the gateway with the VirtualTopic", func() {
		// given
		gateway := newGateway()
		gateway.AddPhysical("orders-v2")
		vt := &kawav1alpha1.VirtualTopic{
			ObjectMeta: v1.ObjectMeta{Namespace: ns, Name: "orders"},
			Spec:       kawav1alpha1.VirtualTopicSpec{PhysicalTopic: "orders-v2"},
		}
		Expect(k8sClient.Create(ctx, vt)).To(Succeed())
		Eventually(ordersPhysicalTopic(gateway), eventually, poll).Should(Equal("orders-v2"))

		// when
		Expect(k8sClient.Delete(ctx, vt)).To(Succeed())

		// then
		Eventually(gone(vt), eventually, poll).Should(BeTrue())
		Expect(ordersPhysicalTopic(gateway)()).To(BeEmpty(), "the virtual topic was left on the gateway")
		Expect(gateway.HasPhysical("orders-v2")).To(BeTrue(), "the physical topic was deleted")
	})

	It("keeps the virtual topic on the gateway with deletionPolicy Retain", func() {
		// given
		gateway := newGateway()
		vt := &kawav1alpha1.VirtualTopic{
			ObjectMeta: v1.ObjectMeta{Namespace: ns, Name: "orders"},
			Spec:       kawav1alpha1.VirtualTopicSpec{PhysicalTopic: "orders-v2"},
		}
		vt.Spec.DeletionPolicy = kawav1alpha1.DeletionPolicyRetain
		Expect(k8sClient.Create(ctx, vt)).To(Succeed())
		Eventually(ordersPhysicalTopic(gateway), eventually, poll).Should(Equal("orders-v2"))

		// when
		Expect(k8sClient.Delete(ctx, vt)).To(Succeed())

		// then
		Eventually(gone(vt), eventually, poll).Should(BeTrue())
		Expect(ordersPhysicalTopic(gateway)()).To(Equal("orders-v2"))
		Expect(gateway.ManagedBy("orders")).To(BeEmpty(), "a retained virtual topic was not released")
		Expect(studio(gateway).PutVirtualTopic(ctx, "orders", kawa.VirtualTopic{Topic: "payments"})).
			To(Succeed(), "a released virtual topic cannot be edited")
	})

	It("never deletes a physical topic that has the name of a removed virtual topic", func() {
		// given: the operator's virtual topic was removed in studio, and a physical topic has its name
		gateway := newGateway()
		vt := &kawav1alpha1.VirtualTopic{
			ObjectMeta: v1.ObjectMeta{Namespace: ns, Name: "orders"},
			Spec:       kawav1alpha1.VirtualTopicSpec{PhysicalTopic: "orders-v2"},
		}
		Expect(k8sClient.Create(ctx, vt)).To(Succeed())
		Eventually(readyReason(vt), eventually, poll).Should(Equal(ReasonSynced))
		gateway.RemoveVirtual("orders")
		gateway.AddPhysical("orders")

		// when
		Expect(k8sClient.Delete(ctx, vt)).To(Succeed())

		// then
		Eventually(gone(vt), eventually, poll).Should(BeTrue())
		Expect(gateway.HasPhysical("orders")).To(BeTrue(), "the finalizer deleted a physical topic")
	})

	It("acts as the operator on the gateway", func() {
		// given
		gateway := newGateway()

		// when
		vt := &kawav1alpha1.VirtualTopic{
			ObjectMeta: v1.ObjectMeta{Namespace: ns, Name: "orders"},
			Spec:       kawav1alpha1.VirtualTopicSpec{PhysicalTopic: "orders-v2"},
		}
		Expect(k8sClient.Create(ctx, vt)).To(Succeed())

		// then
		Eventually(readyReason(vt), eventually, poll).Should(Equal(ReasonSynced))
		Expect(gateway.WritesBy("orders")).To(Equal([]string{"kawa-operator"}), "the write was not made as the operator")
	})

	It("reports refused client credentials", func() {
		// given
		gateway := newGateway()
		useGateway(GatewayConfig{AdminURL: gateway.URL, ClientID: "kawa-operator", ClientSecretFile: secretFile("wrong")})

		// when
		vt := &kawav1alpha1.VirtualTopic{
			ObjectMeta: v1.ObjectMeta{Namespace: ns, Name: "orders"},
			Spec:       kawav1alpha1.VirtualTopicSpec{PhysicalTopic: "orders-v2"},
		}
		Expect(k8sClient.Create(ctx, vt)).To(Succeed())

		// then
		Eventually(readyReason(vt), eventually, poll).Should(Equal(ReasonAuthenticationFailed))
	})

	It("reports a missing client secret file and picks up a written one", func() {
		// given
		gateway := newGateway()
		path := filepath.Join(GinkgoT().TempDir(), "client-secret")
		useGateway(GatewayConfig{AdminURL: gateway.URL, ClientID: "kawa-operator", ClientSecretFile: path})
		vt := &kawav1alpha1.VirtualTopic{
			ObjectMeta: v1.ObjectMeta{Namespace: ns, Name: "orders"},
			Spec:       kawav1alpha1.VirtualTopicSpec{PhysicalTopic: "orders-v2"},
		}
		Expect(k8sClient.Create(ctx, vt)).To(Succeed())
		Eventually(readyReason(vt), eventually, poll).Should(Equal(ReasonGatewayConfigInvalid))

		// when: the mounted Secret appears (or is rotated)
		Expect(os.WriteFile(path, []byte("s3cret"), 0o600)).To(Succeed())

		// then
		Eventually(readyReason(vt), 2*eventually, poll).Should(Equal(ReasonSynced))
	})

	It("does not serve a namespace outside the namespace selector, until it matches", func() {
		// given
		gateway := newGateway()
		namespace := &corev1.Namespace{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: ns}, namespace)).To(Succeed())
		namespace.Labels[excludedLabel] = "true"
		Expect(k8sClient.Update(ctx, namespace)).To(Succeed())
		vt := &kawav1alpha1.VirtualTopic{
			ObjectMeta: v1.ObjectMeta{Namespace: ns, Name: "orders"},
			Spec:       kawav1alpha1.VirtualTopicSpec{PhysicalTopic: "orders-v2"},
		}
		Expect(k8sClient.Create(ctx, vt)).To(Succeed())
		Eventually(readyReason(vt), eventually, poll).Should(Equal(ReasonNamespaceNotAllowed))
		Expect(gateway.Writes("orders")).To(BeZero())

		// when
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: ns}, namespace)).To(Succeed())
		delete(namespace.Labels, excludedLabel)
		Expect(k8sClient.Update(ctx, namespace)).To(Succeed())

		// then
		Eventually(ordersPhysicalTopic(gateway), eventually, poll).Should(Equal("orders-v2"))
	})

	It("lets only the oldest VirtualTopic of any namespace manage a name", func() {
		// given
		gateway := newGateway()
		first := &kawav1alpha1.VirtualTopic{
			ObjectMeta: v1.ObjectMeta{Namespace: ns, Name: "orders"},
			Spec:       kawav1alpha1.VirtualTopicSpec{PhysicalTopic: "orders-a"},
		}
		Expect(k8sClient.Create(ctx, first)).To(Succeed())
		Eventually(readyReason(first), eventually, poll).Should(Equal(ReasonSynced))

		// when
		time.Sleep(time.Second) // creation timestamps have second precision
		other := newNamespace()
		second := &kawav1alpha1.VirtualTopic{
			ObjectMeta: v1.ObjectMeta{Namespace: other, Name: "orders"},
			Spec:       kawav1alpha1.VirtualTopicSpec{PhysicalTopic: "orders-b"},
		}
		Expect(k8sClient.Create(ctx, second)).To(Succeed())

		// then
		Eventually(readyReason(second), eventually, poll).Should(Equal(ReasonConflict))
		Expect(readyMessage(second)).To(ContainSubstring(ns + "/orders"))
		Consistently(ordersPhysicalTopic(gateway), 2*testResyncInterval, poll).Should(Equal("orders-a"))
	})

	It("serves the transformations of the spec and reports it synced", func() {
		// given
		gateway := newGateway()
		vt := transformedOrders(ns)

		// when
		Expect(k8sClient.Create(ctx, vt)).To(Succeed())

		// then
		Eventually(readyReason(vt), eventually, poll).Should(Equal(ReasonSynced))
		topic, _ := gateway.Virtual("orders")
		Expect(topic.ValueFormat).To(Equal(&kawa.PayloadFormat{Type: "avro", OnDecodeError: "skip"}))
		Expect(kawa.TransformationsEqual(topic.Transformations, []kawa.Transformation{
			{Type: "dropFields", Fields: []string{"internal_risk_score"}},
			{Type: "renameField", From: "order_id", To: "id"},
			{Type: "defaultField", Field: "quantity", Value: json.RawMessage(`{"amount": 1, "unit": "piece"}`)},
			{Type: "convert", Format: &kawa.PayloadFormat{Type: "json", OnDecodeError: "skip"}},
		})).To(BeTrue(), "the gateway holds the steps %+v", topic.Transformations)
	})

	It("serves header steps after the convert step", func() {
		// given
		gateway := newGateway()
		vt := transformedOrders(ns)
		vt.Spec.Transformations = append(vt.Spec.Transformations,
			kawav1alpha1.Transformation{Type: kawav1alpha1.TransformationDropHeaders, Headers: []string{"__TypeId__"}},
			kawav1alpha1.Transformation{Type: kawav1alpha1.TransformationSetHeader, Header: "content-type",
				Value: &apiextensionsv1.JSON{Raw: []byte(`"application/json"`)}})

		// when
		Expect(k8sClient.Create(ctx, vt)).To(Succeed())

		// then
		Eventually(readyReason(vt), eventually, poll).Should(Equal(ReasonSynced))
		topic, _ := gateway.Virtual("orders")
		Expect(topic.Transformations[4:]).To(Equal([]kawa.Transformation{
			{Type: "dropHeaders", Headers: []string{"__TypeId__"}},
			{Type: "setHeader", Header: "content-type", Value: json.RawMessage(`"application/json"`)},
		}), "the gateway holds the steps %+v", topic.Transformations)
	})

	It("restores transformations changed outside the operator", func() {
		// given
		gateway := newGateway()
		vt := transformedOrders(ns)
		Expect(k8sClient.Create(ctx, vt)).To(Succeed())
		Eventually(readyReason(vt), eventually, poll).Should(Equal(ReasonSynced))

		// when: someone drops a step on the gateway
		gateway.EditVirtual("orders", func(topic *kawa.VirtualTopic) {
			topic.Transformations = topic.Transformations[1:]
		})

		// then
		Eventually(func() int {
			topic, _ := gateway.Virtual("orders")
			return len(topic.Transformations)
		}, eventually, poll).Should(Equal(4), "the dropped step was not restored")
	})

	It("does not rewrite transformations that are already in sync", func() {
		// given: a convert without onDecodeError and a numeric default, which the gateway lists
		// normalized and re-encoded
		gateway := newGateway()
		vt := transformedOrders(ns)
		Expect(k8sClient.Create(ctx, vt)).To(Succeed())
		Eventually(readyReason(vt), eventually, poll).Should(Equal(ReasonSynced))

		// when: several resyncs pass
		time.Sleep(3 * testResyncInterval)

		// then
		Expect(gateway.Writes("orders")).To(Equal(1), "in-sync transformations were written again on resync")
	})

	It("clears the transformations removed from the spec", func() {
		// given
		gateway := newGateway()
		vt := transformedOrders(ns)
		Expect(k8sClient.Create(ctx, vt)).To(Succeed())
		Eventually(readyReason(vt), eventually, poll).Should(Equal(ReasonSynced))

		// when
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(vt), vt)).To(Succeed())
		vt.Spec.Transformations = nil
		Expect(k8sClient.Update(ctx, vt)).To(Succeed())

		// then
		Eventually(func() []kawa.Transformation {
			topic, _ := gateway.Virtual("orders")
			return topic.Transformations
		}, eventually, poll).Should(BeEmpty())
	})

	It("sends a defaultField without value, or with value null, as null", func() {
		// given: one step leaving value out and one setting it to null, written as unstructured so
		// that the null reaches the API server
		gateway := newGateway()
		vt := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": kawav1alpha1.GroupVersion.String(), "kind": "VirtualTopic",
			"metadata": map[string]any{"namespace": ns, "name": "orders"},
			"spec": map[string]any{
				"physicalTopic": "orders-v2",
				"valueFormat":   map[string]any{"type": "json"},
				"transformations": []any{
					map[string]any{"type": "defaultField", "field": "currency"},
					map[string]any{"type": "defaultField", "field": "note", "value": nil},
				},
			},
		}}

		// when
		Expect(k8sClient.Create(ctx, vt)).To(Succeed())

		// then
		typed := &kawav1alpha1.VirtualTopic{ObjectMeta: v1.ObjectMeta{Namespace: ns, Name: "orders"}}
		Eventually(readyReason(typed), eventually, poll).Should(Equal(ReasonSynced))
		topic, _ := gateway.Virtual("orders")
		Expect(topic.Transformations).To(HaveLen(2))
		for _, step := range topic.Transformations {
			Expect(string(step.Value)).To(Equal("null"), "the step %+v was not sent with value null", step)
		}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(vt), vt)).To(Succeed())
		stored := vt.Object["spec"].(map[string]any)["transformations"].([]any)[1].(map[string]any)
		Expect(stored).NotTo(HaveKey("value"), "the API server kept the explicit null")
		Consistently(func() int { return gateway.Writes("orders") }, 3*testResyncInterval, poll).Should(Equal(1),
			"a null default was written again on resync")
	})

	It("reports a value on a step other than defaultField as an invalid spec", func() {
		// given: CEL rules cannot check the schemaless value, so the gateway refuses it
		newGateway()
		vt := transformedOrders(ns)
		vt.Spec.Transformations[1].Value = &apiextensionsv1.JSON{Raw: []byte(`1`)}

		// when
		Expect(k8sClient.Create(ctx, vt)).To(Succeed())

		// then
		Eventually(readyReason(vt), eventually, poll).Should(Equal(ReasonInvalidSpec))
		Expect(readyMessage(vt)).To(ContainSubstring("transformations[1] (renameField): unknown property 'value'"))
	})

	It("reports transformations the gateway refuses as an invalid spec", func() {
		// given: a convert that is not the last step, which only the gateway checks
		gateway := newGateway()
		vt := transformedOrders(ns)
		last := len(vt.Spec.Transformations) - 1
		vt.Spec.Transformations[0], vt.Spec.Transformations[last] = vt.Spec.Transformations[last], vt.Spec.Transformations[0]

		// when
		Expect(k8sClient.Create(ctx, vt)).To(Succeed())

		// then
		Eventually(readyReason(vt), eventually, poll).Should(Equal(ReasonInvalidSpec))
		Expect(readyMessage(vt)).To(ContainSubstring("transformations[0] (convert)"))
		_, exists := gateway.Virtual("orders")
		Expect(exists).To(BeFalse())
	})

	It("rejects an invalid VirtualTopic at admission", func() {
		celWithoutExpression := &kawav1alpha1.VirtualTopic{
			ObjectMeta: v1.ObjectMeta{Namespace: ns, Name: "a"},
			Spec:       kawav1alpha1.VirtualTopicSpec{PhysicalTopic: "orders-v2"},
		}
		celWithoutExpression.Spec.Filter = &kawav1alpha1.VirtualTopicFilter{Type: kawav1alpha1.FilterCEL}
		Expect(invalid(celWithoutExpression)).To(ContainSubstring("cel filters need expression only"))

		headerWithoutValue := &kawav1alpha1.VirtualTopic{
			ObjectMeta: v1.ObjectMeta{Namespace: ns, Name: "b"},
			Spec:       kawav1alpha1.VirtualTopicSpec{PhysicalTopic: "orders-v2"},
		}
		headerWithoutValue.Spec.Filter = &kawav1alpha1.VirtualTopicFilter{Type: kawav1alpha1.FilterHeaderEquals, Header: "region"}
		Expect(invalid(headerWithoutValue)).To(ContainSubstring("header filters need header and value only"))

		badPhysicalName := &kawav1alpha1.VirtualTopic{
			ObjectMeta: v1.ObjectMeta{Namespace: ns, Name: "c"},
			Spec:       kawav1alpha1.VirtualTopicSpec{PhysicalTopic: "orders v2"},
		}
		Expect(invalid(badPhysicalName)).To(ContainSubstring("spec.physicalTopic"))

		dropNothing := transformedOrders(ns)
		dropNothing.Name = "d"
		dropNothing.Spec.Transformations = []kawav1alpha1.Transformation{{Type: kawav1alpha1.TransformationDropFields, Fields: []string{}}}
		Expect(invalid(dropNothing)).To(ContainSubstring("fields is required for dropFields"))

		renameWithFields := transformedOrders(ns)
		renameWithFields.Name = "e"
		renameWithFields.Spec.Transformations = []kawav1alpha1.Transformation{
			{Type: kawav1alpha1.TransformationRenameField, From: "a", To: "b", Fields: []string{"c"}},
		}
		Expect(invalid(renameWithFields)).To(ContainSubstring("allowed only there"))

		jsonWithRoot := transformedOrders(ns)
		jsonWithRoot.Name = "f"
		jsonWithRoot.Spec.ValueFormat = &kawav1alpha1.PayloadFormat{Type: "json", Root: "order"}
		Expect(invalid(jsonWithRoot)).To(ContainSubstring("root is allowed only for xml"))

		setHeaderWithoutHeader := transformedOrders(ns)
		setHeaderWithoutHeader.Name = "g"
		setHeaderWithoutHeader.Spec.Transformations = []kawav1alpha1.Transformation{
			{Type: kawav1alpha1.TransformationSetHeader, Value: &apiextensionsv1.JSON{Raw: []byte(`"x"`)}},
		}
		Expect(invalid(setHeaderWithoutHeader)).To(ContainSubstring("header is required for setHeader"))

		dropNoHeaders := transformedOrders(ns)
		dropNoHeaders.Name = "h"
		dropNoHeaders.Spec.Transformations = []kawav1alpha1.Transformation{{Type: kawav1alpha1.TransformationDropHeaders}}
		Expect(invalid(dropNoHeaders)).To(ContainSubstring("headers is required for dropHeaders"))
	})

	It("does not allow renaming the virtual topic", func() {
		// given
		newGateway()
		vt := &kawav1alpha1.VirtualTopic{
			ObjectMeta: v1.ObjectMeta{Namespace: ns, Name: "orders"},
			Spec:       kawav1alpha1.VirtualTopicSpec{PhysicalTopic: "orders-v2"},
		}
		Expect(k8sClient.Create(ctx, vt)).To(Succeed())

		// when / then: re-read before each attempt, the controller adds its finalizer concurrently
		Eventually(func() string {
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(vt), vt)).To(Succeed())
			vt.Spec.VirtualTopic = "orders.renamed"
			if err := k8sClient.Update(ctx, vt); err != nil {
				return err.Error()
			}
			return "the rename was accepted"
		}, eventually, poll).Should(ContainSubstring("virtualTopic is immutable"))
	})
})

// transformedOrders is a VirtualTopic "orders" turning Avro values into JSON, with a numeric
// default and a convert step without onDecodeError.
func transformedOrders(namespace string) *kawav1alpha1.VirtualTopic {
	vt := &kawav1alpha1.VirtualTopic{
		ObjectMeta: v1.ObjectMeta{Namespace: namespace, Name: "orders"},
		Spec:       kawav1alpha1.VirtualTopicSpec{PhysicalTopic: "orders-v2"},
	}
	vt.Spec.ValueFormat = &kawav1alpha1.PayloadFormat{Type: "avro"}
	vt.Spec.Transformations = []kawav1alpha1.Transformation{
		{Type: kawav1alpha1.TransformationDropFields, Fields: []string{"internal_risk_score"}},
		{Type: kawav1alpha1.TransformationRenameField, From: "order_id", To: "id"},
		{Type: kawav1alpha1.TransformationDefaultField, Field: "quantity", Value: &apiextensionsv1.JSON{Raw: []byte(`{"amount":1.0,"unit":"piece"}`)}},
		{Type: kawav1alpha1.TransformationConvert, Format: &kawav1alpha1.PayloadFormat{Type: "json"}},
	}
	return vt
}
