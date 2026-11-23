package kawa_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/kawa-platform/kawa-operator/internal/kawa"
	"github.com/kawa-platform/kawa-operator/internal/kawa/kawatest"
)

func TestPutsAndListsAVirtualTopicWithClientCredentials(t *testing.T) {
	// given
	gateway := kawatest.NewGateway("kawa-operator", "s3cret")
	defer gateway.Close()
	client := newClient(t, kawa.Endpoint{BaseURL: gateway.URL, ClientID: "kawa-operator", ClientSecret: "s3cret"})
	topic := kawa.VirtualTopic{
		Topic:       "orders-v2",
		Filter:      &kawa.Filter{Type: "headerEquals", Header: "region", Value: "eu"},
		ValueFormat: &kawa.PayloadFormat{Type: "json", OnDecodeError: "skip"},
	}

	// when
	err := client.PutVirtualTopic(context.Background(), "orders.eu", topic)

	// then
	if err != nil {
		t.Fatalf("PUT was refused: %v", err)
	}
	topics, err := client.ListTopics(context.Background())
	if err != nil {
		t.Fatalf("GET /topics failed: %v", err)
	}
	listed, ok := topics.Virtual["orders.eu"]
	if !ok {
		t.Fatalf("the virtual topic was not listed: %+v", topics)
	}
	if listed.PhysicalTopic != "orders-v2" || *listed.Filter != (kawa.FilterView{Kind: "header", Expression: "region=eu"}) {
		t.Fatalf("the virtual topic was listed as %+v", listed)
	}
}

func TestReportsRefusedClientCredentialsAsAnAuthenticationError(t *testing.T) {
	// given
	gateway := kawatest.NewGateway("kawa-operator", "s3cret")
	defer gateway.Close()
	client := newClient(t, kawa.Endpoint{BaseURL: gateway.URL, ClientID: "kawa-operator", ClientSecret: "wrong"})

	// when
	_, err := client.ListTopics(context.Background())

	// then
	if !kawa.IsAuthenticationError(err) {
		t.Fatalf("a refused client secret was not reported as an authentication error: %v", err)
	}
}

func TestReportsTheAdminAPIErrorMessage(t *testing.T) {
	// given
	gateway := kawatest.NewGateway()
	defer gateway.Close()
	gateway.Reject("orders", http.StatusForbidden, "topic 'orders' rejected by governance: [naming] bad name")
	client := newClient(t, kawa.Endpoint{BaseURL: gateway.URL})

	// when
	err := client.PutVirtualTopic(context.Background(), "orders", kawa.VirtualTopic{Topic: "orders-v2"})

	// then
	if kawa.StatusCode(err) != http.StatusForbidden || err.Error() != "kawa admin API answered 403: topic 'orders' rejected by governance: [naming] bad name" {
		t.Fatalf("the governance rejection was not reported as such: %v", err)
	}
}

func TestRendersFiltersTheWayTheListingDoes(t *testing.T) {
	cases := map[kawa.Filter]kawa.FilterView{
		{Type: "headerEquals", Header: "h", Value: "v"}:     {Kind: "header", Expression: "h=v"},
		{Type: "headerContains", Header: "h", Value: "v"}:   {Kind: "headerContains", Expression: "h contains v"},
		{Type: "headerStartsWith", Header: "h", Value: "v"}: {Kind: "headerStartsWith", Expression: "h starts with v"},
		{Type: "headerMatches", Header: "h", Value: "v.*"}:  {Kind: "headerMatches", Expression: "h matches v.*"},
		{Type: "cel", Expression: "headers.h == 'v'"}:       {Kind: "cel", Expression: "headers.h == 'v'"},
	}
	for filter, want := range cases {
		if got := filter.View(); *got != want {
			t.Errorf("filter %+v rendered as %+v, the gateway lists %+v", filter, *got, want)
		}
	}
}

func TestActsAsTheOperator(t *testing.T) {
	// given
	gateway := kawatest.NewGateway("kawa-operator", "s3cret")
	defer gateway.Close()
	client, err := kawa.ClientFor(kawa.Endpoint{BaseURL: gateway.URL, ClientID: "kawa-operator", ClientSecret: "s3cret"})
	if err != nil {
		t.Fatalf("no client: %v", err)
	}

	// when
	err = client.PutVirtualTopic(context.Background(), "orders", kawa.VirtualTopic{Topic: "orders-v2"})

	// then
	if got := gateway.WritesBy("orders"); err != nil || len(got) != 1 || got[0] != "kawa-operator" {
		t.Fatalf("the write was not made as the operator: %v %v", err, got)
	}
}

