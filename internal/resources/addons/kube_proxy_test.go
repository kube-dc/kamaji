// Copyright 2022 Clastix Labs
// SPDX-License-Identifier: Apache-2.0

package addons

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	kamajiv1alpha1 "github.com/clastix/kamaji/api/v1alpha1"
	"github.com/clastix/kamaji/internal/constants"
)

const baseKubeProxyConfig = `apiVersion: kubeproxy.config.k8s.io/v1alpha1
kind: KubeProxyConfiguration
metricsBindAddress: 127.0.0.1:10249
`

const baseKubeProxyConfigNoMetrics = `apiVersion: kubeproxy.config.k8s.io/v1alpha1
kind: KubeProxyConfiguration
mode: iptables
`

func newKubeProxy(configConf string) *KubeProxy {
	return &KubeProxy{
		configMap: &corev1.ConfigMap{Data: map[string]string{kubeProxyConfigConfKey: configConf}},
		daemonSet: &appsv1.DaemonSet{},
	}
}

// rawJSON wraps a JSON-encoded value for a JSONPatch (the value field is raw JSON,
// so a string must include its quotes).
func rawJSON(s string) *apiextensionsv1.JSON { return &apiextensionsv1.JSON{Raw: []byte(s)} }

func TestApplyConfigJSONPatches_ReplacesExistingMetricsBindAddress(t *testing.T) {
	k := newKubeProxy(baseKubeProxyConfig)
	err := k.applyConfigJSONPatches(kamajiv1alpha1.JSONPatches{
		{Op: "add", Path: "/metricsBindAddress", Value: rawJSON(`"0.0.0.0:10249"`)},
	})
	if err != nil {
		t.Fatalf("applyConfigJSONPatches: %v", err)
	}
	got := k.configMap.Data[kubeProxyConfigConfKey]
	if !strings.Contains(got, "metricsBindAddress: 0.0.0.0:10249") {
		t.Errorf("metricsBindAddress not updated to 0.0.0.0:10249, got:\n%s", got)
	}
	if strings.Contains(got, "127.0.0.1:10249") {
		t.Errorf("old metricsBindAddress 127.0.0.1 still present:\n%s", got)
	}
}

func TestApplyConfigJSONPatches_AddsMissingMetricsBindAddress(t *testing.T) {
	k := newKubeProxy(baseKubeProxyConfigNoMetrics)
	err := k.applyConfigJSONPatches(kamajiv1alpha1.JSONPatches{
		{Op: "add", Path: "/metricsBindAddress", Value: rawJSON(`"0.0.0.0:10249"`)},
	})
	if err != nil {
		t.Fatalf("applyConfigJSONPatches: %v", err)
	}
	if !strings.Contains(k.configMap.Data[kubeProxyConfigConfKey], "metricsBindAddress: 0.0.0.0:10249") {
		t.Errorf("metricsBindAddress not added:\n%s", k.configMap.Data[kubeProxyConfigConfKey])
	}
}

func TestApplyConfigJSONPatches_EmptyIsNoOp(t *testing.T) {
	k := newKubeProxy(baseKubeProxyConfig)
	before := k.configMap.Data[kubeProxyConfigConfKey]
	if err := k.applyConfigJSONPatches(nil); err != nil {
		t.Fatalf("empty patches should be a no-op, got: %v", err)
	}
	if k.configMap.Data[kubeProxyConfigConfKey] != before {
		t.Errorf("empty patch list mutated config.conf")
	}
}

func TestApplyConfigJSONPatches_InvalidPatchErrorsAndLeavesConfig(t *testing.T) {
	k := newKubeProxy(baseKubeProxyConfig)
	before := k.configMap.Data[kubeProxyConfigConfKey]
	// `replace` on a path that does not exist must fail (RFC-6902).
	err := k.applyConfigJSONPatches(kamajiv1alpha1.JSONPatches{
		{Op: "replace", Path: "/doesNotExist", Value: rawJSON(`"x"`)},
	})
	if err == nil {
		t.Fatal("expected error for replace on a missing path, got nil")
	}
	if k.configMap.Data[kubeProxyConfigConfKey] != before {
		t.Errorf("config.conf must be unchanged when a patch fails:\n%s", k.configMap.Data[kubeProxyConfigConfKey])
	}
}

func TestApplyConfigJSONPatches_MissingConfigConfKeyErrors(t *testing.T) {
	k := &KubeProxy{configMap: &corev1.ConfigMap{Data: map[string]string{}}}
	if err := k.applyConfigJSONPatches(kamajiv1alpha1.JSONPatches{
		{Op: "add", Path: "/metricsBindAddress", Value: rawJSON(`"0.0.0.0:10249"`)},
	}); err == nil {
		t.Fatal("expected error when config.conf key is absent")
	}
}

func TestStampConfigChecksumOnDaemonSet_ChangesWithConfig(t *testing.T) {
	k1 := newKubeProxy(baseKubeProxyConfig)
	k1.stampConfigChecksumOnDaemonSet()
	sum1 := k1.daemonSet.Spec.Template.Annotations[constants.Checksum]
	if sum1 == "" {
		t.Fatal("checksum annotation not stamped on the DaemonSet pod template")
	}

	// A different config.conf must yield a different checksum (so the DS rolls).
	k2 := newKubeProxy(strings.ReplaceAll(baseKubeProxyConfig, "127.0.0.1", "0.0.0.0"))
	k2.stampConfigChecksumOnDaemonSet()
	sum2 := k2.daemonSet.Spec.Template.Annotations[constants.Checksum]
	if sum1 == sum2 {
		t.Errorf("checksum did not change when config.conf changed (DS would not roll): %q", sum1)
	}

	// Same config => same checksum (stable, no reconcile churn).
	k3 := newKubeProxy(baseKubeProxyConfig)
	k3.stampConfigChecksumOnDaemonSet()
	if k3.daemonSet.Spec.Template.Annotations[constants.Checksum] != sum1 {
		t.Errorf("checksum not stable across identical config")
	}
}
