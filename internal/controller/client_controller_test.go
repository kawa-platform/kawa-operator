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

var _ = Describe("Client controller", func() {
	var ns string

	BeforeEach(func() {
		ns = newNamespace()
	})

	// Client names are unique on the gateway, and every spec uses "alice": remove the spec's Clients,
	// without their finalizers so that this does not depend on the spec's gateway.
	AfterEach(func() {
		removeAll(&kawav1alpha1.ClientList{})
	})

	It("creates the client with the password from its Secret and reports it synced", func() {
		// given
		gateway := newGateway()
		createPasswordSecret(ns, "s3cret")
		c := alice(ns)

		// when
		Expect(k8sClient.Create(ctx, c)).To(Succeed())

		// then
		Eventually(readyReason(c), eventually, poll).Should(Equal(ReasonSynced))
		Expect(aliceClient(gateway)()).To(Equal(kawatest.AuthClient{Mechanism: "SCRAM-SHA-512", Password: "s3cret"}))
		Expect(c.Status.Client).To(Equal("alice"), "status.client does not name the managed client")
		Expect(c.Finalizers).To(ContainElement(ClientFinalizer))
	})

	It("writes the password again when the Secret changes", func() {
		// given
		gateway := newGateway()
		secret := createPasswordSecret(ns, "s3cret")
		c := alice(ns)
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		Eventually(readyReason(c), eventually, poll).Should(Equal(ReasonSynced))

		// when
		secret.Data["password"] = []byte("rotated")
		Expect(k8sClient.Update(ctx, secret)).To(Succeed())

		// then: well before the next resync
		Eventually(alicePassword(gateway), testResyncInterval/2, poll).Should(Equal("rotated"))
	})

	It("updates the mechanism when the spec changes", func() {
		// given
		gateway := newGateway()
		createPasswordSecret(ns, "s3cret")
		c := alice(ns)
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		Eventually(readyReason(c), eventually, poll).Should(Equal(ReasonSynced))

		// when
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(c), c)).To(Succeed())
		c.Spec.Mechanism = "PLAIN"
		Expect(k8sClient.Update(ctx, c)).To(Succeed())

		// then
		Eventually(aliceClient(gateway), eventually, poll).Should(Equal(kawatest.AuthClient{Mechanism: "PLAIN", Password: "s3cret"}))
	})

	It("recreates a client removed outside the operator", func() {
		// given
		gateway := newGateway()
		createPasswordSecret(ns, "s3cret")
		c := alice(ns)
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		Eventually(readyReason(c), eventually, poll).Should(Equal(ReasonSynced))

		// when: someone deletes the client in kawa studio
		gateway.RemoveAuthClient("alice")

		// then
		Eventually(alicePassword(gateway), eventually, poll).Should(Equal("s3cret"))
	})

	It("does not rewrite a client that is already in sync", func() {
		// given
		gateway := newGateway()
		createPasswordSecret(ns, "s3cret")
		c := alice(ns)
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		Eventually(readyReason(c), eventually, poll).Should(Equal(ReasonSynced))

		// when: several resyncs pass
		time.Sleep(3 * testResyncInterval)

		// then
		Expect(gateway.AuthClientWrites("alice")).To(Equal(1), "an in-sync client was written again on resync")
	})

	It("writes an unmarked client once so that the gateway marks it, and then leaves it alone", func() {
		// given: a synced client the gateway lists unmarked, as one written before it marked resources
		gateway := newGateway()
		createPasswordSecret(ns, "s3cret")
		c := alice(ns)
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		Eventually(readyReason(c), eventually, poll).Should(Equal(ReasonSynced))
		gateway.SetAuthClient("alice", aliceClient(gateway)())

		// when: several resyncs pass
		Eventually(func() string { return gateway.AuthClientManagedBy("alice") }, eventually, poll).
			Should(Equal(kawatest.OperatorPrincipal), "the unmarked client was not written again")
		time.Sleep(3 * testResyncInterval)

		// then
		Expect(gateway.AuthClientWrites("alice")).To(Equal(2), "the marked client was written again on resync")
	})

	It("lets nobody but the operator change the client it manages", func() {
		// given
		gateway := newGateway()
		createPasswordSecret(ns, "s3cret")
		c := alice(ns)
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		Eventually(readyReason(c), eventually, poll).Should(Equal(ReasonSynced))

		// when: someone resets its password in kawa studio, or deletes it
		reset := studio(gateway).PatchAuthClient(ctx, "alice", kawa.AuthClient{Mechanism: "SCRAM-SHA-512"}, "other")
		deleted := studio(gateway).DeleteAuthClient(ctx, "alice")

		// then
		managedConflict(reset, "client", "alice")
		managedConflict(deleted, "client", "alice")
		Expect(alicePassword(gateway)()).To(Equal("s3cret"))
	})

	It("keeps the groups that list the client, when it creates the client and when it writes it again", func() {
		// given: a group made before the client
		gateway := newGateway()
		gateway.SetGroup("producers", kawa.Group{Clients: []string{"alice"}, Roles: []string{"orders-producer"}})
		secret := createPasswordSecret(ns, "s3cret")
		c := alice(ns)
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		Eventually(readyReason(c), eventually, poll).Should(Equal(ReasonSynced))
		Expect(producersGroup(gateway)().Clients).To(Equal([]string{"alice"}), "creating the client took it out of its group")

		// when
		secret.Data["password"] = []byte("rotated")
		Expect(k8sClient.Update(ctx, secret)).To(Succeed())
		Eventually(alicePassword(gateway), eventually, poll).Should(Equal("rotated"))

		// then
		Expect(producersGroup(gateway)().Clients).To(Equal([]string{"alice"}), "writing the client took it out of its group")
	})

	It("reports a missing Secret, and creates the client once the Secret exists", func() {
		// given
		gateway := newGateway()
		c := alice(ns)
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		Eventually(readyReason(c), eventually, poll).Should(Equal(ReasonSecretUnavailable))
		Expect(readyMessage(c)).To(ContainSubstring(`Secret "alice-password" not found`))
		Expect(gateway.AuthClientWrites("alice")).To(BeZero())

		// when
		createPasswordSecret(ns, "s3cret")

		// then: well before the next resync
		Eventually(readyReason(c), testResyncInterval/2, poll).Should(Equal(ReasonSynced))
		Expect(alicePassword(gateway)()).To(Equal("s3cret"))
	})

	It("reports a Secret without the key", func() {
		// given
		newGateway()
		createPasswordSecret(ns, "s3cret")
		c := alice(ns)
		c.Spec.PasswordSecretRef.Key = "other"

		// when
		Expect(k8sClient.Create(ctx, c)).To(Succeed())

		// then
		Eventually(readyReason(c), eventually, poll).Should(Equal(ReasonSecretUnavailable))
		Expect(readyMessage(c)).To(ContainSubstring(`has no key "other"`))
	})

	It("refuses a client that already exists on the gateway, and adopts it when annotated", func() {
		// given
		gateway := newGateway()
		made := kawatest.AuthClient{Mechanism: "PLAIN", Password: "studio"}
		gateway.SetAuthClient("alice", made)
		createPasswordSecret(ns, "s3cret")
		c := alice(ns)
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		Eventually(readyReason(c), eventually, poll).Should(Equal(ReasonAlreadyExists))
		Expect(aliceClient(gateway)()).To(Equal(made), "an existing client was overwritten without kawa-platform.github.io/adopt")

		// when
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(c), c)).To(Succeed())
		c.Annotations = map[string]string{kawav1alpha1.AdoptAnnotation: "true"}
		Expect(k8sClient.Update(ctx, c)).To(Succeed())

		// then: the adopted client gets the Secret's password, even with an unchanged Secret
		Eventually(readyReason(c), eventually, poll).Should(Equal(ReasonSynced))
		Expect(aliceClient(gateway)()).To(Equal(kawatest.AuthClient{Mechanism: "SCRAM-SHA-512", Password: "s3cret"}))
	})

	It("lets only the oldest Client of any namespace manage a name", func() {
		// given
		gateway := newGateway()
		createPasswordSecret(ns, "s3cret")
		first := alice(ns)
		Expect(k8sClient.Create(ctx, first)).To(Succeed())
		Eventually(readyReason(first), eventually, poll).Should(Equal(ReasonSynced))

		// when
		time.Sleep(time.Second) // creation timestamps have second precision
		otherNs := newNamespace()
		createPasswordSecret(otherNs, "mallory")
		second := alice(otherNs)
		Expect(k8sClient.Create(ctx, second)).To(Succeed())

		// then
		Eventually(readyReason(second), eventually, poll).Should(Equal(ReasonConflict))
		Expect(readyMessage(second)).To(ContainSubstring(ns + "/alice"))
		Consistently(alicePassword(gateway), 2*testResyncInterval, poll).Should(Equal("s3cret"))
	})

	It("reports a client the gateway rejects as an invalid spec", func() {
		// given
		gateway := newGateway()
		gateway.RejectAuthClient("alice", http.StatusBadRequest, "unknown mechanism: SCRAM-SHA-512")
		createPasswordSecret(ns, "s3cret")

		// when
		c := alice(ns)
		Expect(k8sClient.Create(ctx, c)).To(Succeed())

		// then
		Eventually(readyReason(c), eventually, poll).Should(Equal(ReasonInvalidSpec))
		Expect(readyMessage(c)).To(ContainSubstring("unknown mechanism"))
	})

	It("deletes the client from the gateway with the Client", func() {
		// given
		gateway := newGateway()
		createPasswordSecret(ns, "s3cret")
		c := alice(ns)
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		Eventually(readyReason(c), eventually, poll).Should(Equal(ReasonSynced))

		// when
		Expect(k8sClient.Delete(ctx, c)).To(Succeed())

		// then
		Eventually(gone(c), eventually, poll).Should(BeTrue())
		_, exists := gateway.AuthClient("alice")
		Expect(exists).To(BeFalse(), "the client was left on the gateway")
	})

	It("keeps the client on the gateway with deletionPolicy Retain", func() {
		// given
		gateway := newGateway()
		createPasswordSecret(ns, "s3cret")
		c := alice(ns)
		c.Spec.DeletionPolicy = kawav1alpha1.DeletionPolicyRetain
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		Eventually(readyReason(c), eventually, poll).Should(Equal(ReasonSynced))

		// when
		Expect(k8sClient.Delete(ctx, c)).To(Succeed())

		// then
		Eventually(gone(c), eventually, poll).Should(BeTrue())
		_, exists := gateway.AuthClient("alice")
		Expect(exists).To(BeTrue(), "a retained client was deleted")
		Expect(gateway.AuthClientManagedBy("alice")).To(BeEmpty(), "a retained client was not released")
		Expect(studio(gateway).DeleteAuthClient(ctx, "alice")).To(Succeed(), "a released client cannot be deleted")
	})

	It("releases a Client whose client was already removed from the gateway", func() {
		// given
		gateway := newGateway()
		createPasswordSecret(ns, "s3cret")
		c := alice(ns)
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		Eventually(readyReason(c), eventually, poll).Should(Equal(ReasonSynced))
		gateway.RemoveAuthClient("alice")

		// when
		Expect(k8sClient.Delete(ctx, c)).To(Succeed())

		// then
		Eventually(gone(c), eventually, poll).Should(BeTrue())
	})

	It("acts as the operator on the gateway", func() {
		// given
		gateway := newGateway()
		createPasswordSecret(ns, "s3cret")

		// when
		c := alice(ns)
		Expect(k8sClient.Create(ctx, c)).To(Succeed())

		// then
		Eventually(readyReason(c), eventually, poll).Should(Equal(ReasonSynced))
		Expect(gateway.AuthClientWritesBy("alice")).To(Equal([]string{"kawa-operator"}), "the write was not made as the operator")
	})

	It("does not serve a namespace outside the namespace selector, until it matches", func() {
		// given
		gateway := newGateway()
		namespace := &corev1.Namespace{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: ns}, namespace)).To(Succeed())
		namespace.Labels[excludedLabel] = "true"
		Expect(k8sClient.Update(ctx, namespace)).To(Succeed())
		createPasswordSecret(ns, "s3cret")
		c := alice(ns)
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		Eventually(readyReason(c), eventually, poll).Should(Equal(ReasonNamespaceNotAllowed))
		Expect(gateway.AuthClientWrites("alice")).To(BeZero())

		// when
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: ns}, namespace)).To(Succeed())
		delete(namespace.Labels, excludedLabel)
		Expect(k8sClient.Update(ctx, namespace)).To(Succeed())

		// then
		Eventually(readyReason(c), eventually, poll).Should(Equal(ReasonSynced))
	})

	It("rejects an invalid Client at admission", func() {
		unknownMechanism := alice(ns)
		unknownMechanism.Name = "a"
		unknownMechanism.Spec.Mechanism = "GSSAPI"
		Expect(invalid(unknownMechanism)).To(ContainSubstring("spec.mechanism"))

		noSecretKey := alice(ns)
		noSecretKey.Name = "b"
		noSecretKey.Spec.PasswordSecretRef.Key = ""
		Expect(invalid(noSecretKey)).To(ContainSubstring("spec.passwordSecretRef.key"))
	})
})

// alice is a Client alice in ns that authenticates with SCRAM-SHA-512 and the password in the key
// password of the Secret alice-password.
func alice(ns string) *kawav1alpha1.Client {
	return &kawav1alpha1.Client{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "alice"},
		Spec: kawav1alpha1.ClientSpec{
			Mechanism:         "SCRAM-SHA-512",
			PasswordSecretRef: kawav1alpha1.SecretKeyReference{Name: "alice-password", Key: "password"},
		},
	}
}

// createPasswordSecret creates the Secret alice-password in ns with password under the key password.
func createPasswordSecret(ns, password string) *corev1.Secret {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "alice-password"},
		Data:       map[string][]byte{"password": []byte(password)},
	}
	Expect(k8sClient.Create(ctx, secret)).To(Succeed())
	return secret
}

// aliceClient is the gateway's client alice, or the zero AuthClient when absent.
func aliceClient(gateway *kawatest.Gateway) func() kawatest.AuthClient {
	return func() kawatest.AuthClient {
		c, _ := gateway.AuthClient("alice")
		return c
	}
}

// alicePassword is the password of the gateway's client alice, or "" when absent.
func alicePassword(gateway *kawatest.Gateway) func() string {
	return func() string {
		return aliceClient(gateway)().Password
	}
}