func TestReportsAWrongSecret(t *testing.T) {
	// given
	gateway := kawatest.NewGateway("kawa-operator", "s3cret")
	defer gateway.Close()
	client, _ := kawa.ClientFor(kawa.Endpoint{BaseURL: gateway.URL, ClientID: "kawa-operator", ClientSecret: "wrong"})

	// when
	_, err := client.ListTopics(context.Background())

	// then
	if !kawa.IsAuthenticationError(err) {
		t.Fatalf("a refused secret was not reported as an authentication error: %v", err)
	}
}

func TestReportsAnOperatorTheGatewayNoLongerKnows(t *testing.T) {
	// given: a client with a cached token, on a gateway with another client so authentication stays on
	gateway := kawatest.NewGateway("kawa-operator", "s3cret", "kawa-studio", "other")
	defer gateway.Close()
	client, _ := kawa.ClientFor(kawa.Endpoint{BaseURL: gateway.URL, ClientID: "kawa-operator", ClientSecret: "s3cret"})
	if _, err := client.ListTopics(context.Background()); err != nil {
		t.Fatalf("the first call failed: %v", err)
	}

	// when: the gateway restarts without the operator client, so the cached token is dead
	gateway.RemoveClient("kawa-operator")
	_, err := client.ListTopics(context.Background())

	// then: the client asked again and learnt why
	if !kawa.IsAuthenticationError(err) {
		t.Fatalf("a removed operator client was not reported as an authentication error: %v", err)
	}
}

func TestRetriesOnceWithAFreshTokenAfterAGatewayRestart(t *testing.T) {
	// given
	gateway := kawatest.NewGateway("kawa-operator", "s3cret")
	defer gateway.Close()
	client, _ := kawa.ClientFor(kawa.Endpoint{BaseURL: gateway.URL, ClientID: "kawa-operator", ClientSecret: "s3cret"})
	if _, err := client.ListTopics(context.Background()); err != nil {
		t.Fatalf("the first call failed: %v", err)
	}

	// when: the gateway forgets its tokens but still knows the operator
	gateway.ForgetTokens()
	err := client.PutVirtualTopic(context.Background(), "orders", kawa.VirtualTopic{Topic: "orders-v2"})

	// then
	if err != nil || len(gateway.WritesBy("orders")) != 1 {
		t.Fatalf("the write was not retried once with a fresh token: %v %v", err, gateway.WritesBy("orders"))
	}
}

func TestCallsAGatewayWithoutAdminAuthenticationAsIs(t *testing.T) {
	// given: a gateway with admin.auth.type none
	gateway := kawatest.NewGateway()
	defer gateway.Close()
	client, _ := kawa.ClientFor(kawa.Endpoint{BaseURL: gateway.URL})

	// when
	err := client.PutVirtualTopic(context.Background(), "orders", kawa.VirtualTopic{Topic: "orders-v2"})

	// then
	if err != nil || gateway.WritesBy("orders")[0] != "admin" {
		t.Fatalf("an unauthenticated gateway was not called as is: %v %v", err, gateway.WritesBy("orders"))
	}
}

func newClient(t *testing.T, endpoint kawa.Endpoint) *kawa.Client {
	t.Helper()
	client, err := kawa.ClientFor(endpoint)
	if err != nil {
		t.Fatalf("no client: %v", err)
	}
	return client
}

func TestListsWhoManagesARoleAndReleasesIt(t *testing.T) {
	// given: a role the operator wrote
	gateway := kawatest.NewGateway("kawa-operator", "s3cret")
	defer gateway.Close()
	client, _ := kawa.ClientFor(kawa.Endpoint{BaseURL: gateway.URL, ClientID: "kawa-operator", ClientSecret: "s3cret"})
	ctx := context.Background()
	if err := client.PutRole(ctx, "reader", kawa.Role{}); err != nil {
		t.Fatalf("PUT was refused: %v", err)
	}
	roles, err := client.ListRoles(ctx)
	if err != nil {
		t.Fatalf("GET /rbac/roles failed: %v", err)
	}
	if !client.Manages(roles["reader"].ManagedBy) {
		t.Fatalf("the operator's role was listed as managed by %+v", roles["reader"].ManagedBy)
	}

	// when
	err = client.ReleaseRole(ctx, "reader")

	// then
	if err != nil {
		t.Fatalf("the release was refused: %v", err)
	}
	if roles, _ := client.ListRoles(ctx); roles["reader"].ManagedBy != nil || client.Manages(roles["reader"].ManagedBy) {
		t.Fatalf("the released role is still listed as managed by %+v", roles["reader"].ManagedBy)
	}
}

func TestCountsEveryResourceAsManagedWithoutAdminAuthentication(t *testing.T) {
	// given: a gateway without admin authentication, which marks nothing
	gateway := kawatest.NewGateway()
	defer gateway.Close()
	client, _ := kawa.ClientFor(kawa.Endpoint{BaseURL: gateway.URL})

	// then
	if !client.Manages(nil) {
		t.Fatal("an unmarked resource was not counted as managed without admin authentication")
	}
}

