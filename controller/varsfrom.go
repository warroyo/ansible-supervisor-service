package main

import (
	"context"
	"encoding/json"
	"errors"
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
// vars_from_resources package value, since "" is not a legible thing to
// put in a config list.
const coreGroupAlias = "core"

// varsFromResource is one "<group>/<resource>" pair varsFrom may read.
// Resource is the plural the RESTMapper resolves a kind to, which is what
// an RBAC rule names.
type varsFromResource struct {
	Group    string
	Resource string
}

// defaultVarsFromResources is what --vars-from-resources holds when
// nothing sets it, and matches vars_from_resources in config/values.yml.
const defaultVarsFromResources = coreGroupAlias + "/configmaps," + vmGroup + "/virtualmachines"

// allowedVarsFromResources is the set of kinds varsFrom may read,
// resolved once at startup from --vars-from-resources.
//
// The controller reads with its own identity rather than the requesting
// user's, so this list is the blast radius: anything of a listed kind in
// the AnsibleRun's own namespace is readable by anyone who can create an
// AnsibleRun there. Doing it properly would mean impersonating the
// requesting user, which needs an identity that only a mutating webhook
// could capture, and this service runs no webhook. Naming the kinds one
// by one is what keeps that radius the size of this list - and it is not
// optional on the core group, where a Supervisor's own admission policy
// refuses a service ClusterRole that wildcards it outright.
//
// It is the same list the ClusterRole grants, passed in so a kind
// outside it is refused with an explanation rather than coming back
// Forbidden from a call the service was never granted. Secrets are
// refused whatever it says.
var allowedVarsFromResources []varsFromResource

func parseVarsFromResources(csv string) []varsFromResource {
	var out []varsFromResource
	for _, entry := range strings.Split(csv, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		group, resource, ok := strings.Cut(entry, "/")
		if !ok || group == "" || resource == "" {
			// Both halves are required. An entry naming a group with no
			// resource must not be read as "everything in that group":
			// the point of the list is that nothing is a wildcard. The
			// core group is spelled "core" here, never "".
			continue
		}
		if group == coreGroupAlias {
			group = ""
		}
		out = append(out, varsFromResource{Group: group, Resource: resource})
	}
	return out
}

func varsFromResourceAllowed(group, resource string) bool {
	for _, r := range allowedVarsFromResources {
		if r.Group == group && r.Resource == resource {
			return true
		}
	}
	return false
}

// varsFromResourceList spells the allowlist back out for an error
// message, in the same "<group>/<resource>" form the package value uses.
func varsFromResourceList() []string {
	out := make([]string, 0, len(allowedVarsFromResources))
	for _, r := range allowedVarsFromResources {
		out = append(out, groupLabel(r.Group)+"/"+r.Resource)
	}
	return out
}

// varsFromGroupAllowed reports whether any kind in this group is
// readable. The group is checked before the kind is resolved to a
// resource, so a reference to a group nothing is granted in fails
// without a discovery lookup.
func varsFromGroupAllowed(group string) bool {
	for _, r := range allowedVarsFromResources {
		if r.Group == group {
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
				"read anything in (allowed: %s); an operator can widen it with the vars_from_resources package value",
				i, groupLabel(gv.Group), strings.Join(varsFromResourceList(), ", "))
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
			if errors.Is(vErr, errNotPopulated) {
				// Deliberately not terminal, for the same reason an
				// absent object is not: the field is one the object's
				// own controller fills in later. The commonest case is
				// the one this run exists for - a VirtualMachine whose
				// guest has not reported an IP yet - and failing it here
				// made the run permanently unrunnable seconds before the
				// value it wanted appeared.
				return nil, nil, fmt.Errorf("spec.varsFrom[%d].vars[%q] against %s %q: %w",
					i, key, ref.Kind, ref.Name, vErr)
			}
			if vErr != nil {
				return nil, nil, terminalf("spec.varsFrom[%d].vars[%q] against %s %q: %v", i, key, ref.Kind, ref.Name, vErr)
			}
			resolved[key] = value
		}
	}

	names = sortedKeys(resolved)
	return resolved, names, nil
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
	// Every kind is granted by name, so this is checked here rather than
	// with the group: the kind has to be resolved to a resource first.
	// Nothing is ever granted by wildcard, so a kind in an allowed group
	// still has to be one of the kinds that group was allowed for.
	if !varsFromResourceAllowed(gv.Group, mapping.Resource.Resource) {
		return nil, terminalf("varsFrom reads %s/%s, which this service is not permitted to read "+
			"(allowed: %s); an operator can widen it with the vars_from_resources package value",
			groupLabel(gv.Group), mapping.Resource.Resource, strings.Join(varsFromResourceList(), ", "))
	}

	obj, err := client.Resource(mapping.Resource).Namespace(namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		// Deliberately not terminal: the object may not exist yet.
		return nil, fmt.Errorf("reading %s %q for spec.varsFrom: %w", ref.Kind, ref.Name, err)
	}
	return obj, nil
}

// errNotPopulated marks a path that resolved to nothing on an object
// that does exist: the field is absent or null right now, which for a
// status field usually means its controller has not filled it in yet.
//
// It is separated from a path that is malformed, or that matched
// something of the wrong shape, because only those two are settled
// facts about an immutable spec. "Not there yet" is the state of the
// cluster, and spec.activeDeadlineSeconds is what bounds waiting on it.
var errNotPopulated = errors.New("nothing is there yet")

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
		// A field that is simply absent lands here, not in the empty
		// result below: the evaluator reports a missing key as an error.
		// So does a path naming a field that will never exist, and the
		// two are indistinguishable from here - which is why waiting is
		// bounded by a deadline rather than by this check.
		return "", fmt.Errorf("%q did not match (%v): %w", path, err, errNotPopulated)
	}

	var values []reflect.Value
	for _, r := range results {
		values = append(values, r...)
	}
	if len(values) == 0 {
		return "", fmt.Errorf("%q matched nothing: %w", path, errNotPopulated)
	}
	if len(values) > 1 {
		return "", fmt.Errorf("%q matched %d values; extra variables are single strings", path, len(values))
	}
	return scalarString(path, values[0])
}

func scalarString(path string, v reflect.Value) (string, error) {
	for v.Kind() == reflect.Interface || v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return "", fmt.Errorf("%q resolved to null: %w", path, errNotPopulated)
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
