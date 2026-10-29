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

// VirtualTopicFinalizer removes a VirtualTopic's virtual topic from the gateway before the
// VirtualTopic goes away.
const VirtualTopicFinalizer = "virtualtopic.kawa-platform.github.io/finalizer"

// VirtualTopicReconciler makes the operator's gateway serve each VirtualTopic through the gateway's
// admin API: it creates, updates (also when the topic drifted, e.g. after an edit in kawa studio) and
// deletes the virtual topic. See [managed] for how it claims, adopts and releases a name.
//
// Every call is made as the operator itself: it logs in to the gateway with its client credentials,
// so the gateway's governance and logs see the operator's client.
type VirtualTopicReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Admin is the gateway's admin API; it reads the endpoint anew on every reconcile, so rotated
	// Secrets are picked up.
	Admin *kawa.Admin
	// NamespaceSelector limits the namespaces whose VirtualTopics are served; nil serves all.
	NamespaceSelector labels.Selector
	// ResyncInterval is how often a synced virtual topic is compared with the gateway again, to
	// undo drift.
	ResyncInterval time.Duration
}

// +kubebuilder:rbac:groups=kawa-platform.github.io,resources=virtualtopics,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=kawa-platform.github.io,resources=virtualtopics/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kawa-platform.github.io,resources=virtualtopics/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch

func (r *VirtualTopicReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	return r.managed().Reconcile(ctx, req)
}

func (r *VirtualTopicReconciler) managed() *managed[*kawav1alpha1.VirtualTopic, kawa.VirtualTopic, kawa.ListedVirtualTopic] {
	return &managed[*kawav1alpha1.VirtualTopic, kawa.VirtualTopic, kawa.ListedVirtualTopic]{
		Client:            r.Client,
		admin:             r.Admin,
		namespaceSelector: r.NamespaceSelector,
		resyncInterval:    r.ResyncInterval,

		kind:      "VirtualTopic",
		resource:  "virtual topic",
		finalizer: VirtualTopicFinalizer,
		newObject: func() *kawav1alpha1.VirtualTopic { return &kawav1alpha1.VirtualTopic{} },
		newList:   func() client.ObjectList { return &kawav1alpha1.VirtualTopicList{} },
		desired: func(_ context.Context, vt *kawav1alpha1.VirtualTopic) (kawa.VirtualTopic, *outcome) {
			return desiredVirtualTopic(vt), nil
		},
		// Only virtual topics are listed: DELETE /topics/{name} deletes the physical topic when name
		// is not a virtual topic, so remove must only ever see names the gateway lists as virtual.
		list: func(kc *kawa.Client, ctx context.Context) (map[string]kawa.ListedVirtualTopic, error) {
			topics, err := kc.ListTopics(ctx)
			if err != nil {
				return nil, err
			}
			return topics.Virtual, nil
		},
		managedBy: func(listed kawa.ListedVirtualTopic) *kawa.ManagedBy { return listed.ManagedBy },
		inSync: func(_ *kawav1alpha1.VirtualTopic, desired kawa.VirtualTopic, listed kawa.ListedVirtualTopic) bool {
			return matches(listed, desired)
		},
		write: func(ctx context.Context, kc *kawa.Client, vt *kawav1alpha1.VirtualTopic, desired kawa.VirtualTopic, _ bool) error {
			return kc.PutVirtualTopic(ctx, vt.GatewayName(), desired)
		},
		remove: func(ctx context.Context, kc *kawa.Client, name string, _ kawa.ListedVirtualTopic) error {
			return kc.DeleteVirtualTopic(ctx, name)
		},
		release:  (*kawa.Client).ReleaseVirtualTopic,
		governed: true,
	}
}

// desiredVirtualTopic is the admin API form of the VirtualTopic's spec.
func desiredVirtualTopic(vt *kawav1alpha1.VirtualTopic) kawa.VirtualTopic {
	topic := kawa.VirtualTopic{Topic: vt.Spec.PhysicalTopic, ExposePhysicalTopic: vt.Spec.ExposePhysicalTopic}
	if f := vt.Spec.Filter; f != nil {
		topic.Filter = &kawa.Filter{Type: string(f.Type), Header: f.Header, Expression: f.Expression}
		if f.Value != nil {
			topic.Filter.Value = *f.Value
		}
	}
	if format := vt.Spec.ValueFormat; format != nil {
		onDecodeError := string(format.OnDecodeError)
		if onDecodeError == "" {
			onDecodeError = "skip"
		}
		topic.ValueFormat = &kawa.PayloadFormat{Type: string(format.Type), OnDecodeError: onDecodeError}
	}
	return topic
}

// matches is whether the gateway already serves desired as listed.
func matches(listed kawa.ListedVirtualTopic, desired kawa.VirtualTopic) bool {
	return listed.PhysicalTopic == desired.Topic &&
		listed.ExposePhysicalTopic == desired.ExposePhysicalTopic &&
		reflect.DeepEqual(listed.Filter, desired.Filter.View()) &&
		reflect.DeepEqual(listed.ValueFormat, desired.ValueFormat)
}

// SetupWithManager sets up the controller with the Manager.
func (r *VirtualTopicReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b, err := r.managed().controller(mgr)
	if err != nil {
		return err
	}
	return b.Complete(r)
}