func TestAdminReusesItsClientUntilTheEndpointChanges(t *testing.T) {
	// given
	gateway := kawatest.NewGateway("kawa-operator", "s3cret")
	defer gateway.Close()
	endpoint := kawa.Endpoint{BaseURL: gateway.URL, ClientID: "kawa-operator", ClientSecret: "s3cret"}
	admin := kawa.NewAdmin(func() (kawa.Endpoint, error) { return endpoint, nil })
	first, err := admin.Client()
	if err != nil {
		t.Fatalf("no client: %v", err)
	}

	// when
	same, _ := admin.Client()
	endpoint.ClientSecret = "rotated"
	rotated, _ := admin.Client()

	// then
	if same != first {
		t.Fatal("an unchanged endpoint yielded a new client, dropping its cached token")
	}
	if rotated == first {
		t.Fatal("a rotated secret kept the old client")
	}
}

func TestAdminReportsAnUnreadableEndpoint(t *testing.T) {
	admin := kawa.NewAdmin(func() (kawa.Endpoint, error) { return kawa.Endpoint{}, errors.New("no secret file") })

	if _, err := admin.Client(); err == nil || err.Error() != "no secret file" {
		t.Fatalf("the endpoint error was not reported: %v", err)
	}
}

// everyStep is one step of each type, with its wire form.
var everyStep = []kawa.Transformation{
	{Type: "dropFields", Fields: []string{"internal_risk_score", "customer.ssn"}},
	{Type: "renameField", From: "order_id", To: "id"},
	{Type: "defaultField", Field: "currency", Value: json.RawMessage(`{"code":"EUR","digits":2}`)},
	{Type: "cel", Expression: `{"id": value.id}`},
	{Type: "convert", Format: &kawa.PayloadFormat{Type: "avro", OnDecodeError: "fail", Subject: "orders-value", Version: 3}},
}

const everyStepJSON = `[
	{"type": "dropFields", "fields": ["internal_risk_score", "customer.ssn"]},
	{"type": "renameField", "from": "order_id", "to": "id"},
	{"type": "defaultField", "field": "currency", "value": {"code": "EUR", "digits": 2}},
	{"type": "cel", "expression": "{\"id\": value.id}"},
	{"type": "convert", "format": {"type": "avro", "onDecodeError": "fail", "subject": "orders-value", "version": 3}}
]`

func TestSendsEveryTransformationStepInTheWireShape(t *testing.T) {
	// given
	var body map[string]any
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(payload, &body)
	}))
	defer gateway.Close()
	client := newClient(t, kawa.Endpoint{BaseURL: gateway.URL})
	topic := kawa.VirtualTopic{
		Topic:           "orders-v2",
		ValueFormat:     &kawa.PayloadFormat{Type: "xml", OnDecodeError: "skip", Root: "order"},
		Transformations: everyStep,
	}

	// when
	err := client.PutVirtualTopic(context.Background(), "orders", topic)

	// then
	if err != nil {
		t.Fatalf("PUT failed: %v", err)
	}
	var want any
	if err := json.Unmarshal([]byte(everyStepJSON), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(body["transformations"], want) {
		t.Errorf("the steps were sent as %v, want %v", body["transformations"], want)
	}
	if format := body["valueFormat"]; !reflect.DeepEqual(format, map[string]any{"type": "xml", "onDecodeError": "skip", "root": "order"}) {
		t.Errorf("the value format was sent as %v", format)
	}
}

func TestSendsADefaultFieldWithoutValueAsNullAndOtherValuesAsTheyAre(t *testing.T) {
	cases := map[string]string{
		"":        `{"type":"defaultField","field":"n","value":null}`,
		`null`:    `{"type":"defaultField","field":"n","value":null}`,
		`false`:   `{"type":"defaultField","field":"n","value":false}`,
		`0`:       `{"type":"defaultField","field":"n","value":0}`,
		`""`:      `{"type":"defaultField","field":"n","value":""}`,
		`[]`:      `{"type":"defaultField","field":"n","value":[]}`,
		`{"a":1}`: `{"type":"defaultField","field":"n","value":{"a":1}}`,
	}
	for value, want := range cases {
		step := kawa.Transformation{Type: "defaultField", Field: "n"}
		if value != "" {
			step.Value = json.RawMessage(value)
		}
		got, err := json.Marshal(step)
		if err != nil || string(got) != want {
			t.Errorf("a defaultField with value %q was sent as %s (%v), want %s", value, got, err, want)
		}
	}
}

