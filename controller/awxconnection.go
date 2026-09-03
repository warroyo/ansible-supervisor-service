package main

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
)

// connectionFieldManager owns status.apiBasePath. Separate from
// StatusFieldManager for the same reason as detailsFieldManager: the
// generic state/message/ready patch must not clobber it.
const connectionFieldManager = "ansible-supervisor-connection"

// awxClientFor builds a client for a connection, resolving which API root
// the instance serves: an explicit spec.apiBasePath wins, then the path
// already detected and cached in status, and only failing both does it
// probe the instance.
func awxClientFor(conn AWXConnection, token string) (*AWXClient, string, error) {
	basePath := ""
	if conn.Spec != nil {
		basePath = normalizeAPIBasePath(conn.Spec.APIBasePath)
	}
	if basePath == "" && conn.Status != nil {
		basePath = normalizeAPIBasePath(conn.Status.APIBasePath)
	}
	if basePath == "" {
		detected, err := DetectAPIBasePath(context.Background(), conn.Spec.URL, conn.Spec.InsecureSkipVerify)
		if err != nil {
			return nil, "", err
		}
		basePath = detected
	}
	return NewAWXClient(conn.Spec.URL, basePath, token, conn.Spec.InsecureSkipVerify), basePath, nil
}

// applyAWXConnection works out which API flavor this instance is (AWX /
// Tower / AAP <= 2.4 serve /api/v2, AAP 2.5+ serves /api/controller/v2)
// and validates that AWX accepts the credentials, so a broken connection
// surfaces as Failed on the AWXConnection itself rather than on every
// AnsibleBinding that references it.
func applyAWXConnection(client *dynamic.DynamicClient, obj interface{}, _ []string) error {
	u, err := toUnstructured(obj)
	if err != nil {
		return err
	}
	conn, err := convertAWXConnection(u)
	if err != nil {
		return fmt.Errorf("decoding AWXConnection: %w", err)
	}
	if conn.Spec == nil {
		return fmt.Errorf("spec is required")
	}
	if conn.Spec.URL == "" {
		return fmt.Errorf("spec.url is required")
	}
	if conn.Spec.SecretRef == "" {
		return fmt.Errorf("spec.secretRef is required")
	}

	token, err := getSecretValue(client, conn.Namespace, conn.Spec.SecretRef, "token")
	if err != nil {
		return fmt.Errorf("reading token from secret %q: %w", conn.Spec.SecretRef, err)
	}

	awxClient, basePath, err := awxClientFor(conn, token)
	if err != nil {
		return fmt.Errorf("resolving the API base path for %s: %w", conn.Spec.URL, err)
	}
	if err := awxClient.Ping(context.Background()); err != nil {
		return fmt.Errorf("validating connection to %s (API base path %s): %w", conn.Spec.URL, basePath, err)
	}

	// Cache it so every reconcile of every binding doesn't re-probe.
	if conn.Status == nil || conn.Status.APIBasePath != basePath {
		if err := writeAWXConnectionDetails(client, u, basePath); err != nil {
			return fmt.Errorf("recording the detected API base path: %w", err)
		}
	}
	return nil
}

func writeAWXConnectionDetails(client *dynamic.DynamicClient, obj *unstructured.Unstructured, basePath string) error {
	statusData := map[string]interface{}{"apiBasePath": basePath}
	return patchStatus(context.Background(), client, awxConnGVR, obj, statusData, connectionFieldManager)
}

// cleanupAWXConnection has nothing external to clean up - the Secret it
// references belongs to the user, not to this controller.
func cleanupAWXConnection(_ *dynamic.DynamicClient, _ interface{}) error {
	return nil
}
