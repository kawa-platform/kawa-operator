package controller

import (
	"context"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kawav1alpha1 "github.com/kawa-platform/kawa-operator/api/v1alpha1"
	"github.com/kawa-platform/kawa-operator/internal/kawa"
)

// ClientFinalizer removes a Client's client from the gateway before the Client goes away.
const ClientFinalizer = "client.kawa-platform.github.io/finalizer"

// passwordSecretIndex indexes Clients by the name of their password Secret.
const passwordSecretIndex = "passwordSecret"

// ClientReconciler makes the operator's gateway have each Client as a SASL client through the
// gateway's admin API: it creates, updates (also when the client drifted, e.g. after an edit in kawa
// studio) and deletes the client. The gateway client is named after the Client. See [managed] for how
// it claims, adopts and releases a name.
//
// The password comes from a Secret. The gateway never returns a password, so the operator cannot see
// a password changed outside it: it writes the password when the client is created, and again when
// the Secret or the key changes (status.passwordVersion).
//
// Which groups a client is in is the Groups' business: the operator keeps a client's groups when it
// writes the client.
//
// Every call is made as the operator itself: it logs in to the gateway with its client credentials.
type ClientReconciler struct {
	client.Client
	// APIReader reads Secrets straight from the API server, so the operator does not cache every
	// Secret's data.
	APIReader client.Reader
	Scheme    *runtime.Scheme
	// Admin is the gateway's admin API; it reads the endpoint anew on every reconcile, so rotated
	// Secrets are picked up.
	Admin *kawa.Admin
	// NamespaceSelector limits the namespaces whose Clients are served; nil serves all.
	NamespaceSelector labels.Selector
	// ResyncInterval is how often a synced client is compared with the gateway again, to undo drift.
	ResyncInterval time.Duration
}

// +kubebuilder:rbac:groups=kawa-platform.github.io,resources=clients,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=kawa-platform.github.io,resources=clients/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kawa-platform.github.io,resources=clients/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *ClientReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	return r.managed().Reconcile(ctx, req)
}

// desiredClient is the client a Client specifies, with its password and the version that identifies
// the password (see [ClientReconciler.password]).
type desiredClient struct {
	client          kawa.AuthClient
	password        string
	passwordVersion string
}

func (r *ClientReconciler) managed() *managed[*kawav1alpha1.Client, desiredClient, kawa.ListedAuthClient] {
	return &managed[*kawav1alpha1.Client, desiredClient, kawa.ListedAuthClient]{
		Client:            r.Client,
		admin:             r.Admin,
		namespaceSelector: r.NamespaceSelector,
		resyncInterval:    r.ResyncInterval,

		kind:      "Client",
		resource:  "client",
		finalizer: ClientFinalizer,
		newObject: func() *kawav1alpha1.Client { return &kawav1alpha1.Client{} },
		newList:   func() client.ObjectList { return &kawav1alpha1.ClientList{} },
		desired: func(ctx context.Context, c *kawav1alpha1.Client) (desiredClient, *outcome) {
			password, version, unavailable := r.password(ctx, c)
			if unavailable != nil {
				return desiredClient{}, unavailable
			}
			// The cache can lag behind the operator's own status write after it wrote the password,
			// and a stale version would make it write the password again.
			if c.Status.PasswordVersion != version {
				if err := r.refreshPasswordVersion(ctx, c); err != nil {
					o := failed(ReasonAPIUnavailable, err)
					return desiredClient{}, &o
				}
			}
			return desiredClient{kawa.AuthClient{Mechanism: string(c.Spec.Mechanism)}, password, version}, nil
		},
		list:      (*kawa.Client).ListAuthClients,
		managedBy: func(listed kawa.ListedAuthClient) *kawa.ManagedBy { return listed.ManagedBy },
		inSync: func(c *kawav1alpha1.Client, desired desiredClient, listed kawa.ListedAuthClient) bool {
			return listed.AuthClient == desired.client && c.Status.PasswordVersion == desired.passwordVersion
		},
		write: func(ctx context.Context, kc *kawa.Client, c *kawav1alpha1.Client, desired desiredClient, exists bool) error {
			var err error
			if exists {
				err = kc.PatchAuthClient(ctx, c.GatewayName(), desired.client, desired.password)
			} else {
				err = createAuthClient(ctx, kc, c.GatewayName(), desired.client, desired.password)
			}
			if err == nil {
				c.Status.PasswordVersion = desired.passwordVersion
			}
			return err
		},
		// Deleting a client also takes it out of every group that lists it.
		remove: func(ctx context.Context, kc *kawa.Client, name string, _ kawa.ListedAuthClient) error {
			return kc.DeleteAuthClient(ctx, name)
		},
		release: (*kawa.Client).ReleaseAuthClient,
	}
}

