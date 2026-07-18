package soot

import (
	"testing"

	kamajiv1alpha1 "github.com/clastix/kamaji/api/v1alpha1"
	"github.com/clastix/kamaji/internal/utilities"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestTenantEndpointObservationUsesExactTransitionToken(t *testing.T) {
	tcp := &kamajiv1alpha1.TenantControlPlane{}
	if got := tenantEndpointObservation(tcp); got != utilities.EndpointModeExternal {
		t.Fatalf("legacy observation = %q", got)
	}

	tcp.ObjectMeta = metav1.ObjectMeta{Annotations: map[string]string{
		utilities.TenantClientEndpointAnnotation:      utilities.TenantClientEndpointClusterIP,
		utilities.TenantClientEndpointTokenAnnotation: "transition-42",
	}}
	if got := tenantEndpointObservation(tcp); got != "transition-42" {
		t.Fatalf("direct observation = %q", got)
	}

	delete(tcp.Annotations, utilities.TenantClientEndpointTokenAnnotation)
	if got := tenantEndpointObservation(tcp); got == "" || got == utilities.EndpointModeExternal {
		t.Fatalf("missing-token direct observation must fail closed, got %q", got)
	}
}
