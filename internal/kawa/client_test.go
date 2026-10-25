package kawa_test

import (
	"context"
	"errors"
	"net/http"
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
