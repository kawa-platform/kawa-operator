package controller

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kawav1alpha1 "github.com/kawa-platform/kawa-operator/api/v1alpha1"
	"github.com/kawa-platform/kawa-operator/internal/kawa"
)

// gatewayNameIndex indexes objects by the name of the gateway resource they manage.
const gatewayNameIndex = "gatewayName"

// managedObject is a custom resource that manages one named resource on the gateway.
type managedObject interface {
	client.Object
	// GatewayName is the name of the resource the object wants on the gateway.
	GatewayName() string
	// ClaimedName points at the status field recording the name the operator manages for the
	// object; empty until the object claims GatewayName.
	ClaimedName() *string
	// ReadyStatus points at the status conditions and observedGeneration the Ready condition is
	// written to.
	ReadyStatus() (*[]metav1.Condition, *int64)
	// GetDeletionPolicy is whether deleting the object deletes the gateway resource (Delete) or
	// only releases it (Retain).
	GetDeletionPolicy() kawav1alpha1.DeletionPolicy
}

// managed reconciles each Object onto the gateway resource it manages: Desired is that resource in
// the form the operator writes it, Listed as the gateway lists it.
//
// The operator only touches a resource it manages: an object first claims the name in its status,
// a resource that already exists on the gateway is adopted only with the kawa-platform.github.io/adopt annotation,
// and of objects competing for a name the one holding the claim wins, else the oldest. It deletes
// (or with deletionPolicy Retain releases) a resource only while the gateway marks it as the
// operator's.
type managed[Object managedObject, Desired, Listed any] struct {
	client.Client
	admin             *kawa.Admin
	namespaceSelector labels.Selector
	resyncInterval    time.Duration

	// kind is the custom resource kind, resource what it manages on the gateway, e.g. "virtual topic".
	kind, resource string
	finalizer      string
	newObject      func() Object
	newList        func() client.ObjectList

	// desired is the admin API form of obj's spec, as the operator writes it, or the outcome when
	// it cannot be built, e.g. when a Secret it needs is missing.
	desired func(ctx context.Context, obj Object) (Desired, *outcome)
	// list reads the gateway's resources by name.
	list      func(kc *kawa.Client, ctx context.Context) (map[string]Listed, error)
	managedBy func(listed Listed) *kawa.ManagedBy
	// inSync is whether listed already is desired.
	inSync func(obj Object, desired Desired, listed Listed) bool
	// write creates (exists false) or updates the resource.
	write func(ctx context.Context, kc *kawa.Client, obj Object, desired Desired, exists bool) error
	// remove deletes the listed resource name.
	remove  func(ctx context.Context, kc *kawa.Client, name string, listed Listed) error
	release func(kc *kawa.Client, ctx context.Context, name string) error
	// governed is whether the gateway's governance rules can refuse a write (403). They can change
	// without the object changing, so a refused write is checked again later.
	governed bool
}

func (m *managed[Object, Desired, Listed]) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	obj := m.newObject()
	if err := m.Get(ctx, req.NamespacedName, obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !obj.GetDeletionTimestamp().IsZero() {
		return m.finalize(ctx, obj)
	}
	if controllerutil.AddFinalizer(obj, m.finalizer) {
		if err := m.Update(ctx, obj); err != nil {
			return ctrl.Result{}, err
		}
	}
	original := obj.DeepCopyObject().(Object)
	return m.report(ctx, obj, original, m.sync(ctx, obj))
}

