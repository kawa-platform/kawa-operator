package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kawav1alpha1 "github.com/kawa-platform/kawa-operator/api/v1alpha1"
)

// The specs are written as unstructured JSON, as kubectl apply sends them, so that they do not
// depend on how the Go types spell a field.
var _ = Describe("VirtualTopic admission", func() {
	var ns string

	BeforeEach(func() {
		newGateway()
		ns = newNamespace()
	})

	AfterEach(func() {
		removeAll(&kawav1alpha1.VirtualTopicList{})
	})

	DescribeTable("rejects an invalid spec at creation",
		func(spec map[string]any) {
			err := k8sClient.Create(ctx, virtualTopicWith(ns, spec))
			Expect(err).To(HaveOccurred(), "the API server accepted the invalid spec %v", spec)
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "the spec was not rejected as invalid: %v", err)
		},
		// 1. transformations need a valueFormat
		Entry("transformations without valueFormat",
			specWith(nil, drop("a"))),
		Entry("only a convert step without valueFormat",
			specWith(nil, convert(format("json")))),

		// 2. convert only last
		Entry("convert as the first of two steps",
			specWith(format("json"), convert(format("json")), drop("a"))),
		Entry("convert in the middle of three steps",
			specWith(format("json"), drop("a"), convert(format("json")), rename("b", "c"))),
		Entry("two convert steps",
			specWith(format("json"), convert(format("json")), convert(format("json")))),

		// 3. Avro output needs a subject
		Entry("an Avro valueFormat without subject and no convert step",
			specWith(format("avro"), drop("a"))),
		Entry("a convert to Avro without subject",
			specWith(format("json"), drop("a"), convert(format("avro")))),
		Entry("a convert to Avro without subject from an Avro valueFormat with subject",
			specWith(withKey(format("avro"), "subject", "orders-value"), convert(format("avro")))),

		// 4. XML output needs a root
		Entry("an XML valueFormat without root and no convert step",
			specWith(format("xml"), drop("a"))),
		Entry("a convert to XML without root",
			specWith(format("json"), drop("a"), convert(format("xml")))),
		Entry("a convert to XML without root from an XML valueFormat with root",
			specWith(withKey(format("xml"), "root", "order"), convert(format("xml")))),

		// 5. field paths
		Entry("a dropFields path with an empty segment", specWith(format("json"), drop("a..b"))),
		Entry("a dropFields path starting with a dot", specWith(format("json"), drop(".a"))),
		Entry("a dropFields path ending with a dot", specWith(format("json"), drop("a."))),
		Entry("a blank dropFields path", specWith(format("json"), drop(""))),
		Entry("a whitespace-only dropFields path", specWith(format("json"), drop("   "))),
		Entry("an invalid dropFields path after a valid one", specWith(format("json"), drop("a", "b..c"))),
		Entry("a renameField from with an empty segment", specWith(format("json"), rename("a..b", "c"))),
		Entry("a renameField from starting with a dot", specWith(format("json"), rename(".a", "c"))),
		Entry("a whitespace-only renameField from", specWith(format("json"), rename(" ", "c"))),
		Entry("a renameField to ending with a dot", specWith(format("json"), rename("a", "c."))),
		Entry("a renameField to with an empty segment", specWith(format("json"), rename("a", "c..d"))),
		Entry("a blank renameField to", specWith(format("json"), rename("a", ""))),
		Entry("a whitespace-only renameField to", specWith(format("json"), rename("a", "\t"))),
		Entry("a defaultField field with an empty segment", specWith(format("json"), defaultTo("a..b"))),
		Entry("a defaultField field starting with a dot", specWith(format("json"), defaultTo(".a"))),
		Entry("a defaultField field ending with a dot", specWith(format("json"), defaultTo("a."))),
		Entry("a blank defaultField field", specWith(format("json"), defaultTo(""))),
		Entry("a whitespace-only defaultField field", specWith(format("json"), defaultTo("  "))),

		// 6. renameField from and to
		Entry("a renameField to itself", specWith(format("json"), rename("customer", "customer"))),
		Entry("a nested renameField to itself", specWith(format("json"), rename("customer.name", "customer.name"))),
		Entry("a renameField into its own child", specWith(format("json"), rename("customer", "customer.name"))),
		Entry("a renameField into its own parent", specWith(format("json"), rename("customer.name", "customer"))),
		Entry("a renameField into its own grandchild", specWith(format("json"), rename("a", "a.b.c"))),
	)

	DescribeTable("accepts a valid spec",
		func(spec map[string]any) {
			Expect(k8sClient.Create(ctx, virtualTopicWith(ns, spec))).To(Succeed())
		},
		Entry("no transformations and no valueFormat", map[string]any{"physicalTopic": "orders-v2"}),
		Entry("no transformations with a valueFormat", specWith(format("json"))),
		Entry("convert as the only step", specWith(format("json"), convert(format("json")))),
		Entry("convert as the last step", specWith(format("json"), drop("a"), rename("b", "c"), convert(format("json")))),
		Entry("an Avro valueFormat without subject converted to JSON",
			specWith(format("avro"), drop("a"), convert(format("json")))),
		Entry("an Avro valueFormat with subject and no convert step",
			specWith(withKey(format("avro"), "subject", "orders-value"), drop("a"))),
		Entry("a convert to Avro with subject",
			specWith(format("json"), drop("a"), convert(withKey(format("avro"), "subject", "orders-value")))),
		Entry("an XML valueFormat with root and no convert step",
			specWith(withKey(format("xml"), "root", "order"), drop("a"))),
		Entry("a convert to XML with root",
			specWith(format("json"), drop("a"), convert(withKey(format("xml"), "root", "order")))),
		Entry("a decode-only Avro valueFormat without subject", specWith(format("avro"))),
		Entry("a decode-only XML valueFormat without root", specWith(format("xml"))),
		Entry("single-segment and nested paths",
			specWith(format("json"), drop("a", "a.b.c"), rename("x", "y.z"), defaultTo("q.r.s"))),
		Entry("path segments that contain spaces",
			specWith(format("json"), drop("first name", "customer.last name"), rename("order id", "id"), defaultTo("unit price"))),
		Entry("a renameField to a name that only starts with the same text",
			specWith(format("json"), rename("customer", "customerName"))),
		Entry("a nested renameField to a name that only starts with the same text",
			specWith(format("json"), rename("customer.name", "customer.names"))),
		Entry("a renameField from a name that only starts with the same text",
			specWith(format("json"), rename("customerName", "customer"))),
	)

	It("rejects an update that makes the spec invalid", func() {
		// given
		vt := virtualTopicWith(ns, specWith(format("json"), drop("a")))
		Expect(k8sClient.Create(ctx, vt)).To(Succeed())

		// when / then: re-read before each attempt, the controller adds its finalizer concurrently
		Eventually(func() string {
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(vt), vt)).To(Succeed())
			vt.Object["spec"] = specWith(format("json"), convert(format("json")), drop("a"))
			err := k8sClient.Update(ctx, vt)
			switch {
			case err == nil:
				return "the invalid update was accepted"
			case apierrors.IsInvalid(err):
				return "invalid"
			default:
				return err.Error()
			}
		}, eventually, poll).Should(Equal("invalid"))
	})
})

