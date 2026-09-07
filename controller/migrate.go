package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
)

// The upgrade gate.
//
// An AnsibleBindingVM used to be named after its binding and its VM, so
// two bindings selecting one VM got a child each. It is now named after
// the VM alone, which is what makes the name a claim: Kubernetes lets
// one object hold it, so the create that succeeds is the arbitration.
//
// That is a rename, and a rename is not transparent here. A child from
// the old scheme owns an AWX inventory host and may have a job in
// flight; creating canonical children alongside those would put two
// owners on one VM - the exact thing the change exists to stop - and
// could launch a second provisioning run against a machine already
// being configured. Nor can this controller resolve them itself:
// deleting them runs their cleanup, which under cleanupPolicy: Delete
// deletes their AWX hosts, and that is an operator's decision.
//
// So the controller refuses to start while any remain, and says what
// they are. The drain is done with the previous version, which still
// knows how to finish those children's work.

// legacyChild is one AnsibleBindingVM this version will not manage.
type legacyChild struct {
	namespace   string
	name        string
	vmName      string
	binding     string
	reason      string
	terminating bool
	jobID       int64
}

func (l legacyChild) String() string {
	out := fmt.Sprintf("%s/%s (VM %q, binding %q): %s", l.namespace, l.name, l.vmName, l.binding, l.reason)
	if l.terminating {
		out += ", still terminating"
	}
	if l.jobID != 0 {
		out += fmt.Sprintf(", AWX job %d recorded", l.jobID)
	}
	return out
}

// findLegacyChildren lists every AnsibleBindingVM in the cluster and
// returns the ones this version cannot treat as claims.
//
// Read live from the API server rather than from an informer: this runs
// before the caches are started, and a decision to refuse to start on
// what a partly-filled cache happened to hold would be worse than no
// check at all.
func findLegacyChildren(ctx context.Context, client *dynamic.DynamicClient) ([]legacyChild, error) {
	list, err := client.Resource(ansBindVMGVR).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var out []legacyChild
	for i := range list.Items {
		item := &list.Items[i]
		child, cErr := convertAnsibleBindingVM(item)
		if cErr != nil || child.Spec == nil || child.Spec.VMName == "" {
			out = append(out, legacyChild{
				namespace: item.GetNamespace(), name: item.GetName(),
				reason:      "its spec cannot be read, so nothing can say which VM it claims",
				terminating: !item.GetDeletionTimestamp().IsZero(),
			})
			continue
		}
		entry := legacyChild{
			namespace: child.Namespace, name: child.Name,
			vmName: child.Spec.VMName, binding: child.Spec.BindingName,
			terminating: !child.DeletionTimestamp.IsZero(),
		}
		if child.Status != nil {
			entry.jobID = child.Status.LastJobID
		}
		switch {
		case child.Name != childName(child.Spec.VMName):
			entry.reason = "named for its binding and its VM, so it is not the claim on that VM"
		case child.Spec.BindingUID == "":
			entry.reason = "has no spec.bindingUID, so which incarnation of its binding owns it is unknown"
		default:
			continue
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].namespace != out[j].namespace {
			return out[i].namespace < out[j].namespace
		}
		return out[i].name < out[j].name
	})
	return out, nil
}

// legacyChildSampleLimit bounds how many are named in the refusal. The
// count is what matters; the sample is there to start the search.
const legacyChildSampleLimit = 10

// checkForLegacyChildren refuses to run against state this version
// cannot claim, and says what to do about it.
func checkForLegacyChildren(ctx context.Context, client *dynamic.DynamicClient) error {
	legacy, err := findLegacyChildren(ctx, client)
	if err != nil {
		// Not treated as "there are none": starting on an unread cluster
		// is what would create the duplicate owners.
		return fmt.Errorf("listing AnsibleBindingVMs to check for ones from an earlier version: %w", err)
	}
	if len(legacy) == 0 {
		return nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d AnsibleBindingVM(s) were created by an earlier version and cannot be managed by this one:\n", len(legacy))
	for i, l := range legacy {
		if i == legacyChildSampleLimit {
			fmt.Fprintf(&b, "  ... and %d more\n", len(legacy)-legacyChildSampleLimit)
			break
		}
		fmt.Fprintf(&b, "  %s\n", l)
	}
	b.WriteString("\nChildren are now named after the VM alone, so that the name is an exclusive claim on it. " +
		"Running both schemes at once would put two owners on one VM and could launch a second job against a machine already being configured.\n" +
		"\nTo upgrade: stop any source that recreates bindings (GitOps included), roll back to the previous controller image, " +
		"let its bindings finish or explicitly resolve any job still running, delete those bindings so their children and finalizers complete " +
		"(cleanupPolicy: Delete removes their AWX hosts; Retain leaves the hosts and their ownership markers in place), then deploy this version and recreate the bindings. " +
		"Recreated bindings provision again - this is not a silent migration.\n" +
		"Do not remove finalizers or delete these objects by hand to get past this check: that abandons AWX hosts and job state this controller can no longer account for.")
	return fmt.Errorf("%s", b.String())
}
