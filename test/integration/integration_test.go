// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package integration

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/gruntwork-io/terratest/modules/logger"
	"github.com/gruntwork-io/terratest/modules/retry"
	"github.com/gruntwork-io/terratest/modules/terraform"
	"github.com/hashicorp/vault/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlruntime "k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	secretsv1alpha1 "github.com/hashicorp/vault-secrets-operator/api/v1alpha1"
	"github.com/hashicorp/vault-secrets-operator/internal/helpers"
)

var (
	testRoot            string
	binDir              string
	chartPath           string
	testVaultAddress    string
	k8sVaultNamespace   string
	kustomizeConfigRoot string

	// extended in TestMain
	scheme = ctrlruntime.NewScheme()
	// set in TestMain
	restConfig = rest.Config{}
)

func init() {
	_, curFilePath, _, _ := runtime.Caller(0)
	testRoot = path.Dir(curFilePath)
	var err error
	binDir, err = filepath.Abs(filepath.Join(testRoot, "..", "..", "bin"))
	if err != nil {
		panic(err)
	}

	chartPath, err = filepath.Abs(filepath.Join(testRoot, "..", "..", "chart"))
	if err != nil {
		panic(err)
	}

	kustomizeConfigRoot, err = filepath.Abs(filepath.Join(testRoot, "..", "..", "config"))
	if err != nil {
		panic(err)
	}

	k8sVaultNamespace = os.Getenv("K8S_VAULT_NAMESPACE")
	if k8sVaultNamespace == "" {
		k8sVaultNamespace = "vault"
	}
	testVaultAddress = fmt.Sprintf("http://vault.%s.svc.cluster.local:8200", k8sVaultNamespace)
}

// testVaultAddress is the address in k8s of the vault setup by
// `make setup-integration-test{,-ent}`

// Set the environment variable INTEGRATION_TESTS to any non-empty value to run
// the tests in this package. The test assumes it has available:
// - kubectl
//   - A Kubernetes cluster in which:
//   - Vault is deployed and accessible
//
// See `make setup-integration-test` for manual testing.
func TestMain(m *testing.M) {
	if os.Getenv("INTEGRATION_TESTS") != "" {
		utilruntime.Must(clientgoscheme.AddToScheme(scheme))
		utilruntime.Must(secretsv1alpha1.AddToScheme(scheme))
		restConfig = *ctrl.GetConfigOrDie()

		os.Setenv("VAULT_ADDR", "http://127.0.0.1:38300")
		os.Setenv("VAULT_TOKEN", "root")
		os.Setenv("PATH", fmt.Sprintf("%s:%s", binDir, os.Getenv("PATH")))
		os.Exit(m.Run())
	}
}

func setCommonTFOptions(t *testing.T, opts *terraform.Options) *terraform.Options {
	t.Helper()
	if os.Getenv("SUPPRESS_TF_OUTPUT") != "" {
		opts.Logger = logger.Discard
	}
	return terraform.WithDefaultRetryableErrors(t, opts)
}

func getVaultClient(t *testing.T, namespace string) *api.Client {
	t.Helper()
	client, err := api.NewClient(nil)
	if err != nil {
		t.Fatal(err)
	}
	if namespace != "" {
		client.SetNamespace(namespace)
	}
	return client
}

func getCRDClient(t *testing.T) client.Client {
	// restConfig is set in TestMain for when running integration tests.
	t.Helper()

	k8sClient, err := client.New(&restConfig, client.Options{Scheme: scheme})
	require.NoError(t, err)

	return k8sClient
}

func waitForSecretData(t *testing.T, ctx context.Context, crdClient client.Client, maxRetries int, delay time.Duration,
	name, namespace string, expectedData map[string]interface{},
) (*corev1.Secret, error) {
	t.Helper()
	var validSecret corev1.Secret
	secObjKey := client.ObjectKey{Namespace: namespace, Name: name}
	_, err := retry.DoWithRetryE(t,
		fmt.Sprintf("wait for k8s Secret data to be synced by the operator, objKey=%s", secObjKey),
		maxRetries, delay, func() (string, error) {
			var err error
			var destSecret corev1.Secret
			if err := crdClient.Get(ctx, secObjKey, &destSecret); err != nil {
				return "", err
			}

			if _, ok := destSecret.Data["_raw"]; !ok {
				return "", fmt.Errorf("secret hasn't been synced yet, missing '_raw' field")
			}

			var rawSecret map[string]interface{}
			err = json.Unmarshal(destSecret.Data["_raw"], &rawSecret)
			require.NoError(t, err)
			if _, ok := rawSecret["data"]; ok {
				rawSecret = rawSecret["data"].(map[string]interface{})
			}
			for k, v := range expectedData {
				// compare expected secret data to _raw in the k8s secret
				if !reflect.DeepEqual(v, rawSecret[k]) {
					err = errors.Join(err, fmt.Errorf("expected data '%s:%s' missing from _raw: %#v", k, v, rawSecret))
				}
				// compare expected secret k/v to the top level items in the k8s secret
				if !reflect.DeepEqual(v, string(destSecret.Data[k])) {
					err = errors.Join(err, fmt.Errorf("expected '%s:%s', actual '%s:%s'", k, v, k, string(destSecret.Data[k])))
				}
			}
			if len(expectedData) != len(rawSecret) {
				err = errors.Join(err, fmt.Errorf("expected data length %d does not match _raw length %d", len(expectedData), len(rawSecret)))
			}
			// the k8s secret has an extra key because of the "_raw" item
			if len(expectedData) != len(destSecret.Data)-1 {
				err = errors.Join(err, fmt.Errorf("expected data length %d does not match k8s secret data length %d", len(expectedData), len(destSecret.Data)-1))
			}

			if err == nil {
				validSecret = destSecret
			}

			return "", err
		})

	return &validSecret, err
}