// virtualTopicWith is a VirtualTopic "orders" in namespace with spec.
func virtualTopicWith(namespace string, spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": kawav1alpha1.GroupVersion.String(), "kind": "VirtualTopic",
		"metadata": map[string]any{"namespace": namespace, "name": "orders"},
		"spec":     spec,
	}}
}

// specWith is a VirtualTopic spec of physical topic orders-v2 with valueFormat, left out when nil,
// and steps, left out when there are none.
func specWith(valueFormat map[string]any, steps ...map[string]any) map[string]any {
	spec := map[string]any{"physicalTopic": "orders-v2"}
	if valueFormat != nil {
		spec["valueFormat"] = valueFormat
	}
	if len(steps) > 0 {
		transformations := make([]any, 0, len(steps))
		for _, step := range steps {
			transformations = append(transformations, step)
		}
		spec["transformations"] = transformations
	}
	return spec
}

// format is a payload format of type kind.
func format(kind string) map[string]any {
	return map[string]any{"type": kind}
}

// withKey is format with key set to value.
func withKey(format map[string]any, key, value string) map[string]any {
	format[key] = value
	return format
}

func drop(fields ...string) map[string]any {
	items := make([]any, 0, len(fields))
	for _, field := range fields {
		items = append(items, field)
	}
	return map[string]any{"type": "dropFields", "fields": items}
}

func rename(from, to string) map[string]any {
	return map[string]any{"type": "renameField", "from": from, "to": to}
}

func defaultTo(field string) map[string]any {
	return map[string]any{"type": "defaultField", "field": field, "value": int64(1)}
}

func convert(format map[string]any) map[string]any {
	return map[string]any{"type": "convert", "format": format}
}
