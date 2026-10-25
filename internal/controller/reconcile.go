package controller

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/kawa-platform/kawa-operator/internal/kawa"
)

// outcome is how a reconcile ended: the Ready condition to report, and how to continue.
type outcome struct {
	status  metav1.ConditionStatus
	reason  string
	message string
	// retry is returned to controller-runtime for exponential backoff.
	retry error
	// requeueAfter checks again later without counting as a failure.
	requeueAfter time.Duration
}

// failed is a Ready=False outcome for err, retried with backoff.
func failed(reason string, err error) outcome {
	return outcome{
		status:  metav1.ConditionFalse,
		reason:  reason,
		message: err.Error(),
		retry:   err,
	}
}

// gatewayFailed is the outcome of an admin API call that failed with err.
func gatewayFailed(err error) outcome {
	return failed(unavailableReason(err), err)
}

// result is how controller-runtime continues after o.
func (o outcome) result() (ctrl.Result, error) {
	if o.retry != nil {
		return ctrl.Result{}, o.retry
	}
	return ctrl.Result{RequeueAfter: o.requeueAfter}, nil
}

// setReadyCondition records o as the Ready condition computed for generation.
func setReadyCondition(
	conditions *[]metav1.Condition,
	observedGeneration *int64,
	generation int64,
	o outcome,
) {
	meta.SetStatusCondition(conditions, metav1.Condition{
		Type:               ConditionReady,
		Status:             o.status,
		Reason:             o.reason,
		Message:            o.message,
		ObservedGeneration: generation,
	})
	*observedGeneration = generation
}

// patchStatus writes obj's status when it differs from original. An object already gone is not an
// error.
func patchStatus(ctx context.Context, c client.Client, obj, original client.Object) error {
	if reflect.DeepEqual(original, obj) {
		return nil
	}
	return client.IgnoreNotFound(c.Status().Patch(ctx, obj, client.MergeFrom(original)))
}

// admitNamespace is the outcome for an object in namespace when selector does not admit that
// namespace, or nil when it does. A nil selector admits every namespace.
func admitNamespace(ctx context.Context, c client.Reader, selector labels.Selector, namespace string) *outcome {
	admitted, err := namespaceAdmitted(ctx, c, selector, namespace)
	if err != nil {
		o := failed(ReasonNamespaceUnreadable, err)
		return &o
	}
	if !admitted {
		return &outcome{
			status: metav1.ConditionFalse,
			reason: ReasonNamespaceNotAllowed,
			message: fmt.Sprintf("namespace %q does not match the operator's namespace selector %q",
				namespace, selector.String()),
		}
	}
	return nil
}

// namespaceAdmitted is whether selector admits namespace. A nil selector admits every namespace.
func namespaceAdmitted(ctx context.Context, c client.Reader, selector labels.Selector, namespace string) (bool, error) {
	var ns corev1.Namespace
	if err := c.Get(ctx, types.NamespacedName{Name: namespace}, &ns); err != nil {
		return false, fmt.Errorf("reading namespace %q: %w", namespace, err)
	}
	return selector == nil || selector.Matches(labels.Set(ns.Labels)), nil
}

// releaseOnGateway clears the operator's managed-by marker of the gateway resource name, which
// deletionPolicy Retain leaves on the gateway, so it can be changed outside Kubernetes again. It is
// nil when released, also when the resource is gone (404) or not the operator's (409); otherwise it
// is the failure to report.
func releaseOnGateway(
	ctx context.Context,
	admin *kawa.Admin,
	release func(*kawa.Client, context.Context, string) error,
	kind, name string,
) *outcome {
	kawaClient, err := admin.Client()
	if err != nil {
		o := failed(ReasonGatewayConfigInvalid, err)
		return &o
	}
	switch err := release(kawaClient, ctx, name); {
	case err == nil, kawa.StatusCode(err) == http.StatusNotFound, kawa.StatusCode(err) == http.StatusConflict:
		logf.FromContext(ctx).Info("Released managed-by marker on the gateway", "kind", kind, "name", name)
		return nil
	default:
		o := gatewayFailed(err)
		return &o
	}
}

// claimsFirst orders objects competing for the same gateway name: the older one, then by namespace
// and name.
func claimsFirst(a, b client.Object) bool {
	at, bt := a.GetCreationTimestamp(), b.GetCreationTimestamp()
	if !at.Equal(&bt) {
		return at.Before(&bt)
	}
	if a.GetNamespace() != b.GetNamespace() {
		return a.GetNamespace() < b.GetNamespace()
	}
	return a.GetName() < b.GetName()
}

// inNamespace maps a Namespace to requests for the objects of a newList list in it, so they follow
// a change of its labels (the namespace selector).
func inNamespace(c client.Reader, newList func() client.ObjectList) handler.MapFunc {
	return func(ctx context.Context, namespace client.Object) []reconcile.Request {
		list := newList()
		if err := c.List(ctx, list, client.InNamespace(namespace.GetName())); err != nil {
			return nil
		}
		items, err := meta.ExtractList(list)
		if err != nil {
			return nil
		}
		requests := make([]reconcile.Request, 0, len(items))
		for _, item := range items {
			if obj, ok := item.(client.Object); ok {
				requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(obj)})
			}
		}
		return requests
	}
}