// createAuthClient creates the client name. It puts the client in the groups that already list it,
// such as a Group made before the Client: creating a client sets its groups.
func createAuthClient(ctx context.Context, kc *kawa.Client, name string, desired kawa.AuthClient, password string) error {
	groups, err := kc.ListGroups(ctx)
	if err != nil {
		return err
	}
	var memberships []string
	for groupName, group := range groups {
		if slices.Contains(group.Clients, name) {
			memberships = append(memberships, groupName)
		}
	}
	return kc.PutAuthClient(ctx, name, desired, password, memberships)
}

// password is the Client's password and the version that identifies it (the Secret's
// resourceVersion and the key), or the outcome when the Secret does not have it.
func (r *ClientReconciler) password(ctx context.Context, c *kawav1alpha1.Client) (string, string, *outcome) {
	ref := c.Spec.PasswordSecretRef
	unavailable := func(message string) (string, string, *outcome) {
		// No retry: the Secret watch reconciles the Client when the Secret appears or changes.
		return "", "", &outcome{status: metav1.ConditionFalse, reason: ReasonSecretUnavailable, message: message}
	}
	var secret corev1.Secret
	err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: c.Namespace, Name: ref.Name}, &secret)
	switch {
	case apierrors.IsNotFound(err):
		return unavailable(fmt.Sprintf("Secret %q not found", ref.Name))
	case err != nil:
		o := failed(ReasonAPIUnavailable, fmt.Errorf("reading Secret %q: %w", ref.Name, err))
		return "", "", &o
	}
	password, ok := secret.Data[ref.Key]
	if !ok || len(password) == 0 {
		return unavailable(fmt.Sprintf("Secret %q has no key %q", ref.Name, ref.Key))
	}
	return string(password), secret.ResourceVersion + "/" + ref.Key, nil
}

// refreshPasswordVersion reads c's status.passwordVersion from the API server instead of the cache.
func (r *ClientReconciler) refreshPasswordVersion(ctx context.Context, c *kawav1alpha1.Client) error {
	var live kawav1alpha1.Client
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(c), &live); err != nil {
		return client.IgnoreNotFound(err)
	}
	c.Status.PasswordVersion = live.Status.PasswordVersion
	return nil
}

// usingSecret maps a Secret to requests for the Clients in its namespace that take their password
// from it.
func (r *ClientReconciler) usingSecret(ctx context.Context, secret client.Object) []reconcile.Request {
	var clients kawav1alpha1.ClientList
	if err := r.List(ctx, &clients, client.InNamespace(secret.GetNamespace()),
		client.MatchingFields{passwordSecretIndex: secret.GetName()}); err != nil {
		logf.FromContext(ctx).Error(err, "Failed to list Clients for Secret", "secret", secret.GetName())
		return nil
	}
	requests := make([]reconcile.Request, 0, len(clients.Items))
	for _, c := range clients.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&c)})
	}
	return requests
}

// SetupWithManager sets up the controller with the Manager. Secrets are watched by metadata only, so
// their data is not cached.
func (r *ClientReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &kawav1alpha1.Client{}, passwordSecretIndex,
		func(obj client.Object) []string {
			return []string{obj.(*kawav1alpha1.Client).Spec.PasswordSecretRef.Name}
		}); err != nil {
		return err
	}
	b, err := r.managed().controller(mgr)
	if err != nil {
		return err
	}
	return b.
		WatchesMetadata(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.usingSecret)).
		Complete(r)
}