func waitForPKIData(t *testing.T, maxRetries int, delay time.Duration, name, namespace, expectedCommonName, previousSerialNumber string) (string, *corev1.Secret, error) {
	t.Helper()
	destSecret := &corev1.Secret{}
	newSerialNumber, err := retry.DoWithRetryE(t, "wait for k8s Secret data to be synced by the operator", maxRetries, delay, func() (string, error) {
		var err error
		destSecret, err = k8s.GetSecretE(t, &k8s.KubectlOptions{Namespace: namespace}, name)
		if err != nil {
			return "", err
		}
		if len(destSecret.Data) == 0 {
			return "", fmt.Errorf("data in secret %s/%s is empty: %#v", namespace, name, destSecret)
		}
		if len(destSecret.Data["certificate"]) == 0 {
			return "", fmt.Errorf("certificate is empty")
		}

		pem, rest := pem.Decode(destSecret.Data["certificate"])
		assert.Empty(t, rest)
		cert, err := x509.ParseCertificate(pem.Bytes)
		require.NoError(t, err)
		if cert.Subject.CommonName != expectedCommonName {
			return "", fmt.Errorf("subject common name %q does not match expected %q", cert.Subject.CommonName, expectedCommonName)
		}
		if cert.SerialNumber.String() == previousSerialNumber {
			return "", fmt.Errorf("serial number %q still matches previous serial number %q", cert.SerialNumber, previousSerialNumber)
		}

		return cert.SerialNumber.String(), nil
	})

	return newSerialNumber, destSecret, err
}

type dynamicK8SOutputs struct {
	NamePrefix       string   `json:"name_prefix"`
	Namespace        string   `json:"namespace"`
	K8sNamespace     string   `json:"k8s_namespace"`
	K8sConfigContext string   `json:"k8s_config_context"`
	AuthMount        string   `json:"auth_mount"`
	AuthPolicy       string   `json:"auth_policy"`
	AuthRole         string   `json:"auth_role"`
	DBRole           string   `json:"db_role"`
	DBPath           string   `json:"db_path"`
	TransitPath      string   `json:"transit_path"`
	TransitKeyName   string   `json:"transit_key_name"`
	TransitRef       string   `json:"transit_ref"`
	K8sDBSecrets     []string `json:"k8s_db_secret"`
}

func assertDynamicSecret(t *testing.T, maxRetries int, delay time.Duration, vdsObj *secretsv1alpha1.VaultDynamicSecret, expected map[string]int) {
	t.Helper()

	namespace := vdsObj.GetNamespace()
	name := vdsObj.Spec.Destination.Name
	opts := &k8s.KubectlOptions{
		Namespace: namespace,
	}
	retry.DoWithRetry(t,
		"wait for dynamic secret sync", maxRetries, delay,
		func() (string, error) {
			sec, err := k8s.GetSecretE(t, opts, name)
			if err != nil {
				return "", err
			}
			if len(sec.Data) == 0 {
				return "", fmt.Errorf("empty data for secret %s: %#v", sec, sec)
			}

			actual := make(map[string]int)
			for f, b := range sec.Data {
				actual[f] = len(b)
			}
			assert.Equal(t, expected, actual)

			assertSyncableSecret(t, vdsObj,
				"secrets.hashicorp.com/v1alpha1",
				"VaultDynamicSecret", sec)

			return "", nil
		})
}

func assertSyncableSecret(t *testing.T, obj client.Object, expectedAPIVersion, expectedKind string, sec *corev1.Secret) {
	t.Helper()

	meta, err := helpers.NewSyncableSecretMetaData(obj)
	require.NoError(t, err)

	if meta.Destination.Create {
		assert.Equal(t, helpers.OwnerLabels, sec.Labels,
			"expected owner labels not set on %s",
			client.ObjectKeyFromObject(sec))

		// check the OwnerReferences
		expectedOwnerRefs := []v1.OwnerReference{
			{
				// For some reason TypeMeta is empty when using the client.Client
				// from within the tests. So we have to hard code APIVersion and Kind.
				// There are numerous related GH issues for this:
				// Normally it should be:
				// APIVersion: meta.APIVersion,
				// Kind:       meta.Kind,
				// e.g. https://github.com/kubernetes/client-go/issues/541
				APIVersion: expectedAPIVersion,
				Kind:       expectedKind,
				Name:       obj.GetName(),
				UID:        obj.GetUID(),
			},
		}
		assert.Equal(t, expectedOwnerRefs, sec.OwnerReferences,
			"expected owner references not set on %s",
			client.ObjectKeyFromObject(sec))
	} else {
		assert.Nil(t, sec.Labels,
			"expected no labels set on %s",
			client.ObjectKeyFromObject(sec))
		assert.Nil(t, sec.OwnerReferences,
			"expected no OwnerReferences set on %s",
			client.ObjectKeyFromObject(sec))
	}
}

func deployOperatorWithKustomize(t *testing.T, k8sOpts *k8s.KubectlOptions, kustomizeConfigPath string) {
	// deploy the Operator with Kustomize
	t.Helper()
	k8s.KubectlApplyFromKustomize(t, k8sOpts, kustomizeConfigPath)
	retry.DoWithRetry(t, "waitOperatorPodReady", 30, time.Millisecond*500, func() (string, error) {
		return "", k8s.RunKubectlE(t, k8sOpts,
			"wait", "--for=condition=Ready",
			"--timeout=2m", "pod", "-l", "control-plane=controller-manager")
	},
	)
}