func (m *managed[Object, Desired, Listed]) sync(ctx context.Context, obj Object) outcome {
	name := obj.GatewayName()

	if o := admitNamespace(ctx, m.Client, m.namespaceSelector, obj.GetNamespace()); o != nil {
		return *o
	}

	if holder, err := m.claimHolder(ctx, obj); err != nil {
		return failed(ReasonAPIUnavailable, err)
	} else if holder != "" {
		return outcome{
			status:       metav1.ConditionFalse,
			reason:       ReasonConflict,
			message:      fmt.Sprintf("%s %s already manages %s %q on the gateway", m.kind, holder, m.resource, name),
			requeueAfter: m.resyncInterval,
		}
	}

	desired, o := m.desired(ctx, obj)
	if o != nil {
		return *o
	}

	kc, err := m.admin.Client()
	if err != nil {
		return failed(ReasonGatewayConfigInvalid, err)
	}
	listing, err := m.list(kc, ctx)
	if err != nil {
		return gatewayFailed(err)
	}
	listed, exists := listing[name]

	// The status holds the name this object claimed, so a mismatch means the operator does not yet
	// own the resource: the object is new, or the claim write below failed on an earlier reconcile.
	if claim := obj.ClaimedName(); *claim != name {
		if adopt, _ := strconv.ParseBool(obj.GetAnnotations()[kawav1alpha1.AdoptAnnotation]); exists && !adopt {
			return outcome{
				status: metav1.ConditionFalse,
				reason: ReasonAlreadyExists,
				message: fmt.Sprintf("%s %q already exists on the gateway; annotate the %s with %s=true to take it over",
					m.resource, name, m.kind, kawav1alpha1.AdoptAnnotation),
				requeueAfter: m.resyncInterval,
			}
		}
		// Claim the name before writing, so a failed status update after the write cannot make the
		// operator mistake its own resource for a foreign one.
		*claim = name
		if err := m.Status().Update(ctx, obj); err != nil {
			return failed(ReasonAPIUnavailable, err)
		}
	}

	synced := outcome{
		status:       metav1.ConditionTrue,
		reason:       ReasonSynced,
		message:      "the gateway has the " + m.resource,
		requeueAfter: m.resyncInterval,
	}
	// A resource not marked as the operator's is written again so that the gateway marks it.
	if exists && kc.Manages(m.managedBy(listed)) && m.inSync(obj, desired, listed) {
		return synced
	}
	if err := m.write(ctx, kc, obj, desired, exists); err != nil {
		return m.writeFailed(err)
	}
	logf.FromContext(ctx).Info("Wrote gateway resource", "kind", m.kind, "name", name, "created", !exists)
	return synced
}

// writeFailed is the outcome of a write the gateway refused with err.
func (m *managed[Object, Desired, Listed]) writeFailed(err error) outcome {
	switch code := kawa.StatusCode(err); {
	case code == http.StatusBadRequest:
		return outcome{status: metav1.ConditionFalse, reason: ReasonInvalidSpec, message: err.Error()}
	case code == http.StatusConflict:
		// Another principal manages the resource; it may release it later.
		return outcome{status: metav1.ConditionFalse, reason: ReasonConflict, message: err.Error(), requeueAfter: m.resyncInterval}
	case code == http.StatusForbidden && m.governed:
		return outcome{status: metav1.ConditionFalse, reason: ReasonGovernanceRejected, message: err.Error(), requeueAfter: m.resyncInterval}
	default:
		return gatewayFailed(err)
	}
}

// finalize deletes the claimed resource from the gateway (with deletionPolicy Retain, only releases
// it) and then lets the object go. It does so even when the namespace stopped matching the namespace
// selector: the operator cleans up what it made.
func (m *managed[Object, Desired, Listed]) finalize(ctx context.Context, obj Object) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(obj, m.finalizer) {
		return ctrl.Result{}, nil
	}
	if name := *obj.ClaimedName(); name != "" {
		original := obj.DeepCopyObject().(Object)
		var o *outcome
		if obj.GetDeletionPolicy() == kawav1alpha1.DeletionPolicyRetain {
			o = releaseOnGateway(ctx, m.admin, m.release, m.kind, name)
		} else {
			o = m.deleteFromGateway(ctx, name)
		}
		if o != nil {
			return m.report(ctx, obj, original, *o)
		}
	}
	controllerutil.RemoveFinalizer(obj, m.finalizer)
	return ctrl.Result{}, m.Update(ctx, obj)
}

// deleteFromGateway deletes the resource name when the gateway marks it as the operator's. It is nil
// when the resource is gone or not the operator's; otherwise it is the failure to report.
func (m *managed[Object, Desired, Listed]) deleteFromGateway(ctx context.Context, name string) *outcome {
	log := logf.FromContext(ctx)
	kc, err := m.admin.Client()
	if err != nil {
		o := failed(ReasonGatewayConfigInvalid, err)
		return &o
	}
	listing, err := m.list(kc, ctx)
	if err != nil {
		o := gatewayFailed(err)
		return &o
	}
	listed, exists := listing[name]
	if !exists {
		return nil
	}
	if !kc.Manages(m.managedBy(listed)) {
		log.Info("Left gateway resource the operator does not manage", "kind", m.kind, "name", name)
		return nil
	}
	err = m.remove(ctx, kc, name, listed)
	switch code := kawa.StatusCode(err); {
	case err == nil, code == http.StatusNotFound:
		log.Info("Deleted gateway resource", "kind", m.kind, "name", name)
		return nil
	case code == http.StatusForbidden:
		return &outcome{status: metav1.ConditionFalse, reason: ReasonDeletionRejected, message: err.Error(), requeueAfter: time.Minute}
	case code == http.StatusConflict:
		// The resource changed between listing and deleting, e.g. a client joined a group being
		// emptied, or another principal took it over: list it again.
		o := failed(ReasonDeletionRejected, err)
		return &o
	default:
		o := gatewayFailed(err)
		return &o
	}
}

