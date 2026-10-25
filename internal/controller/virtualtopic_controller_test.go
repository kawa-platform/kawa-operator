package controller

import (
	"net/http"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
