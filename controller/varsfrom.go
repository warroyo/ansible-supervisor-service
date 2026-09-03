package main

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/util/jsonpath"
)

// coreGroupAlias is how the core API group is spelled in the
// vars_from_api_groups package value, since "" is not a legible thing to
// put in a config list.
const coreGroupAlias = "core"

// allowedVarsFromGroups is the set of API groups varsFrom may read,
// resolved once at startup from --vars-from-api-groups.
//
// The controller reads with its own identity rather than the requesting
// user's, so this list is the blast radius: anything of an allowed kind
// in the AnsibleRun's own namespace is readable by anyone who can create
// an AnsibleRun there. Namespace-scoped RBAC on Supervisor generally
// already grants a tenant that much, and Secrets - the usual exception -
// are refused outright below. Doing it properly would mean impersonating
// the requesting user, which needs an identity that only a mutating
// webhook could capture, and this service runs no webhook.
var allowedVarsFromGroups []string

func parseVarsFromGroups(csv string) []string {
	var groups []string
	for _, g := range strings.Split(csv, ",") {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		if g == coreGroupAlias {
			g = ""
		}
		groups = append(groups, g)
	}
	return groups
}

func varsFromGroupAllowed(group string) bool {
	for _, g := range allowedVarsFromGroups {
		if g == group {
			return true
		}
	}
	return false
}

func groupLabel(group string) string {
	if group == "" {
		return coreGroupAlias
	}
	return group
}

// resolveVarsFrom fetches each source and evaluates its JSONPaths into
// extra_vars, returning the resolved pairs and their names in sorted
// order.
//
// Every failure here is terminal except a referenced object that does
// not exist yet, which is left retryable: an orchestrator may create the
// run before the object it names has settled. spec.activeDeadlineSeconds
// is what stops that waiting forever.
func resolveVarsFrom(
	ctx context.Context,
	client dynamic.Interface,
	mapper meta.RESTMapper,
	namespace string,
	sources []VarsFromSource,
	extraVars map[string]string,
) (resolved map[string]string, names []string, err error) {
	resolved = map[string]string{}

	for i := range sources {
		src := sources[i]
		ref := src.Resource
		if ref.APIVersion == "" || ref.Kind == "" || ref.Name == "" {
			return nil, nil, terminalf("spec.varsFrom[%d].resource needs apiVersion, kind and name", i)
		}
		if len(src.Vars) == 0 {
			return nil, nil, terminalf("spec.varsFrom[%d].vars must not be empty", i)
		}

		gv, pErr := schema.ParseGroupVersion(ref.APIVersion)
		if pErr != nil {
			return nil, nil, terminalf("spec.varsFrom[%d].resource.apiVersion %q is not valid: %v", i, ref.APIVersion, pErr)
		}

		// Secrets are refused whatever group they claim to be in.
		// extra_vars are echoed in AWX job output and kept in the job's
		// stored launch parameters, so sourcing a Secret through them is
		// a credential leak with extra steps. AWX Credentials attached to
		// the template are the mechanism for this.
		if ref.Kind == "Secret" {
			return nil, nil, terminalf("spec.varsFrom[%d] reads a Secret: extra variables are visible in AWX job "+
				"output and stored launch parameters, so Secrets are never sourced this way - attach an AWX "+
				"Credential to the template instead", i)
		}
		if !varsFromGroupAllowed(gv.Group) {
			return nil, nil, terminalf("spec.varsFrom[%d] reads API group %q, which this service is not permitted to "+
				"read (allowed: %s); an operator can widen it with the vars_from_api_groups package value",
				i, groupLabel(gv.Group), strings.Join(groupLabelList(), ", "))
		}

		obj, fErr := getVarsFromObject(ctx, client, mapper, namespace, gv, ref)
		if fErr != nil {
			return nil, nil, fErr
		}

		for _, key := range sortedKeys(src.Vars) {
			if _, clash := extraVars[key]; clash {
				return nil, nil, terminalf("spec.varsFrom[%d].vars key %q is already set in spec.extraVars: "+
					"remove one rather than relying on which wins", i, key)
			}
			if _, clash := resolved[key]; clash {
				return nil, nil, terminalf("spec.varsFrom sets %q more than once", key)
			}
			value, vErr := evalJSONPath(obj, src.Vars[key])
			if vErr != nil {
				return nil, nil, terminalf("spec.varsFrom[%d].vars[%q] against %s %q: %v", i, key, ref.Kind, ref.Name, vErr)
			}
			resolved[key] = value
		}
	}

	names = sortedKeys(resolved)
	return resolved, names, nil
}

