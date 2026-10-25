package controller

import (
	"net/http"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kawav1alpha1 "github.com/kawa-platform/kawa-operator/api/v1alpha1"
	"github.com/kawa-platform/kawa-operator/internal/kawa"
	"github.com/kawa-platform/kawa-operator/internal/kawa/kawatest"
)

var _ = Describe("Group controller", func() {
	var ns string

	BeforeEach(func() {
		ns = newNamespace()
	})

	// Group names are unique on the gateway, and every spec uses "producers": remove the spec's
	// Groups, without their finalizers so that this does not depend on the spec's gateway.
	AfterEach(func() {
		removeAll(&kawav1alpha1.GroupList{})
	})

	It("creates the group on the gateway and reports it synced", func() {
		// given
		gateway := newGateway()
		group := producers(ns)

		// when
		Expect(k8sClient.Create(ctx, group)).To(Succeed())

		// then
		Eventually(readyReason(group), eventually, poll).Should(Equal(ReasonSynced))
		gatewayGroup, ok := gateway.Group("producers")
		Expect(ok).To(BeTrue(), "the group was not created under metadata.name")
		Expect(gatewayGroup).To(Equal(kawa.Group{Clients: []string{"alice", "bob"}, Roles: []string{"orders-producer"}}))
		Expect(group.Status.Group).To(Equal("producers"), "status.group does not name the managed group")
		Expect(group.Finalizers).To(ContainElement(GroupFinalizer))
	})

	It("updates the group when the spec changes", func() {
		// given
		gateway := newGateway()
		group := producers(ns)
		Expect(k8sClient.Create(ctx, group)).To(Succeed())
		Eventually(readyReason(group), eventually, poll).Should(Equal(ReasonSynced))

		// when
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(group), group)).To(Succeed())
		group.Spec.Clients = []string{"carol"}
		group.Spec.Roles = []string{"orders-producer", "orders-reader"}
		Expect(k8sClient.Update(ctx, group)).To(Succeed())

		// then
		Eventually(producersGroup(gateway), eventually, poll).Should(Equal(
			kawa.Group{Clients: []string{"carol"}, Roles: []string{"orders-producer", "orders-reader"}}))
	})

	It("undoes changes made to the group outside the operator", func() {
		// given
		gateway := newGateway()
		group := producers(ns)
		Expect(k8sClient.Create(ctx, group)).To(Succeed())
		Eventually(readyReason(group), eventually, poll).Should(Equal(ReasonSynced))
		want := producersGroup(gateway)()

		// when: someone adds a client in kawa studio
		gateway.SetGroup("producers", kawa.Group{Clients: append(want.Clients, "mallory"), Roles: want.Roles})

		// then
		Eventually(producersGroup(gateway), eventually, poll).Should(Equal(want))
	})

	It("does not rewrite a group that is already in sync", func() {
		// given: no clients, which the gateway lists as an empty list
		gateway := newGateway()
		group := producers(ns)
		group.Spec.Clients = nil
		Expect(k8sClient.Create(ctx, group)).To(Succeed())
		Eventually(readyReason(group), eventually, poll).Should(Equal(ReasonSynced))

		// when: several resyncs pass
		time.Sleep(3 * testResyncInterval)

		// then
		Expect(gateway.GroupWrites("producers")).To(Equal(1), "an in-sync group was written again on resync")
	})

	It("writes an unmarked group once so that the gateway marks it, and then leaves it alone", func() {
		// given: a synced group the gateway lists unmarked, as one written before it marked resources
		gateway := newGateway()
		group := producers(ns)
		Expect(k8sClient.Create(ctx, group)).To(Succeed())
		Eventually(readyReason(group), eventually, poll).Should(Equal(ReasonSynced))
		gateway.SetGroup("producers", producersGroup(gateway)())

		// when: several resyncs pass
		Eventually(func() string { return gateway.GroupManagedBy("producers") }, eventually, poll).
			Should(Equal(kawatest.OperatorPrincipal), "the unmarked group was not written again")
		time.Sleep(3 * testResyncInterval)

		// then
		Expect(gateway.GroupWrites("producers")).To(Equal(2), "the marked group was written again on resync")
	})

	It("lets nobody but the operator change the group it manages, also through a client", func() {
		// given
		gateway := newGateway()
		group := producers(ns)
		Expect(k8sClient.Create(ctx, group)).To(Succeed())
		Eventually(readyReason(group), eventually, poll).Should(Equal(ReasonSynced))
		want := producersGroup(gateway)()

		// when: someone edits it in kawa studio, directly and by creating a client in it
		direct := studio(gateway).PutGroup(ctx, "producers", kawa.Group{})
		cascade := studio(gateway).PutAuthClient(ctx, "carol", kawa.AuthClient{Mechanism: "SCRAM-SHA-512"}, "pw", []string{"producers"})

		// then
		managedConflict(direct, "group", "producers")
		managedConflict(cascade, "group", "producers")
		Expect(producersGroup(gateway)()).To(Equal(want))
	})

	It("refuses a group that already exists on the gateway, and adopts it when annotated", func() {
		// given
		gateway := newGateway()
		made := kawa.Group{Clients: []string{"studio"}, Roles: []string{"admin"}}
		gateway.SetGroup("producers", made)
		group := producers(ns)
		Expect(k8sClient.Create(ctx, group)).To(Succeed())
		Eventually(readyReason(group), eventually, poll).Should(Equal(ReasonAlreadyExists))
		Expect(producersGroup(gateway)()).To(Equal(made), "an existing group was overwritten without kawa-platform.github.io/adopt")

		// when
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(group), group)).To(Succeed())
		group.Annotations = map[string]string{kawav1alpha1.AdoptAnnotation: "true"}
		Expect(k8sClient.Update(ctx, group)).To(Succeed())

		// then
		Eventually(readyReason(group), eventually, poll).Should(Equal(ReasonSynced))
		Expect(producersGroup(gateway)().Clients).To(Equal([]string{"alice", "bob"}))
	})

	It("lets only the oldest Group of any namespace manage a name", func() {
		// given
		gateway := newGateway()
		first := producers(ns)
		Expect(k8sClient.Create(ctx, first)).To(Succeed())
		Eventually(readyReason(first), eventually, poll).Should(Equal(ReasonSynced))
		want := producersGroup(gateway)()

		// when
		time.Sleep(time.Second) // creation timestamps have second precision
		second := producers(newNamespace())
		second.Spec.Clients = []string{"mallory"}
		Expect(k8sClient.Create(ctx, second)).To(Succeed())

		// then
		Eventually(readyReason(second), eventually, poll).Should(Equal(ReasonConflict))
		Expect(readyMessage(second)).To(ContainSubstring(ns + "/producers"))
		Consistently(producersGroup(gateway), 2*testResyncInterval, poll).Should(Equal(want))
	})

	It("reports a group the gateway rejects as an invalid spec", func() {
		// given
		gateway := newGateway()
		gateway.RejectGroup("producers", http.StatusBadRequest, "invalid group body: unknown field")

		// when
		group := producers(ns)
		Expect(k8sClient.Create(ctx, group)).To(Succeed())

		// then
		Eventually(readyReason(group), eventually, poll).Should(Equal(ReasonInvalidSpec))
		Expect(readyMessage(group)).To(ContainSubstring("unknown field"))
	})

	It("takes the clients out of the group and deletes it from the gateway with the Group", func() {
		// given: the gateway refuses to delete a group that still has clients
		gateway := newGateway()
		group := producers(ns)
		Expect(k8sClient.Create(ctx, group)).To(Succeed())
		Eventually(readyReason(group), eventually, poll).Should(Equal(ReasonSynced))

		// when
		Expect(k8sClient.Delete(ctx, group)).To(Succeed())

		// then
		Eventually(gone(group), eventually, poll).Should(BeTrue())
		_, exists := gateway.Group("producers")
		Expect(exists).To(BeFalse(), "the group was left on the gateway")
	})

	It("keeps the group on the gateway with deletionPolicy Retain", func() {
		// given
		gateway := newGateway()
		group := producers(ns)
		group.Spec.DeletionPolicy = kawav1alpha1.DeletionPolicyRetain
		Expect(k8sClient.Create(ctx, group)).To(Succeed())
		Eventually(readyReason(group), eventually, poll).Should(Equal(ReasonSynced))

		// when
		Expect(k8sClient.Delete(ctx, group)).To(Succeed())

		// then
		Eventually(gone(group), eventually, poll).Should(BeTrue())
		retained, exists := gateway.Group("producers")
		Expect(exists).To(BeTrue(), "a retained group was deleted")
		Expect(retained.Clients).To(Equal([]string{"alice", "bob"}), "a retained group lost its clients")
		Expect(gateway.GroupManagedBy("producers")).To(BeEmpty(), "a retained group was not released")
		Expect(studio(gateway).PutGroup(ctx, "producers", kawa.Group{})).To(Succeed(), "a released group cannot be edited")
	})

	It("releases a Group whose group was already removed from the gateway, without recreating it", func() {
		// given
		gateway := newGateway()
		group := producers(ns)
		Expect(k8sClient.Create(ctx, group)).To(Succeed())
		Eventually(readyReason(group), eventually, poll).Should(Equal(ReasonSynced))
		gateway.RemoveGroup("producers")

		// when
		Expect(k8sClient.Delete(ctx, group)).To(Succeed())

		// then
		Eventually(gone(group), eventually, poll).Should(BeTrue())
		_, exists := gateway.Group("producers")
		Expect(exists).To(BeFalse(), "deleting the Group recreated the group")
	})

	It("acts as the operator on the gateway", func() {
		// given
		gateway := newGateway()

		// when
		group := producers(ns)
		Expect(k8sClient.Create(ctx, group)).To(Succeed())

		// then
		Eventually(readyReason(group), eventually, poll).Should(Equal(ReasonSynced))
		Expect(gateway.GroupWritesBy("producers")).To(Equal([]string{"kawa-operator"}), "the write was not made as the operator")
	})

	It("does not serve a namespace outside the namespace selector, until it matches", func() {
		// given
		gateway := newGateway()
		namespace := &corev1.Namespace{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: ns}, namespace)).To(Succeed())
		namespace.Labels[excludedLabel] = "true"
		Expect(k8sClient.Update(ctx, namespace)).To(Succeed())
		group := producers(ns)
		Expect(k8sClient.Create(ctx, group)).To(Succeed())
		Eventually(readyReason(group), eventually, poll).Should(Equal(ReasonNamespaceNotAllowed))
		Expect(gateway.GroupWrites("producers")).To(BeZero())

		// when
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: ns}, namespace)).To(Succeed())
		delete(namespace.Labels, excludedLabel)
		Expect(k8sClient.Update(ctx, namespace)).To(Succeed())

		// then
		Eventually(readyReason(group), eventually, poll).Should(Equal(ReasonSynced))
	})

	It("rejects an invalid Group at admission", func() {
		duplicateClient := producers(ns)
		duplicateClient.Name = "a"
		duplicateClient.Spec.Clients = []string{"alice", "alice"}
		Expect(invalid(duplicateClient)).To(ContainSubstring("spec.clients"))

		duplicateRole := producers(ns)
		duplicateRole.Name = "b"
		duplicateRole.Spec.Roles = []string{"orders-producer", "orders-producer"}
		Expect(invalid(duplicateRole)).To(ContainSubstring("spec.roles"))
	})
})

// producers is a Group producers in ns whose clients alice and bob inherit the role orders-producer.
func producers(ns string) *kawav1alpha1.Group {
	return &kawav1alpha1.Group{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "producers"},
		Spec: kawav1alpha1.GroupSpec{
			Clients: []string{"alice", "bob"},
			Roles:   []string{"orders-producer"},
		},
	}
}

// producersGroup is the gateway's group producers, or the zero Group when absent.
func producersGroup(gateway *kawatest.Gateway) func() kawa.Group {
	return func() kawa.Group {
		group, _ := gateway.Group("producers")
		return group
	}
}