// claimHolder is the namespace/name of another object that wins the gateway name obj wants, or "".
// An object holding the claim (status) wins over one that does not, even while it is being deleted:
// it owns the resource until its finalizer is done. Between objects alike, the older one wins; an
// unclaimed rival that is being deleted or outside the namespace selector does not count.
func (m *managed[Object, Desired, Listed]) claimHolder(ctx context.Context, obj Object) (string, error) {
	name := obj.GatewayName()
	claimed := *obj.ClaimedName() == name
	siblings, err := m.sameName(ctx, obj)
	if err != nil {
		return "", err
	}
	for _, other := range siblings {
		otherClaimed := *other.ClaimedName() == name
		switch {
		case otherClaimed != claimed:
			if !otherClaimed {
				continue
			}
		case !other.GetDeletionTimestamp().IsZero() || !claimsFirst(other, obj):
			continue
		case !otherClaimed:
			admitted, err := namespaceAdmitted(ctx, m.Client, m.namespaceSelector, other.GetNamespace())
			if err != nil {
				return "", err
			}
			if !admitted {
				continue
			}
		}
		return other.GetNamespace() + "/" + other.GetName(), nil
	}
	return "", nil
}

// sameName is the other objects that want obj's gateway name.
func (m *managed[Object, Desired, Listed]) sameName(ctx context.Context, obj Object) ([]Object, error) {
	list := m.newList()
	if err := m.List(ctx, list, client.MatchingFields{gatewayNameIndex: obj.GatewayName()}); err != nil {
		return nil, err
	}
	items, err := meta.ExtractList(list)
	if err != nil {
		return nil, err
	}
	others := make([]Object, 0, len(items))
	for _, item := range items {
		if other, ok := item.(Object); ok && other.GetUID() != obj.GetUID() {
			others = append(others, other)
		}
	}
	return others, nil
}

// rivals maps an object to requests for the others that want its gateway name, so a loser of the
// name follows the winner, e.g. takes the name over once the winner is deleted.
func (m *managed[Object, Desired, Listed]) rivals(ctx context.Context, obj client.Object) []reconcile.Request {
	t, ok := obj.(Object)
	if !ok {
		return nil
	}
	others, err := m.sameName(ctx, t)
	if err != nil {
		logf.FromContext(ctx).Error(err, "Failed to list rivals for a gateway name", "kind", m.kind, "name", t.GatewayName())
		return nil
	}
	requests := make([]reconcile.Request, 0, len(others))
	for _, other := range others {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(other)})
	}
	return requests
}

// report records result as the Ready condition and turns it into the reconcile result.
func (m *managed[Object, Desired, Listed]) report(ctx context.Context, obj, original Object, result outcome) (ctrl.Result, error) {
	conditions, observedGeneration := obj.ReadyStatus()
	setReadyCondition(conditions, observedGeneration, obj.GetGeneration(), result)
	if err := patchStatus(ctx, m.Client, obj, original); err != nil {
		return ctrl.Result{}, err
	}
	return result.result()
}

// controller indexes Objects by gateway name and is their controller, following rivals for the same
// name and namespace label changes. The caller adds watches of its own and completes it with r.
func (m *managed[Object, Desired, Listed]) controller(mgr ctrl.Manager) (*builder.Builder, error) {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), m.newObject(), gatewayNameIndex,
		func(obj client.Object) []string {
			return []string{obj.(Object).GatewayName()}
		}); err != nil {
		return nil, err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(m.newObject()).
		Watches(m.newObject(), handler.EnqueueRequestsFromMapFunc(m.rivals)).
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(inNamespace(m.Client, m.newList))).
		Named(strings.ToLower(m.kind)), nil
}
