package controller

import (
	"context"
	"reflect"
	"time"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kawav1alpha1 "github.com/kawa-platform/kawa-operator/api/v1alpha1"
	"github.com/kawa-platform/kawa-operator/internal/kawa"
)

// RoleFinalizer removes a Role's role from the gateway before the Role goes away.
const RoleFinalizer = "role.kawa-platform.github.io/finalizer"

// RoleReconciler makes the operator's gateway have each Role as an RBAC role through the gateway's
// admin API: it creates, updates (also when the role drifted, e.g. after an edit in kawa studio) and
// deletes the role. The gateway role is named after the Role. See [managed] for how it claims, adopts
// and releases a name.
//
// Every call is made as the operator itself: it logs in to the gateway with its client credentials.
type RoleReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Admin is the gateway's admin API; it reads the endpoint anew on every reconcile, so rotated
	// Secrets are picked up.
	Admin *kawa.Admin
	// NamespaceSelector limits the namespaces whose Roles are served; nil serves all.
	NamespaceSelector labels.Selector
	// ResyncInterval is how often a synced role is compared with the gateway again, to undo drift.
	ResyncInterval time.Duration
}

// +kubebuilder:rbac:groups=kawa-platform.github.io,resources=roles,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=kawa-platform.github.io,resources=roles/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kawa-platform.github.io,resources=roles/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch

func (r *RoleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	return r.managed().Reconcile(ctx, req)
}

func (r *RoleReconciler) managed() *managed[*kawav1alpha1.Role, kawa.Role, kawa.ListedRole] {
	return &managed[*kawav1alpha1.Role, kawa.Role, kawa.ListedRole]{
		Client:            r.Client,
		admin:             r.Admin,
		namespaceSelector: r.NamespaceSelector,
		resyncInterval:    r.ResyncInterval,

		kind:      "Role",
		resource:  "role",
		finalizer: RoleFinalizer,
		newObject: func() *kawav1alpha1.Role { return &kawav1alpha1.Role{} },
		newList:   func() client.ObjectList { return &kawav1alpha1.RoleList{} },
		desired: func(_ context.Context, role *kawav1alpha1.Role) (kawa.Role, *outcome) {
			return desiredRole(role), nil
		},
		list:      (*kawa.Client).ListRoles,
		managedBy: func(listed kawa.ListedRole) *kawa.ManagedBy { return listed.ManagedBy },
		inSync: func(_ *kawav1alpha1.Role, desired kawa.Role, listed kawa.ListedRole) bool {
			return reflect.DeepEqual(listed.Role, desired)
		},
		write: func(ctx context.Context, kc *kawa.Client, role *kawav1alpha1.Role, desired kawa.Role, _ bool) error {
			return kc.PutRole(ctx, role.GatewayName(), desired)
		},
		// Deleting a role also takes it out of every group that lists it.
		remove: func(ctx context.Context, kc *kawa.Client, name string, _ kawa.ListedRole) error {
			return kc.DeleteRole(ctx, name)
		},
		release: (*kawa.Client).ReleaseRole,
	}
}

// desiredRole is the admin API form of the Role's spec, as the gateway lists it.
func desiredRole(role *kawav1alpha1.Role) kawa.Role {
	acls := make([]kawa.ACL, 0, len(role.Spec.ACLs))
	for _, acl := range role.Spec.ACLs {
		resource := kawa.Resource{
			Type:        string(acl.Resource.Type),
			Pattern:     acl.Resource.Pattern,
			PatternType: string(acl.Resource.PatternType),
		}
		if resource.PatternType == "" {
			resource.PatternType = string(kawav1alpha1.PatternTypeLiteral)
		}
		acls = append(acls, kawa.ACL{Resource: resource, Operation: string(acl.Operation), Permission: string(acl.Permission)})
	}
	return kawa.Role{ACLs: acls}
}

// SetupWithManager sets up the controller with the Manager.
func (r *RoleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b, err := r.managed().controller(mgr)
	if err != nil {
		return err
	}
	return b.Complete(r)
}
