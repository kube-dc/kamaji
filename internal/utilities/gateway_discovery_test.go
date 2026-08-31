// Copyright 2022 Clastix Labs
// SPDX-License-Identifier: Apache-2.0

package utilities

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	fakediscovery "k8s.io/client-go/discovery/fake"
	clienttesting "k8s.io/client-go/testing"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// fakeDiscovery serves the given Gateway API kinds at gateway.networking.k8s.io/v1.
func fakeDiscovery(kinds ...string) *fakediscovery.FakeDiscovery {
	d := &fakediscovery.FakeDiscovery{Fake: &clienttesting.Fake{}}
	list := &metav1.APIResourceList{GroupVersion: gatewayv1.GroupVersion.String()}

	for _, k := range kinds {
		list.APIResources = append(list.APIResources, metav1.APIResource{Kind: k})
	}

	d.Resources = []*metav1.APIResourceList{list}

	return d
}

// A Gateway API bundle older than 1.5 serves HTTPRoute, GRPCRoute and Gateway at
// v1 but TLSRoute only at v1alpha2/v1alpha3: the group is available, TLSRoute is
// not. Owning TLSRoute on such a cluster makes the manager exit at startup
// (cache sync timeout), so the per-kind check must say "no" for TLSRoute and
// "yes" for the others.
func TestIsGatewayKindAvailablePerKind(t *testing.T) {
	d := fakeDiscovery("Gateway", "GatewayClass", "HTTPRoute", "GRPCRoute")

	for kind, want := range map[string]bool{
		"HTTPRoute": true,
		"GRPCRoute": true,
		"Gateway":   true,
		"TLSRoute":  false,
	} {
		if got := IsGatewayKindAvailable(context.Background(), nil, d, kind); got != want {
			t.Fatalf("%s available = %t, want %t", kind, got, want)
		}
	}

	if IsTLSRouteAvailable(context.Background(), nil, d) {
		t.Fatal("TLSRoute must not be reported available on a bundle < 1.5")
	}

	// Group present is NOT the same as TLSRoute served at v1.
	if ok, _ := GatewayAPIResourcesAvailable(context.Background(), d); !ok {
		t.Fatal("group-level availability should still be true")
	}
}

func TestIsGatewayKindAvailableBundle15(t *testing.T) {
	d := fakeDiscovery("Gateway", "GatewayClass", "HTTPRoute", "GRPCRoute", "TLSRoute")

	if !IsTLSRouteAvailable(context.Background(), nil, d) {
		t.Fatal("TLSRoute served at v1 must be reported available")
	}
}
