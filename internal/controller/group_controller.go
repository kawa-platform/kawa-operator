package controller

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kawav1alpha1 "github.com/kawa-platform/kawa-operator/api/v1alpha1"
	"github.com/kawa-platform/kawa-operator/internal/kawa"
)

// GroupFinalizer removes a Group's group from the gateway before the Group goes away.
const GroupFinalizer = "group.kawa-platform.github.io/finalizer"

// GroupReconciler makes the operator's gateway have each Group as an RBAC group through the
// gateway's admin API: it creates, updates (also when the group drifted, e.g. after an edit in kawa
// studio) and deletes the group. The gateway group is named after the Group. See [managed] for how it
// claims, adopts and releases a name.
//
// Every call is made as the operator itself: it logs in to the gateway with its client credentials.
type GroupReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Admin is the gateway's admin API; it reads the endpoint anew on every reconcile, so rotated
	// Secrets are picked up.
	Admin *kawa.Admin
	// NamespaceSelector limits the namespaces whose Groups are served; nil serves all.
	NamespaceSelector labels.Selector
	// ResyncInterval is how often a synced group is compared with the gateway again, to undo drift.
	ResyncInterval time.Duration
}

// +kubebuilder:rbac:groups=kawa-platform.github.io,resources=groups,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=kawa-platform.github.io,resources=groups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kawa-platform.github.io,resources=groups/finalizers,verbs=update
// +kubebuilder:rbac:groups=kawa-platform.github.io,resources=roles,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch

func (r *GroupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	return r.managed().Reconcile(ctx, req)
}

func (r *GroupReconciler) managed() *managed[*kawav1alpha1.Group, kawa.Group, kawa.ListedGroup] {
	return &managed[*kawav1alpha1.Group, kawa.Group, kawa.ListedGroup]{
		Client:            r.Client,
		admin:             r.Admin,
		namespaceSelector: r.NamespaceSelector,
		resyncInterval:    r.ResyncInterval,

		kind:      "Group",
		resource:  "group",
		finalizer: GroupFinalizer,
		newObject: func() *kawav1alpha1.Group { return &kawav1alpha1.Group{} },
		newList:   func() client.ObjectList { return &kawav1alpha1.GroupList{} },
		desired: func(_ context.Context, group *kawav1alpha1.Group) (kawa.Group, *outcome) {
			return kawa.Group{Clients: group.Spec.Clients, Roles: group.Spec.Roles}, nil
		},
		list:      (*kawa.Client).ListGroups,
		managedBy: func(listed kawa.ListedGroup) *kawa.ManagedBy { return listed.ManagedBy },
		inSync: func(_ *kawav1alpha1.Group, desired kawa.Group, listed kawa.ListedGroup) bool {
			return listed.Equal(desired)
		},
		write: func(ctx context.Context, kc *kawa.Client, group *kawav1alpha1.Group, desired kawa.Group, _ bool) error {
			return kc.PutGroup(ctx, group.GatewayName(), desired)
		},
		// The gateway only deletes a group without clients, so the clients are taken out first.
		remove: func(ctx context.Context, kc *kawa.Client, name string, listed kawa.ListedGroup) error {
			if len(listed.Clients) > 0 {
				if err := kc.PutGroup(ctx, name, kawa.Group{Roles: listed.Roles}); err != nil {
					return err
				}
			}
			return kc.DeleteGroup(ctx, name)
		},
		release: (*kawa.Client).ReleaseGroup,
	}
}

// SetupWithManager sets up the controller with the Manager. It also follows Roles: the gateway
// refuses a group with a role it does not have yet, so a Group is reconciled again whenever a Role
// it references changes, e.g. once the Role is synced.
func (r *GroupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &kawav1alpha1.Group{}, groupRoleIndex,
		func(obj client.Object) []string {
			return obj.(*kawav1alpha1.Group).Spec.Roles
		}); err != nil {
		return err
	}
	b, err := r.managed().controller(mgr)
	if err != nil {
		return err
	}
	return b.
		Watches(&kawav1alpha1.Role{}, handler.EnqueueRequestsFromMapFunc(r.referencing)).
		Complete(r)
}

// groupRoleIndex indexes Groups by the gateway roles they reference.
const groupRoleIndex = "spec.roles"

// referencing maps a Role to requests for the Groups, in any namespace, that reference its gateway role.
func (r *GroupReconciler) referencing(ctx context.Context, obj client.Object) []reconcile.Request {
	role, ok := obj.(*kawav1alpha1.Role)
	if !ok {
		return nil
	}
	var groups kawav1alpha1.GroupList
	if err := r.List(ctx, &groups, client.MatchingFields{groupRoleIndex: role.GatewayName()}); err != nil {
		logf.FromContext(ctx).Error(err, "Failed to list Groups referencing a role", "role", role.GatewayName())
		return nil
	}
	requests := make([]reconcile.Request, 0, len(groups.Items))
	for i := range groups.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&groups.Items[i])})
	}
	return requests
}