func TestSendsOnlyTheKeysOfTheStepType(t *testing.T) {
	cases := map[string]kawa.Transformation{
		`{"type":"dropFields","fields":["a"]}`:          {Type: "dropFields", Fields: []string{"a"}},
		`{"type":"renameField","from":"a","to":"b"}`:    {Type: "renameField", From: "a", To: "b"},
		`{"type":"cel","expression":"value"}`:           {Type: "cel", Expression: "value"},
		`{"type":"convert","format":{"type":"json"}}`:   {Type: "convert", Format: &kawa.PayloadFormat{Type: "json"}},
		`{"type":"defaultField","field":"a","value":1}`: {Type: "defaultField", Field: "a", Value: json.RawMessage(`1`)},
	}
	for want, step := range cases {
		if got, err := json.Marshal(step); err != nil || string(got) != want {
			t.Errorf("the step %+v was sent as %s (%v), want %s", step, got, err, want)
		}
	}
}

func TestOmitsTransformationsWhenThereAreNone(t *testing.T) {
	// given
	var body map[string]any
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(payload, &body)
	}))
	defer gateway.Close()
	client := newClient(t, kawa.Endpoint{BaseURL: gateway.URL})

	// when
	err := client.PutVirtualTopic(context.Background(), "orders", kawa.VirtualTopic{Topic: "orders-v2"})

	// then
	if _, sent := body["transformations"]; err != nil || sent {
		t.Fatalf("a virtual topic without steps was sent with transformations: %v %v", err, body)
	}
}

func TestReadsEveryTransformationStepFromTheListing(t *testing.T) {
	// given
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `[{"type": "virtual", "name": "orders", "physicalTopic": "orders-v2",
			"valueFormat": {"type": "json", "onDecodeError": "skip"}, "transformations": `+everyStepJSON+`}]`)
	}))
	defer gateway.Close()
	client := newClient(t, kawa.Endpoint{BaseURL: gateway.URL})

	// when
	topics, err := client.ListTopics(context.Background())

	// then
	if err != nil {
		t.Fatalf("GET /topics failed: %v", err)
	}
	listed := topics.Virtual["orders"].Transformations
	if !kawa.TransformationsEqual(listed, everyStep) {
		t.Errorf("the steps were read as %+v", listed)
	}
	if !reflect.DeepEqual(listed[4].Format, everyStep[4].Format) {
		t.Errorf("the convert format was read as %+v", listed[4].Format)
	}
}

func TestComparesTransformationsByMeaning(t *testing.T) {
	defaultTo := func(value string) []kawa.Transformation {
		step := kawa.Transformation{Type: "defaultField", Field: "n"}
		if value != "" {
			step.Value = json.RawMessage(value)
		}
		return []kawa.Transformation{step}
	}
	equal := map[string][2][]kawa.Transformation{
		"absent and empty lists":     {nil, {}},
		"an integer and a float":     {defaultTo(`1`), defaultTo(`1.0`)},
		"an integer and exponent":    {defaultTo(`100`), defaultTo(`1e2`)},
		"equal integers above 2^53":  {defaultTo(`9007199254740993`), defaultTo(`9007199254740993.0`)},
		"objects spaced and ordered": {defaultTo(`{"a":1,"b":[true,null]}`), defaultTo(`{ "b": [true, null], "a": 1 }`)},
		"a missing value and null":   {defaultTo(""), defaultTo(`null`)},
		"the same steps":             {everyStep, everyStep},
	}
	for name, c := range equal {
		if !kawa.TransformationsEqual(c[0], c[1]) {
			t.Errorf("%s compared unequal", name)
		}
	}
	otherFormat := append([]kawa.Transformation(nil), everyStep...)
	otherFormat[4] = kawa.Transformation{Type: "convert", Format: &kawa.PayloadFormat{Type: "avro", OnDecodeError: "fail", Subject: "orders-value"}}
	unequal := map[string][2][]kawa.Transformation{
		"different numbers":          {defaultTo(`1`), defaultTo(`2`)},
		"integers apart above 2^53":  {defaultTo(`9007199254740993`), defaultTo(`9007199254740992`)},
		"nested integers above 2^53": {defaultTo(`{"n":9007199254740993}`), defaultTo(`{"n":9007199254740992}`)},
		"a number and a string":      {defaultTo(`1`), defaultTo(`"1"`)},
		"a missing step":             {everyStep, everyStep[:4]},
		"a different convert format": {everyStep, otherFormat},
		"reordered dropped fields": {
			{{Type: "dropFields", Fields: []string{"a", "b"}}},
			{{Type: "dropFields", Fields: []string{"b", "a"}}},
		},
	}
	for name, c := range unequal {
		if kawa.TransformationsEqual(c[0], c[1]) {
			t.Errorf("%s compared equal", name)
		}
	}
}
