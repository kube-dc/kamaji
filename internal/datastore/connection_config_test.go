// Copyright 2022 Clastix Labs
// SPDX-License-Identifier: Apache-2.0

package datastore_test

import (
	"context"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kamajiv1alpha1 "github.com/clastix/kamaji/api/v1alpha1"
	"github.com/clastix/kamaji/internal/datastore"
)

func endpointStrings(cfg *datastore.ConnectionConfig) []string {
	out := make([]string, 0, len(cfg.Endpoints))
	for _, ep := range cfg.Endpoints {
		out = append(out, ep.String())
	}

	return out
}

// The controller-side connection must prefer spec.managementEndpoints when set:
// control planes may reach the datastore over a network path the controller
// cannot use. Rendered control-plane args keep using spec.endpoints.
func TestNewConnectionConfigPrefersManagementEndpoints(t *testing.T) {
	ds := kamajiv1alpha1.DataStore{
		Spec: kamajiv1alpha1.DataStoreSpec{
			Driver:              kamajiv1alpha1.EtcdDriver,
			Endpoints:           kamajiv1alpha1.Endpoints{"etcd-0.etcd.tenant.svc.cluster.local:2379"},
			ManagementEndpoints: kamajiv1alpha1.Endpoints{"etcd-lb-ext.tenant.svc.cluster.local:32380"},
		},
	}

	cfg, err := datastore.NewConnectionConfig(context.Background(), fake.NewClientBuilder().Build(), ds)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := endpointStrings(cfg)
	if len(got) != 1 || got[0] != "etcd-lb-ext.tenant.svc.cluster.local:32380" {
		t.Fatalf("managementEndpoints not preferred, got %v", got)
	}
}

// Backward compatibility: with no managementEndpoints, the controller keeps
// dialing spec.endpoints — the pre-field behavior, byte for byte.
func TestNewConnectionConfigFallsBackToEndpoints(t *testing.T) {
	ds := kamajiv1alpha1.DataStore{
		Spec: kamajiv1alpha1.DataStoreSpec{
			Driver:    kamajiv1alpha1.EtcdDriver,
			Endpoints: kamajiv1alpha1.Endpoints{"etcd-0.etcd.tenant.svc.cluster.local:2379", "etcd-1.etcd.tenant.svc.cluster.local:2379"},
		},
	}

	cfg, err := datastore.NewConnectionConfig(context.Background(), fake.NewClientBuilder().Build(), ds)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := endpointStrings(cfg)
	if len(got) != 2 || got[0] != "etcd-0.etcd.tenant.svc.cluster.local:2379" || got[1] != "etcd-1.etcd.tenant.svc.cluster.local:2379" {
		t.Fatalf("endpoints fallback broken, got %v", got)
	}
}
