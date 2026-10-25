package controller

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kawav1alpha1 "github.com/kawa-platform/kawa-operator/api/v1alpha1"
	"github.com/kawa-platform/kawa-operator/internal/kawa"
	"github.com/kawa-platform/kawa-operator/internal/kawa/kawatest"
)

const (
	eventually = 10 * time.Second
	poll       = 100 * time.Millisecond
)

// newNamespace creates a namespace of its own for one test.
func newNamespace() string {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "kawa-test-"}}
	Expect(k8sClient.Create(ctx, ns)).To(Succeed())
	return ns.Name
}

// excludedLabel on a namespace puts it outside the suite's namespace selector.
const excludedLabel = "kawa-platform.github.io/excluded"

var (
	gatewayMu sync.Mutex
	gateway   GatewayConfig
)

// currentEndpoint is the suite operator's gateway endpoint: the current spec's fake gateway.
func currentEndpoint() (kawa.Endpoint, error) {
	gatewayMu.Lock()
	defer gatewayMu.Unlock()
	return gateway.Endpoint()
}

// useGateway points the operator at a GatewayConfig for the current spec.
func useGateway(config GatewayConfig) {
	gatewayMu.Lock()
	defer gatewayMu.Unlock()
	gateway = config
}

// newGateway starts a fake admin API with the clients kawa-operator and kawa-studio, and points the
// operator at it with kawa-operator's secret in a file.
func newGateway() *kawatest.Gateway {
	fake := kawatest.NewGateway("kawa-operator", "s3cret", "kawa-studio", "studio-s3cret")
	DeferCleanup(fake.Close)
	useGateway(GatewayConfig{AdminURL: fake.URL, ClientID: "kawa-operator", ClientSecretFile: secretFile("s3cret")})
	return fake
}

// exclude puts namespace outside the suite's namespace selector, or back inside it.
func exclude(namespace string, excluded bool) {
	ns := &corev1.Namespace{}
	Expect(k8sClient.Get(ctx, client.ObjectKey{Name: namespace}, ns)).To(Succeed())
	if excluded {
		if ns.Labels == nil {
			ns.Labels = map[string]string{}
		}
		ns.Labels[excludedLabel] = "true"
	} else {
		delete(ns.Labels, excludedLabel)
	}
	Expect(k8sClient.Update(ctx, ns)).To(Succeed())
}

// studio is an admin API client of gateway that authenticates as kawa-studio, not as the operator.
func studio(gateway *kawatest.Gateway) *kawa.Client {
	c, err := kawa.ClientFor(kawa.Endpoint{BaseURL: gateway.URL, ClientID: "kawa-studio", ClientSecret: "studio-s3cret"})
	Expect(err).NotTo(HaveOccurred())
	return c
}

// managedConflict is a failure unless err is the gateway's 409 for a change to the operator's
// kind name.
func managedConflict(err error, kind, name string) {
	Expect(kawa.StatusCode(err)).To(Equal(http.StatusConflict), fmt.Sprintf("the change was not refused: %v", err))
	Expect(err.Error()).To(ContainSubstring(fmt.Sprintf("%s '%s' is managed by kawa-operator", kind, name)))
}

// secretFile is a temporary file holding secret.
func secretFile(secret string) string {
	path := filepath.Join(GinkgoT().TempDir(), "client-secret")
	Expect(os.WriteFile(path, []byte(secret+"\n"), 0o600)).To(Succeed())
	return path
}

// readyCondition is obj's Ready condition, or nil when it has none yet.
func readyCondition(obj client.Object) *metav1.Condition {
	switch o := obj.(type) {
	case *kawav1alpha1.VirtualTopic:
		return meta.FindStatusCondition(o.Status.Conditions, ConditionReady)
	case *kawav1alpha1.Role:
		return meta.FindStatusCondition(o.Status.Conditions, ConditionReady)
	case *kawav1alpha1.Group:
		return meta.FindStatusCondition(o.Status.Conditions, ConditionReady)
	case *kawav1alpha1.Client:
		return meta.FindStatusCondition(o.Status.Conditions, ConditionReady)
	default:
		Fail(fmt.Sprintf("%T reports no Ready condition", obj))
		return nil
	}
}

// readyReason is the reason of obj's Ready condition, or "" when it has none yet.
func readyReason(obj client.Object) func() string {
	return func() string {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
			return "get failed: " + err.Error()
		}
		if c := readyCondition(obj); c != nil {
			return c.Reason
		}
		return ""
	}
}

// readyMessage is the message of obj's Ready condition.
func readyMessage(obj client.Object) string {
	Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), obj)).To(Succeed())
	if c := readyCondition(obj); c != nil {
		return c.Message
	}
	return ""
}

// gone is whether obj no longer exists.
func gone(obj client.Object) func() bool {
	return func() bool {
		return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), obj))
	}
}

// ordersPhysicalTopic is the physical topic of the gateway's virtual topic "orders", or "" when absent.
func ordersPhysicalTopic(gateway *kawatest.Gateway) func() string {
	return func() string {
		topic, _ := gateway.Virtual("orders")
		return topic.Topic
	}
}

// invalid is the API server's rejection of creating obj, or a failure when it was accepted.
func invalid(obj client.Object) string {
	err := k8sClient.Create(ctx, obj)
	Expect(err).To(HaveOccurred(), fmt.Sprintf("the API server accepted the invalid %T", obj))
	return strings.TrimSpace(err.Error())
}

// removeAll deletes every object of list's kind, stripping its finalizer first, and waits until
// they are gone.
func removeAll(list client.ObjectList) {
	Expect(k8sClient.List(ctx, list)).To(Succeed())
	items, err := meta.ExtractList(list)
	Expect(err).NotTo(HaveOccurred())
	for _, item := range items {
		obj := item.(client.Object)
		Eventually(func() error {
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
				return client.IgnoreNotFound(err)
			}
			obj.SetFinalizers(nil)
			if err := k8sClient.Update(ctx, obj); err != nil {
				return err
			}
			return client.IgnoreNotFound(k8sClient.Delete(ctx, obj))
		}, eventually, poll).Should(Succeed())
		Eventually(gone(obj), eventually, poll).Should(BeTrue())
	}
}