func groupLabelList() []string {
	out := make([]string, 0, len(allowedVarsFromGroups))
	for _, g := range allowedVarsFromGroups {
		out = append(out, groupLabel(g))
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// getVarsFromObject resolves the kind to a resource and fetches it from
// the run's own namespace. A kind the cluster does not serve is terminal;
// an object that is merely absent is not, so it can appear later.
func getVarsFromObject(
	ctx context.Context,
	client dynamic.Interface,
	mapper meta.RESTMapper,
	namespace string,
	gv schema.GroupVersion,
	ref ResourceRef,
) (*unstructured.Unstructured, error) {
	gk := schema.GroupKind{Group: gv.Group, Kind: ref.Kind}

	mapping, err := mapper.RESTMapping(gk, gv.Version)
	if meta.IsNoMatchError(err) {
		// The mapper caches discovery, so a CRD installed after this
		// process started looks missing until the cache is dropped.
		if resettable, ok := mapper.(interface{ Reset() }); ok {
			resettable.Reset()
			mapping, err = mapper.RESTMapping(gk, gv.Version)
		}
	}
	if err != nil {
		return nil, terminalf("resolving %s %s: %v", ref.APIVersion, ref.Kind, err)
	}
	if mapping.Scope.Name() != meta.RESTScopeNameNamespace {
		return nil, terminalf("%s %s is cluster-scoped: varsFrom only reads objects in the AnsibleRun's own namespace",
			ref.APIVersion, ref.Kind)
	}

	obj, err := client.Resource(mapping.Resource).Namespace(namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		// Deliberately not terminal: the object may not exist yet.
		return nil, fmt.Errorf("reading %s %q for spec.varsFrom: %w", ref.Kind, ref.Name, err)
	}
	return obj, nil
}

// evalJSONPath evaluates one JSONPath template against an object and
// coerces the result to a string.
//
// Only scalars are accepted. extraVars is map[string]string, and silently
// JSON-encoding a list or an object into it would hand the playbook a
// string where it expected structure - better to say so than to let the
// playbook fail somewhere further away.
func evalJSONPath(obj *unstructured.Unstructured, path string) (string, error) {
	jp := jsonpath.New("varsFrom")
	if err := jp.Parse(path); err != nil {
		return "", fmt.Errorf("%q is not a valid JSONPath: %w", path, err)
	}
	results, err := jp.FindResults(obj.Object)
	if err != nil {
		return "", fmt.Errorf("%q did not match: %w", path, err)
	}

	var values []reflect.Value
	for _, r := range results {
		values = append(values, r...)
	}
	if len(values) == 0 {
		return "", fmt.Errorf("%q matched nothing", path)
	}
	if len(values) > 1 {
		return "", fmt.Errorf("%q matched %d values; extra variables are single strings", path, len(values))
	}
	return scalarString(path, values[0])
}

func scalarString(path string, v reflect.Value) (string, error) {
	for v.Kind() == reflect.Interface || v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return "", fmt.Errorf("%q resolved to null", path)
		}
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.String:
		return v.String(), nil
	case reflect.Bool:
		return fmt.Sprint(v.Bool()), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return fmt.Sprint(v.Int()), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return fmt.Sprint(v.Uint()), nil
	case reflect.Float32, reflect.Float64:
		// Whole floats print as "8080", not "8080.000000": unstructured
		// decodes every JSON number to float64, so an integer field would
		// otherwise reach the playbook in a shape it does not expect.
		return string(mustJSON(v.Float())), nil
	default:
		return "", fmt.Errorf("%q resolved to a %s; extra variables must be scalars", path, v.Kind())
	}
}

func mustJSON(f float64) []byte {
	b, err := json.Marshal(f)
	if err != nil {
		return []byte(fmt.Sprint(f))
	}
	return b
}
