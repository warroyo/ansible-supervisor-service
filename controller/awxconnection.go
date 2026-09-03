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

// defaultCABundleKey is the Secret key a CA bundle is read from when
// spec.caBundleSecretRef.key is left empty.
const defaultCABundleKey = "ca.crt"

// awxTLSOptionsFor resolves how TLS should be handled for a connection,
// reading the CA bundle Secret when one is referenced.
func awxTLSOptionsFor(ctx context.Context, client *dynamic.DynamicClient, conn AWXConnection) (TLSOptions, error) {
	opts := TLSOptions{InsecureSkipVerify: conn.Spec.InsecureSkipVerify}
	ref := conn.Spec.CABundleSecretRef
	if ref == nil {
		return opts, nil
	}
	// Skipping verification while a CA is configured is contradictory,
	// and quietly letting insecure win would leave an operator believing
	// the CA is being checked when nothing is.
	if conn.Spec.InsecureSkipVerify {
		return TLSOptions{}, fmt.Errorf(
			"spec.insecureSkipVerify and spec.caBundleSecretRef are mutually exclusive: set one or the other: %w", errPermanentConfig)
	}
	if ref.Name == "" {
		return TLSOptions{}, fmt.Errorf("spec.caBundleSecretRef.name is required: %w", errPermanentConfig)
	}
	key := ref.Key
	if key == "" {
		key = defaultCABundleKey
	}
	bundle, err := getSecretValue(ctx, client, conn.Namespace, ref.Name, key)
	if err != nil {
		return TLSOptions{}, fmt.Errorf("reading the CA bundle from secret %q key %q: %w", ref.Name, key, err)
	}
	opts.CABundlePEM = bundle
	return opts, nil
}

// awxClientFor builds a client for a connection, resolving which API root
// the instance serves: an explicit spec.apiBasePath wins, then the path
// already detected and cached in status, and only failing both does it
// probe the instance.
func awxClientFor(ctx context.Context, client *dynamic.DynamicClient, conn AWXConnection, token string) (*AWXClient, string, error) {
	tlsOpts, err := awxTLSOptionsFor(ctx, client, conn)
	if err != nil {
		return nil, "", err
	}
	basePath := ""
	if conn.Spec != nil {
		basePath = normalizeAPIBasePath(conn.Spec.APIBasePath)
	}
	if basePath == "" && conn.Status != nil {
		basePath = normalizeAPIBasePath(conn.Status.APIBasePath)
	}
	if basePath == "" {
		detected, err := DetectAPIBasePath(ctx, conn.Spec.URL, tlsOpts)
		if err != nil {
			return nil, "", err
		}
		basePath = detected
	}
	awxClient, err := NewAWXClient(conn.Spec.URL, basePath, token, tlsOpts)
	if err != nil {
		return nil, "", err
	}
	return awxClient, basePath, nil
}

// applyAWXConnection works out which API flavor this instance is (AWX /
// Tower / AAP <= 2.4 serve /api/v2, AAP 2.5+ serves /api/controller/v2)
// and validates that AWX accepts the credentials, so a broken connection
// surfaces as Failed on the AWXConnection itself rather than on every
// AnsibleBinding that references it.
func applyAWXConnection(ctx context.Context, client *dynamic.DynamicClient, obj interface{}) error {
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

	token, err := getSecretValue(ctx, client, conn.Namespace, conn.Spec.SecretRef, "token")
	if err != nil {
		return fmt.Errorf("reading token from secret %q: %w", conn.Spec.SecretRef, err)
	}

	awxClient, basePath, err := awxClientFor(ctx, client, conn, token)
	if err != nil {
		return fmt.Errorf("preparing a client for %s: %w", conn.Spec.URL, err)
	}
	if err := awxClient.Ping(ctx); err != nil {
		return fmt.Errorf("validating connection to %s (API base path %s): %w", conn.Spec.URL, basePath, err)
	}

	// Cache it so every reconcile of every binding doesn't re-probe.
	if conn.Status == nil || conn.Status.APIBasePath != basePath {
		if err := writeAWXConnectionDetails(ctx, client, u, basePath); err != nil {
			return fmt.Errorf("recording the detected API base path: %w", err)
		}
	}
	return nil
}

func writeAWXConnectionDetails(ctx context.Context, client *dynamic.DynamicClient, obj *unstructured.Unstructured, basePath string) error {
	statusData := map[string]interface{}{"apiBasePath": basePath}
	return patchStatus(ctx, client, awxConnGVR, obj, statusData, connectionFieldManager)
}

// awxConnectionStaleFinalizer was set by earlier versions of this
// controller. An AWXConnection creates nothing outside Kubernetes - the
// Secret it references belongs to the user - so its cleanup was a no-op
// and the finalizer bought nothing while adding a way for a delete to
// hang if the controller was down. It is stripped wherever it is found.
const awxConnectionStaleFinalizer = "field.vmware.com/awx-connection-cleanup"
