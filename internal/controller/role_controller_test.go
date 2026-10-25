package controller

import (
	"net/http"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kawav1alpha1 "github.com/kawa-platform/kawa-operator/api/v1alpha1"
	"github.com/kawa-platform/kawa-operator/internal/kawa"
	"github.com/kawa-platform/kawa-operator/internal/kawa/kawatest"
)

var _ = Describe("Role controller", func() {
	var ns string

	BeforeEach(func() {
		ns = newNamespace()
	})

	// Role names are unique on the gateway, and every spec uses "orders-reader": remove the spec's
	// Roles, without their finalizers so that this does not depend on the spec's gateway.
	AfterEach(func() {
		removeAll(&kawav1alpha1.RoleList{})
	})

	It("creates the role on the gateway and reports it synced", func() {
		// given: ACLs with different permissions on the same resource
		gateway := newGateway()
		role := ordersReader(ns)
		for i := range role.Spec.ACLs {
			role.Spec.ACLs[i].Resource.PatternType = "PREFIXED"
		}
		role.Spec.ACLs[1].Permission = "DENY"

		// when
		Expect(k8sClient.Create(ctx, role)).To(Succeed())

		// then
		Eventually(readyReason(role), eventually, poll).Should(Equal(ReasonSynced))
		gatewayRole, ok := gateway.Role("orders-reader")
		Expect(ok).To(BeTrue(), "the role was not created under metadata.name")
		resource := kawa.Resource{Type: "TOPIC", Pattern: ptr.To("orders"), PatternType: "PREFIXED"}
		Expect(gatewayRole.ACLs).To(Equal([]kawa.ACL{
			{Resource: resource, Operation: "READ", Permission: "ALLOW"},
			{Resource: resource, Operation: "DESCRIBE", Permission: "DENY"},
		}), "the gateway role does not have the spec's ACLs")
		Expect(role.Status.Role).To(Equal("orders-reader"), "status.role does not name the managed role")
		Expect(role.Finalizers).To(ContainElement(RoleFinalizer))
	})

	It("updates the role when the spec changes", func() {
		// given
		gateway := newGateway()
		role := ordersReader(ns)
		Expect(k8sClient.Create(ctx, role)).To(Succeed())
		Eventually(readyReason(role), eventually, poll).Should(Equal(ReasonSynced))

		// when
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(role), role)).To(Succeed())
		role.Spec.ACLs = []kawav1alpha1.ACL{{
			Resource:  kawav1alpha1.Resource{Type: "TOPIC", Pattern: ptr.To("orders")},
			Operation: "WRITE", Permission: "DENY",
		}}
		Expect(k8sClient.Update(ctx, role)).To(Succeed())

		// then
		Eventually(ordersReaderACLs(gateway), eventually, poll).Should(Equal([]kawa.ACL{{
			Resource:  kawa.Resource{Type: "TOPIC", Pattern: ptr.To("orders"), PatternType: "LITERAL"},
			Operation: "WRITE", Permission: "DENY",
		}}))
	})

	It("undoes changes made to the role outside the operator", func() {
		// given
		gateway := newGateway()
		role := ordersReader(ns)
		Expect(k8sClient.Create(ctx, role)).To(Succeed())
		Eventually(readyReason(role), eventually, poll).Should(Equal(ReasonSynced))
		want := ordersReaderACLs(gateway)()

		// when: someone grants more in kawa studio
		gateway.SetRole("orders-reader", kawa.Role{ACLs: append(want, kawa.ACL{
			Resource: kawa.Resource{Type: "CLUSTER", PatternType: "LITERAL"}, Operation: "ALL", Permission: "ALLOW",
		})})

		// then
		Eventually(ordersReaderACLs(gateway), eventually, poll).Should(Equal(want))
	})

	It("does not rewrite a role that is already in sync", func() {
		// given: a CLUSTER resource, which the gateway stores without pattern, with the default patternType
		gateway := newGateway()
		role := &kawav1alpha1.Role{
			ObjectMeta: v1.ObjectMeta{Namespace: ns, Name: "orders-reader"},
			Spec: kawav1alpha1.RoleSpec{
				ACLs: []kawav1alpha1.ACL{{
					Resource: kawav1alpha1.Resource{Type: "CLUSTER"}, Operation: "DESCRIBE", Permission: "ALLOW",
				}},
			},
		}
		Expect(k8sClient.Create(ctx, role)).To(Succeed())
		Eventually(readyReason(role), eventually, poll).Should(Equal(ReasonSynced))

		// when: several resyncs pass
		time.Sleep(3 * testResyncInterval)

		// then
		Expect(gateway.RoleWrites("orders-reader")).To(Equal(1), "an in-sync role was written again on resync")
	})

	It("writes an unmarked role once so that the gateway marks it, and then leaves it alone", func() {
		// given: a synced role the gateway lists unmarked, as one written before it marked resources
		gateway := newGateway()
		role := ordersReader(ns)
		Expect(k8sClient.Create(ctx, role)).To(Succeed())
		Eventually(readyReason(role), eventually, poll).Should(Equal(ReasonSynced))
		gateway.SetRole("orders-reader", kawa.Role{ACLs: ordersReaderACLs(gateway)()})

		// when: several resyncs pass
		Eventually(func() string { return gateway.RoleManagedBy("orders-reader") }, eventually, poll).
			Should(Equal(kawatest.OperatorPrincipal), "the unmarked role was not written again")
		time.Sleep(3 * testResyncInterval)

		// then
		Expect(gateway.RoleWrites("orders-reader")).To(Equal(2), "the marked role was written again on resync")
	})

	It("lets nobody but the operator change the role it manages", func() {
		// given
		gateway := newGateway()
		role := ordersReader(ns)
		Expect(k8sClient.Create(ctx, role)).To(Succeed())
		Eventually(readyReason(role), eventually, poll).Should(Equal(ReasonSynced))
		want := ordersReaderACLs(gateway)()

		// when: someone edits it in kawa studio
		err := studio(gateway).PutRole(ctx, "orders-reader", kawa.Role{})

		// then
		managedConflict(err, "role", "orders-reader")
		Expect(ordersReaderACLs(gateway)()).To(Equal(want))
	})

	It("refuses a role that already exists on the gateway, and adopts it when annotated", func() {
		// given
		gateway := newGateway()
		made := []kawa.ACL{{
			Resource:  kawa.Resource{Type: "GROUP", Pattern: ptr.To("studio"), PatternType: "LITERAL"},
			Operation: "READ", Permission: "ALLOW",
		}}
		gateway.SetRole("orders-reader", kawa.Role{ACLs: made})
		role := ordersReader(ns)
		Expect(k8sClient.Create(ctx, role)).To(Succeed())
		Eventually(readyReason(role), eventually, poll).Should(Equal(ReasonAlreadyExists))
		Expect(ordersReaderACLs(gateway)()).To(Equal(made), "an existing role was overwritten without kawa-platform.github.io/adopt")

		// when
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(role), role)).To(Succeed())
		role.Annotations = map[string]string{kawav1alpha1.AdoptAnnotation: "true"}
		Expect(k8sClient.Update(ctx, role)).To(Succeed())

		// then
		Eventually(readyReason(role), eventually, poll).Should(Equal(ReasonSynced))
		Expect(ordersReaderACLs(gateway)()).To(HaveLen(2))
	})

	It("lets only the oldest Role of any namespace manage a name", func() {
		// given
		gateway := newGateway()
		first := ordersReader(ns)
		Expect(k8sClient.Create(ctx, first)).To(Succeed())
		Eventually(readyReason(first), eventually, poll).Should(Equal(ReasonSynced))
		want := ordersReaderACLs(gateway)()

		// when
		time.Sleep(time.Second) // creation timestamps have second precision
		second := ordersReader(newNamespace())
		second.Spec.ACLs[0].Operation = "ALL"
		Expect(k8sClient.Create(ctx, second)).To(Succeed())

		// then
		Eventually(readyReason(second), eventually, poll).Should(Equal(ReasonConflict))
		Expect(readyMessage(second)).To(ContainSubstring(ns + "/orders-reader"))
		Consistently(ordersReaderACLs(gateway), 2*testResyncInterval, poll).Should(Equal(want))
	})

	It("reports a role the gateway rejects as an invalid spec", func() {
		// given
		gateway := newGateway()
		gateway.RejectRole("orders-reader", http.StatusBadRequest, "invalid role body: unknown operation")

		// when
		role := ordersReader(ns)
		Expect(k8sClient.Create(ctx, role)).To(Succeed())

		// then
		Eventually(readyReason(role), eventually, poll).Should(Equal(ReasonInvalidSpec))
		Expect(readyMessage(role)).To(ContainSubstring("unknown operation"))
	})

	It("deletes the role from the gateway with the Role", func() {
		// given
		gateway := newGateway()
		role := ordersReader(ns)
		Expect(k8sClient.Create(ctx, role)).To(Succeed())
		Eventually(readyReason(role), eventually, poll).Should(Equal(ReasonSynced))

		// when
		Expect(k8sClient.Delete(ctx, role)).To(Succeed())

		// then
		Eventually(gone(role), eventually, poll).Should(BeTrue())
		_, exists := gateway.Role("orders-reader")
		Expect(exists).To(BeFalse(), "the role was left on the gateway")
	})

	It("keeps the role on the gateway with deletionPolicy Retain", func() {
		// given
		gateway := newGateway()
		role := ordersReader(ns)
		role.Spec.DeletionPolicy = kawav1alpha1.DeletionPolicyRetain
		Expect(k8sClient.Create(ctx, role)).To(Succeed())
		Eventually(readyReason(role), eventually, poll).Should(Equal(ReasonSynced))

		// when
		Expect(k8sClient.Delete(ctx, role)).To(Succeed())

		// then
		Eventually(gone(role), eventually, poll).Should(BeTrue())
		_, exists := gateway.Role("orders-reader")
		Expect(exists).To(BeTrue(), "a retained role was deleted")
		Expect(gateway.RoleManagedBy("orders-reader")).To(BeEmpty(), "a retained role was not released")
		Expect(studio(gateway).PutRole(ctx, "orders-reader", kawa.Role{})).To(Succeed(), "a released role cannot be edited")
	})

	It("releases a Role whose role was already removed from the gateway", func() {
		// given
		gateway := newGateway()
		role := ordersReader(ns)
		Expect(k8sClient.Create(ctx, role)).To(Succeed())
		Eventually(readyReason(role), eventually, poll).Should(Equal(ReasonSynced))
		gateway.RemoveRole("orders-reader")

		// when
		Expect(k8sClient.Delete(ctx, role)).To(Succeed())

		// then
		Eventually(gone(role), eventually, poll).Should(BeTrue())
	})

	It("takes the name over once the Role holding it is deleted", func() {
		// given
		gateway := newGateway()
		first := ordersReader(ns)
		Expect(k8sClient.Create(ctx, first)).To(Succeed())
		Eventually(readyReason(first), eventually, poll).Should(Equal(ReasonSynced))
		time.Sleep(time.Second) // creation timestamps have second precision
		second := ordersReader(newNamespace())
		second.Spec.ACLs[0].Operation = "ALL"
		Expect(k8sClient.Create(ctx, second)).To(Succeed())
		Eventually(readyReason(second), eventually, poll).Should(Equal(ReasonConflict))

		// when
		Expect(k8sClient.Delete(ctx, first)).To(Succeed())

		// then
		Eventually(readyReason(second), eventually, poll).Should(Equal(ReasonSynced))
		Expect(ordersReaderACLs(gateway)()[0].Operation).To(Equal("ALL"))
	})

	It("keeps the name with the Role holding it, against an older Role whose namespace becomes served", func() {
		// given: the older Role is outside the namespace selector, so the newer one syncs
		gateway := newGateway()
		older := ordersReader(newNamespace())
		exclude(older.Namespace, true)
		Expect(k8sClient.Create(ctx, older)).To(Succeed())
		Eventually(readyReason(older), eventually, poll).Should(Equal(ReasonNamespaceNotAllowed))
		time.Sleep(time.Second) // creation timestamps have second precision
		holder := ordersReader(ns)
		Expect(k8sClient.Create(ctx, holder)).To(Succeed())
		Eventually(readyReason(holder), eventually, poll).Should(Equal(ReasonSynced))
		want := ordersReaderACLs(gateway)()

		// when
		exclude(older.Namespace, false)

		// then
		Eventually(readyReason(older), eventually, poll).Should(Equal(ReasonConflict))
		Expect(readyMessage(older)).To(ContainSubstring(ns + "/orders-reader"))
		Consistently(readyReason(holder), 2*testResyncInterval, poll).Should(Equal(ReasonSynced))
		Expect(ordersReaderACLs(gateway)()).To(Equal(want))
	})

	It("reports a role another principal manages as a conflict, and syncs once it is released", func() {
		// given
		gateway := newGateway()
		gateway.RejectRole("orders-reader", http.StatusConflict, "role 'orders-reader' is managed by someone-else")

		// when
		role := ordersReader(ns)
		Expect(k8sClient.Create(ctx, role)).To(Succeed())

		// then
		Eventually(readyReason(role), eventually, poll).Should(Equal(ReasonConflict))
		Expect(readyMessage(role)).To(ContainSubstring("managed by someone-else"))
		gateway.RejectRole("orders-reader", 0, "")
		Eventually(readyReason(role), eventually, poll).Should(Equal(ReasonSynced))
	})

	It("keeps the Role while the gateway refuses to delete the role, and deletes it once allowed", func() {
		// given
		gateway := newGateway()
		role := ordersReader(ns)
		Expect(k8sClient.Create(ctx, role)).To(Succeed())
		Eventually(readyReason(role), eventually, poll).Should(Equal(ReasonSynced))
		gateway.RejectRole("orders-reader", http.StatusConflict, "role 'orders-reader' changed")

		// when
		Expect(k8sClient.Delete(ctx, role)).To(Succeed())

		// then
		Eventually(readyReason(role), eventually, poll).Should(Equal(ReasonDeletionRejected))
		gateway.RejectRole("orders-reader", 0, "")
		Eventually(gone(role), eventually, poll).Should(BeTrue())
		_, exists := gateway.Role("orders-reader")
		Expect(exists).To(BeFalse(), "the role was left on the gateway")
	})

	It("never deletes a role the operator does not manage", func() {
		// given: a synced Role, outside the namespace selector, whose role the gateway no longer marks as the operator's
		gateway := newGateway()
		role := ordersReader(ns)
		Expect(k8sClient.Create(ctx, role)).To(Succeed())
		Eventually(readyReason(role), eventually, poll).Should(Equal(ReasonSynced))
		exclude(ns, true)
		Eventually(readyReason(role), eventually, poll).Should(Equal(ReasonNamespaceNotAllowed))
		made := []kawa.ACL{{
			Resource:  kawa.Resource{Type: "GROUP", Pattern: ptr.To("studio"), PatternType: "LITERAL"},
			Operation: "READ", Permission: "ALLOW",
		}}
		gateway.SetRole("orders-reader", kawa.Role{ACLs: made})

		// when
		Expect(k8sClient.Delete(ctx, role)).To(Succeed())

		// then
		Eventually(gone(role), eventually, poll).Should(BeTrue())
		Expect(ordersReaderACLs(gateway)()).To(Equal(made), "a role the operator does not manage was deleted")
	})

	It("acts as the operator on the gateway", func() {
		// given
		gateway := newGateway()

		// when
		role := ordersReader(ns)
		Expect(k8sClient.Create(ctx, role)).To(Succeed())

		// then
		Eventually(readyReason(role), eventually, poll).Should(Equal(ReasonSynced))
		Expect(gateway.RoleWritesBy("orders-reader")).To(Equal([]string{"kawa-operator"}), "the write was not made as the operator")
	})

	It("does not serve a namespace outside the namespace selector, until it matches", func() {
		// given
		gateway := newGateway()
		namespace := &corev1.Namespace{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: ns}, namespace)).To(Succeed())
		namespace.Labels[excludedLabel] = "true"
		Expect(k8sClient.Update(ctx, namespace)).To(Succeed())
		role := ordersReader(ns)
		Expect(k8sClient.Create(ctx, role)).To(Succeed())
		Eventually(readyReason(role), eventually, poll).Should(Equal(ReasonNamespaceNotAllowed))
		Expect(gateway.RoleWrites("orders-reader")).To(BeZero())

		// when
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: ns}, namespace)).To(Succeed())
		delete(namespace.Labels, excludedLabel)
		Expect(k8sClient.Update(ctx, namespace)).To(Succeed())

		// then
		Eventually(readyReason(role), eventually, poll).Should(Equal(ReasonSynced))
	})

	It("rejects an invalid Role at admission", func() {
		clusterWithPattern := ordersReader(ns)
		clusterWithPattern.Name = "a"
		clusterWithPattern.Spec.ACLs[0].Resource.Type = "CLUSTER"
		Expect(invalid(clusterWithPattern)).To(ContainSubstring("pattern must be unset for CLUSTER"))

		topicWithoutPattern := ordersReader(ns)
		topicWithoutPattern.Name = "b"
		topicWithoutPattern.Spec.ACLs[0].Resource.Pattern = nil
		Expect(invalid(topicWithoutPattern)).To(ContainSubstring("set for every other type"))

		emptyLiteralPattern := ordersReader(ns)
		emptyLiteralPattern.Name = "c"
		emptyLiteralPattern.Spec.ACLs[0].Resource.Pattern = ptr.To("")
		Expect(invalid(emptyLiteralPattern)).To(ContainSubstring("an empty pattern needs patternType PREFIXED"))

		noACLs := ordersReader(ns)
		noACLs.Name = "d"
		noACLs.Spec.ACLs = nil
		Expect(invalid(noACLs)).To(ContainSubstring("spec.acls"))
	})
})

// ordersReader is a Role orders-reader in ns that allows READ and DESCRIBE on the topic orders.
func ordersReader(ns string) *kawav1alpha1.Role {
	return &kawav1alpha1.Role{
		ObjectMeta: v1.ObjectMeta{Namespace: ns, Name: "orders-reader"},
		Spec: kawav1alpha1.RoleSpec{
			ACLs: []kawav1alpha1.ACL{
				{Resource: kawav1alpha1.Resource{Type: "TOPIC", Pattern: ptr.To("orders")}, Operation: "READ", Permission: "ALLOW"},
				{Resource: kawav1alpha1.Resource{Type: "TOPIC", Pattern: ptr.To("orders")}, Operation: "DESCRIBE", Permission: "ALLOW"},
			},
		},
	}
}

// ordersReaderACLs are the ACLs of the gateway's role orders-reader, or nil when absent.
func ordersReaderACLs(gateway *kawatest.Gateway) func() []kawa.ACL {
	return func() []kawa.ACL {
		role, _ := gateway.Role("orders-reader")
		return role.ACLs
	}
}
