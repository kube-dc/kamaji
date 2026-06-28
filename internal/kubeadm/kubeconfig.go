// Copyright 2022 Clastix Labs
// SPDX-License-Identifier: Apache-2.0

package kubeadm

import (
	"bytes"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"time"

	kubeadmconstants "k8s.io/kubernetes/cmd/kubeadm/app/constants"
	"k8s.io/kubernetes/cmd/kubeadm/app/phases/kubeconfig"

	"github.com/clastix/kamaji/internal/crypto"
	"github.com/clastix/kamaji/internal/utilities"
)

func buildCertificateDirectoryWithCA(ca CertificatePrivateKeyPair, directory string) error {
	if err := os.MkdirAll(directory, os.FileMode(0o755)); err != nil {
		return err
	}

	certPath := path.Join(directory, kubeadmconstants.CACertName)
	if err := os.WriteFile(certPath, ca.Certificate, os.FileMode(0o600)); err != nil {
		return err
	}

	keyPath := path.Join(directory, kubeadmconstants.CAKeyName)

	return os.WriteFile(keyPath, ca.PrivateKey, os.FileMode(0o600))
}

func CreateKubeconfig(kubeconfigName string, ca CertificatePrivateKeyPair, config *Configuration) ([]byte, error) {
	if err := buildCertificateDirectoryWithCA(ca, config.InitConfiguration.CertificatesDir); err != nil {
		return nil, err
	}

	defer deleteCertificateDirectory(config.InitConfiguration.CertificatesDir)

	if err := kubeconfig.CreateKubeConfigFile(kubeconfigName, config.InitConfiguration.CertificatesDir, &config.InitConfiguration); err != nil {
		return nil, err
	}

	path := filepath.Join(config.InitConfiguration.CertificatesDir, kubeconfigName)

	return os.ReadFile(path)
}

func IsKubeconfigCAValid(in, caCrt []byte) bool {
	kc, err := utilities.DecodeKubeconfigYAML(in)
	if err != nil {
		return false
	}

	for _, cluster := range kc.Clusters {
		if !bytes.Equal(cluster.Cluster.CertificateAuthorityData, caCrt) {
			return false
		}
	}

	return true
}

func IsKubeconfigValid(bytes []byte, expirationThreshold time.Duration) bool {
	kc, err := utilities.DecodeKubeconfigYAML(bytes)
	if err != nil {
		return false
	}

	ok, _ := crypto.IsValidCertificateKeyPairBytes(kc.AuthInfos[0].AuthInfo.ClientCertificateData, kc.AuthInfos[0].AuthInfo.ClientKeyData, expirationThreshold)

	return ok
}

// IsKubeconfigServerMatching reports whether every cluster server in the
// kubeconfig points at the expected host:port endpoint.
//
// kamaji binds the control-plane endpoint into the kubeconfig at generation
// time and the checksum-based regeneration path can miss a later endpoint
// change (e.g. a public-hostname endpoint replaced by the LB IP), leaving a
// STALE server URL that silently breaks mgmt-side consumers — notably the CAPI
// control-plane provider, whose per-cluster kubeconfig is copied from this one.
// This lets the kubeconfig reconciler force a regenerate when the server drifts.
//
// It is conservative against churn: when it cannot determine the expected
// endpoint or parse the data, it returns true (treat as matching) so it never
// forces an unnecessary regeneration — the CA/expiry/checksum checks still own
// those cases.
func IsKubeconfigServerMatching(in []byte, expectedEndpoint string) bool {
	if expectedEndpoint == "" || len(in) == 0 {
		return true
	}

	kc, err := utilities.DecodeKubeconfigYAML(in)
	if err != nil || len(kc.Clusters) == 0 {
		return true
	}

	for _, cluster := range kc.Clusters {
		u, uErr := url.Parse(cluster.Cluster.Server)
		if uErr != nil || u.Host != expectedEndpoint {
			return false
		}
	}

	return true
}
