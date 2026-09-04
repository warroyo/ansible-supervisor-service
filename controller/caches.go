package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/cache"
)

// The informer stores this process already maintains, shared with the
// reconcile paths that need to read a kind other than their own: a child
// reads its AWXConnection, a binding lists its children. Both informers
// run cluster-wide and are kept current by watch, so every read that
// goes through here is one the API server never sees.
//
// Set once at startup, before any worker runs. Nil in unit tests, which
// is why every reader below falls back to the API server rather than
// assuming a store is there.
var (
	awxConnStore   cache.Indexer
	ansBindVMStore cache.Indexer
)

// childrenByBindingIndex indexes AnsibleBindingVMs by
// "namespace/bindingName", taken from the label the parent stamps on
// every child it creates. It is what turns the parent's per-pass LIST of
// its children into a map lookup.
const childrenByBindingIndex = "childrenByBinding"

func childrenByBindingIndexFunc(obj interface{}) ([]string, error) {
	u, err := toUnstructured(obj)
	if err != nil {
		return nil, nil
	}
	binding := u.GetLabels()[BindingLabel]
	if binding == "" {
		return nil, nil
	}
	return []string{key(u.GetNamespace(), binding)}, nil
}

// getAWXConnection reads an AWXConnection through the informer store,
// falling back to the API server on a miss - a connection created
// moments ago may not have reached the cache yet, and "not in the cache"
// must not be reported as "does not exist".
func getAWXConnection(ctx context.Context, client *dynamic.DynamicClient, namespace, name string) (*unstructured.Unstructured, error) {
	if awxConnStore != nil {
		obj, exists, err := awxConnStore.GetByKey(key(namespace, name))
		if err == nil && exists {
			u, convErr := toUnstructured(obj)
			if convErr == nil {
				return u.DeepCopy(), nil
			}
		}
	}
	return client.Resource(awxConnGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
}

// awxClientCacheTTL bounds how long a built client is reused even if the
// AWXConnection has not changed. The cache key carries the connection's
// resourceVersion, so an edit invalidates it immediately - but a token
// rotated in the Secret alone changes nothing on the connection, and
// this is what stops that being cached for the life of the process.
const awxClientCacheTTL = 10 * time.Minute

type awxClientCacheEntry struct {
	client   *AWXClient
	basePath string
	expires  time.Time
}

var (
	awxClientMu    sync.Mutex
	awxClientCache = map[string]awxClientCacheEntry{}
)

// awxClientForConnection returns a client for a connection, building one
// only when the cache has nothing usable.
//
// Building a client is not free: it reads the token Secret, possibly a
// CA bundle Secret, and may probe the instance for its API base path -
// and http.Client is where the connection pool lives, so a fresh one per
// call means a fresh TLS handshake per call. With one object per VM that
// was paid once per VM per pass; now it is paid once per connection.
func awxClientForConnection(ctx context.Context, client *dynamic.DynamicClient, conn AWXConnection) (*AWXClient, string, error) {
	if conn.Spec == nil {
		return nil, "", fmt.Errorf("AWXConnection %q has no spec: %w", conn.Name, errPermanentConfig)
	}
	cacheKey := connectionKey(conn)

	awxClientMu.Lock()
	entry, ok := awxClientCache[cacheKey]
	awxClientMu.Unlock()
	if ok && time.Now().Before(entry.expires) {
		return entry.client, entry.basePath, nil
	}

	token, err := getSecretValue(ctx, client, conn.Namespace, conn.Spec.SecretRef, "token")
	if err != nil {
		return nil, "", fmt.Errorf("reading AWX token from secret %q: %w", conn.Spec.SecretRef, err)
	}
	awxClient, basePath, err := awxClientFor(ctx, client, conn, token)
	if err != nil {
		return nil, "", err
	}

	awxClientMu.Lock()
	// Entries for superseded resourceVersions are never looked up again,
	// so expiry is the only thing that removes them.
	now := time.Now()
	for k, e := range awxClientCache {
		if now.After(e.expires) {
			delete(awxClientCache, k)
		}
	}
	awxClientCache[cacheKey] = awxClientCacheEntry{client: awxClient, basePath: basePath, expires: now.Add(awxClientCacheTTL)}
	awxClientMu.Unlock()

	return awxClient, basePath, nil
}

// templateCacheTTL is deliberately short. A cached template carries
// ask_limit_on_launch with it, and that flag is the whole of what stops
// a run going against an entire inventory instead of one host - see
// resolveTemplateForLaunch, which never reads this cache at all.
const templateCacheTTL = 30 * time.Second

type templateCacheEntry struct {
	tmpl    AWXTemplate
	expires time.Time
}

var (
	templateMu    sync.Mutex
	templateCache = map[string]templateCacheEntry{}
)

// connectionKey identifies one version of one connection. The namespace
// is in it because AWXConnection is namespaced, and a key of
// connection-name alone collides across tenants - which for the template
// cache would hand one namespace another's template id. The
// resourceVersion is in it because a template id is only meaningful on
// the instance that issued it: edit spec.url and every id cached under
// the old key is about a different AWX.
func connectionKey(conn AWXConnection) string {
	return fmt.Sprintf("%s/%s/%s", conn.Namespace, conn.Name, conn.ResourceVersion)
}

func templateCacheKey(connKey string, ref TemplateRef) string {
	return fmt.Sprintf("%s/%s/%s", connKey, ref.Type, ref.Name)
}

// resolveTemplateCached resolves a template for work that only needs its
// inventory - the per-VM host check. Launches do not come through here.
func resolveTemplateCached(ctx context.Context, awxClient *AWXClient, connKey string, ref TemplateRef) (*AWXTemplate, error) {
	k := templateCacheKey(connKey, ref)

	templateMu.Lock()
	entry, ok := templateCache[k]
	templateMu.Unlock()
	if ok && time.Now().Before(entry.expires) {
		tmpl := entry.tmpl
		return &tmpl, nil
	}

	tmpl, err := resolveTemplate(ctx, awxClient, ref)
	if err != nil {
		return nil, err
	}
	cacheTemplate(k, tmpl)
	return tmpl, nil
}

// resolveTemplateForLaunch resolves a template the caller is about to
// launch, always from AWX. Prompt-on-launch can be switched off in the
// AWX UI at any moment, and checkTemplateLaunchFields is only as good as
// the flags it is given: a cached template would leave a window in which
// the controller launched a run AWX would silently widen to the whole
// inventory.
func resolveTemplateForLaunch(ctx context.Context, awxClient *AWXClient, connKey string, ref TemplateRef) (*AWXTemplate, error) {
	tmpl, err := resolveTemplate(ctx, awxClient, ref)
	if err != nil {
		return nil, err
	}
	cacheTemplate(templateCacheKey(connKey, ref), tmpl)
	return tmpl, nil
}

func cacheTemplate(k string, tmpl *AWXTemplate) {
	templateMu.Lock()
	defer templateMu.Unlock()
	now := time.Now()
	for existing, e := range templateCache {
		if now.After(e.expires) {
			delete(templateCache, existing)
		}
	}
	templateCache[k] = templateCacheEntry{tmpl: *tmpl, expires: now.Add(templateCacheTTL)}
}
