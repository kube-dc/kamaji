// Copyright 2022 Clastix Labs
// SPDX-License-Identifier: Apache-2.0

package utilities

import (
	"context"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// AreGatewayResourcesAvailable checks if Gateway API is available in the cluster through a discovery Client
// with fallback to client-based check.
func AreGatewayResourcesAvailable(ctx context.Context, c client.Client, discoveryClient discovery.DiscoveryInterface) bool {
	if discoveryClient == nil {
		return IsGatewayAPIAvailableViaClient(ctx, c)
	}

	available, err := GatewayAPIResourcesAvailable(ctx, discoveryClient)
	if err != nil {
		return false
	}

	return available
}

// NOTE: These functions are extremely similar, maybe they can be merged and accept a GVK.
// Explicit for now.
// GatewayAPIResourcesAvailable checks if Gateway API is available in the cluster.
func GatewayAPIResourcesAvailable(ctx context.Context, discoveryClient discovery.DiscoveryInterface) (bool, error) {
	gatewayAPIGroup := gatewayv1.GroupName

	serverGroups, err := discoveryClient.ServerGroups()
	if err != nil {
		return false, err
	}

	for _, group := range serverGroups.Groups {
		if group.Name == gatewayAPIGroup {
			return true, nil
		}
	}

	return false, nil
}

// GatewayKindAPIAvailable reports whether the given Gateway API kind is served at
// gateway.networking.k8s.io/v1 (the version this binary's gateway-api library
// speaks). A cluster can serve the group -- HTTPRoute, Gateway -- while still
// serving TLSRoute only as v1alpha2/v1alpha3 (Gateway API bundles < 1.5), and
// registering a watch for a kind the API server does not serve at v1 makes the
// controller-runtime cache sync time out and the manager exit. Check per kind.
func GatewayKindAPIAvailable(_ context.Context, discoveryClient discovery.DiscoveryInterface, kind string) (bool, error) {
	gv := gatewayv1.GroupVersion

	resourceList, err := discoveryClient.ServerResourcesForGroupVersion(gv.String())
	if err != nil {
		return false, err
	}

	for _, resource := range resourceList.APIResources {
		if resource.Kind == kind {
			return true, nil
		}
	}

	return false, nil
}

// TLSRouteAPIAvailable checks specifically for TLSRoute resource availability.
func TLSRouteAPIAvailable(ctx context.Context, discoveryClient discovery.DiscoveryInterface) (bool, error) {
	return GatewayKindAPIAvailable(ctx, discoveryClient, "TLSRoute")
}

// IsGatewayKindAvailable checks if the given Gateway API kind is served at v1,
// with fallback to a client-based check.
func IsGatewayKindAvailable(ctx context.Context, c client.Client, discoveryClient discovery.DiscoveryInterface, kind string) bool {
	if discoveryClient == nil {
		return IsGatewayKindAvailableViaClient(ctx, c, kind)
	}

	available, err := GatewayKindAPIAvailable(ctx, discoveryClient, kind)
	if err != nil {
		return false
	}

	return available
}

// IsTLSRouteAvailable checks if TLSRoute is available with fallback to client-based check.
func IsTLSRouteAvailable(ctx context.Context, c client.Client, discoveryClient discovery.DiscoveryInterface) bool {
	return IsGatewayKindAvailable(ctx, c, discoveryClient, "TLSRoute")
}

// IsGatewayKindAvailableViaClient uses the client's RESTMapper to check whether
// the given Gateway API kind resolves at v1.
func IsGatewayKindAvailableViaClient(_ context.Context, c client.Client, kind string) bool {
	gvk := schema.GroupVersionKind{
		Group:   gatewayv1.GroupName,
		Version: gatewayv1.GroupVersion.Version,
		Kind:    kind,
	}

	restMapper := c.RESTMapper()
	_, err := restMapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		if meta.IsNoMatchError(err) {
			return false
		}
		// Other errors might be transient, assume available
		return true
	}

	return true
}

// IsTLSRouteAvailableViaClient uses client to check TLSRoute availability.
func IsTLSRouteAvailableViaClient(ctx context.Context, c client.Client) bool {
	return IsGatewayKindAvailableViaClient(ctx, c, "TLSRoute")
}

// IsGatewayAPIAvailableViaClient uses client to check Gateway API availability.
func IsGatewayAPIAvailableViaClient(ctx context.Context, c client.Client) bool {
	return IsTLSRouteAvailableViaClient(ctx, c)
}
