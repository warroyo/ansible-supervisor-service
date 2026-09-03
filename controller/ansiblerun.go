package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
)

// runDetailsFieldManager owns everything in an AnsibleRun's status
// except the generic state/message/ready/lastUpdated the engine writes,
// so the two server-side applies merge instead of clobbering each other.
const runDetailsFieldManager = "ansible-supervisor-run"

// varsFromRESTMapper resolves the kinds spec.varsFrom names. Set once at
// startup; deferred and cache-backed, so a CRD installed later resolves
// after a reset rather than never.
var varsFromRESTMapper meta.RESTMapper

// terminalError marks an outcome no retry can change: the run is over,
// and status.finishedAt is stamped so the TTL can collect it.
//
// The distinction is the whole basis of an AnsibleRun's lifecycle. AWX
// being unreachable, or a referenced object that has not appeared yet,
// must keep retrying - abandoning those would fail runs over a blip. A
// playbook that failed, or a spec that cannot be satisfied, must not:
// the spec is immutable, so there is nothing to come back to.
type terminalError struct{ err error }

func (e *terminalError) Error() string { return e.err.Error() }
func (e *terminalError) Unwrap() error { return e.err }

func terminalf(format string, a ...interface{}) error {
	return &terminalError{fmt.Errorf(format, a...)}
}

func isTerminalError(err error) bool {
	var t *terminalError
	return errors.As(err, &t)
}

// applyAnsibleRun drives one run from creation to a terminal state and
// eventually to its own deletion.
//
// Unlike an AnsibleBinding, which is standing desired state, this is a
// one-way trip: launch at most one AWX job, poll it to a terminal
// status, then stop. A spec change cannot re-trigger it (the CRD makes
// spec immutable) and neither can the re-run annotation.
func applyAnsibleRun(ctx context.Context, client *dynamic.DynamicClient, obj interface{}) error {
	u, err := toUnstructured(obj)
	if err != nil {
		return err
	}
	run, err := convertAnsibleRun(u)
	if err != nil {
		return fmt.Errorf("decoding AnsibleRun: %w", err)
	}
	if run.Spec == nil {
		return fmt.Errorf("spec is required")
	}
	status := AnsibleRunStatus{}
	if run.Status != nil {
		status = *run.Status
	}

	// Already finished: nothing left but to collect it when its TTL is up.
	if status.FinishedAt != "" {
		return collectFinishedRun(ctx, client, &run, status)
	}

	// The deadline is checked before anything else so a run wedged on a
	// retryable condition - AWX down, a referenced object that never
	// appears, a job stuck non-terminal - still reaches an end state.
	if deadlineExceeded(&run) {
		return failRun(ctx, client, u, &status, fmt.Sprintf(
			"Run exceeded spec.activeDeadlineSeconds (%ds) before finishing.", run.Spec.ActiveDeadlineSeconds))
	}

	// A launch was attempted but no job ID came back, which means this
	// process died between the POST and recording its result. The job may
	// well be running in AWX. Launching again could run a decommission
	// playbook, or open a ticket, twice - so this fails and points a human
	// at AWX instead. An AnsibleBinding resolves the same window the other
	// way, because relaunching a convergent configuration run is safe.
	if status.LaunchAttemptedAt != "" && status.JobID == 0 {
		return failRun(ctx, client, u, &status, fmt.Sprintf(
			"A launch was sent at %s but its result was never recorded, so this run may or may not have started "+
				"in AWX. Check the template's recent jobs there, and create a new AnsibleRun if it did not run.",
			status.LaunchAttemptedAt))
	}

	// Everything below can fail terminally from several layers down, so
	// the classification is handled once, here: a terminal error stamps
	// finishedAt and the reason, and is not retried.
	rErr := reconcileRun(ctx, client, u, &run, &status)
	if isTerminalError(rErr) {
		return failRun(ctx, client, u, &status, rErr.Error())
	}
	return rErr
}

// reconcileRun polls an in-flight job, or does everything up to and
// including the single launch. It writes the detail half of status as it
// goes, so work already done - inventory hosts upserted, a job launched -
// survives a later failure.
func reconcileRun(ctx context.Context, client *dynamic.DynamicClient, u *unstructured.Unstructured, run *AnsibleRun, status *AnsibleRunStatus) error {
	awxClient, err := awxClientForRun(ctx, client, run)
	if err != nil {
		return err
	}
	if status.JobID != 0 {
		return pollRun(ctx, client, u, run, status, awxClient)
	}
	return launchRun(ctx, client, u, run, status, awxClient)
}

// awxClientForRun resolves the connection, its token and TLS settings.
// A missing or malformed AWXConnection is terminal - it is a spec error,
// and the spec cannot be edited to fix it.
func awxClientForRun(ctx context.Context, client *dynamic.DynamicClient, run *AnsibleRun) (*AWXClient, error) {
	conn, err := runConnection(ctx, client, run)
	if err != nil {
		return nil, err
	}
	token, err := getSecretValue(ctx, client, run.Namespace, conn.Spec.SecretRef, "token")
	if err != nil {
		if isPermanent(err) {
			return nil, terminalf("reading the AWX token from secret %q: %v", conn.Spec.SecretRef, err)
		}
		return nil, fmt.Errorf("reading the AWX token from secret %q: %w", conn.Spec.SecretRef, err)
	}
	awxClient, _, err := awxClientFor(ctx, client, conn, token)
	if err != nil {
		return nil, fmt.Errorf("preparing a client for AWXConnection %q: %w", run.Spec.AWXConnectionRef, err)
	}
	return awxClient, nil
}

func runConnection(ctx context.Context, client *dynamic.DynamicClient, run *AnsibleRun) (AWXConnection, error) {
	if run.Spec.AWXConnectionRef == "" {
		return AWXConnection{}, terminalf("spec.awxConnectionRef is required")
	}
	connObj, err := client.Resource(awxConnGVR).Namespace(run.Namespace).Get(ctx, run.Spec.AWXConnectionRef, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return AWXConnection{}, terminalf("AWXConnection %q does not exist in this namespace", run.Spec.AWXConnectionRef)
		}
		return AWXConnection{}, fmt.Errorf("fetching AWXConnection %q: %w", run.Spec.AWXConnectionRef, err)
	}
	conn, err := convertAWXConnection(connObj)
	if err != nil || conn.Spec == nil {
		return AWXConnection{}, terminalf("AWXConnection %q is malformed", run.Spec.AWXConnectionRef)
	}
	return conn, nil
}

// pollRun checks an in-flight job and records the outcome. Only a
// terminal AWX status ends the run.
func pollRun(ctx context.Context, client *dynamic.DynamicClient, u *unstructured.Unstructured, run *AnsibleRun, status *AnsibleRunStatus, awxClient *AWXClient) error {
	awxStatus, err := pollJobStatus(ctx, awxClient, run.Spec.Template.Type, status.JobID)
	if err != nil {
		return fmt.Errorf("polling job %d: %w", status.JobID, err)
	}
	status.JobStatus = awxStatus
	if !isTerminalAWXStatus(awxStatus) {
		return writeRunDetails(ctx, client, u, status)
	}
	if mapAWXStatus(awxStatus) == PhaseFailed {
		// The controller did its job here; AWX ran the playbook and it
		// failed. Terminal, but not a reconcile error to retry.
		return terminalf("AWX job %d finished %s: see %s", status.JobID, awxStatus, status.JobURL)
	}
	status.FinishedAt = nowRFC3339()
	return writeRunDetails(ctx, client, u, status)
}

// launchRun resolves the template, gathers variables, reconciles
// inventory hosts, then fires exactly once.
func launchRun(ctx context.Context, client *dynamic.DynamicClient, u *unstructured.Unstructured, run *AnsibleRun, status *AnsibleRunStatus, awxClient *AWXClient) error {
	spec := run.Spec

	if len(spec.Hosts) > 0 && spec.VMRef != nil {
		return terminalf("spec.hosts and spec.vmRef are mutually exclusive: a run points at explicit hosts or at one VM, not both")
	}

	var tmpl *AWXTemplate
	var err error
	switch spec.Template.Type {
	case TemplateTypeJob:
		tmpl, err = awxClient.FindJobTemplate(ctx, spec.Template.Name)
	case TemplateTypeWorkflow:
		tmpl, err = awxClient.FindWorkflowJobTemplate(ctx, spec.Template.Name)
	default:
		return terminalf("spec.template.type must be %q or %q, got %q", TemplateTypeJob, TemplateTypeWorkflow, spec.Template.Type)
	}
	if err != nil {
		// A named template that isn't there, or is ambiguous, is a spec
		// error rather than an outage: the spec cannot be edited to fix
		// it, so failing is more useful than retrying forever.
		return terminalf("resolving template %q: %v", spec.Template.Name, err)
	}

	extraVars, resolvedNames, err := gatherRunVars(ctx, client, run)
	if err != nil {
		return err
	}
	status.ResolvedVars = resolvedNames

	targets, err := runTargets(ctx, client, run)
	if err != nil {
		return err
	}
	if len(targets) > 0 && tmpl.Inventory == nil {
		return terminalf("template %q has no inventory, so there is nowhere to create the host(s) this run targets "+
			"and no inventory for a limit to scope against: point it at a template with an inventory, or drop "+
			"spec.hosts/spec.vmRef to accept the template's own scope", spec.Template.Name)
	}

	if err := checkTemplateAcceptsLaunchFields(tmpl, spec.Template.Name, len(targets) > 0, len(extraVars) > 0,
		"remove spec.hosts/spec.vmRef to accept the template's own scope"); err != nil {
		return &terminalError{err}
	}

	// Reconcile every target's inventory host before launching. Hosts
	// already upserted when a later one fails are recorded in status, so
	// they are cleaned up with the run rather than leaked.
	var limits []string
	for _, t := range targets {
		hostID, owned, hErr := upsertInventoryHost(ctx, awxClient, *tmpl.Inventory, t.Name,
			runHostOwnerMarker(run.Namespace, run.Name), t.Address, t.Variables)
		if hErr != nil {
			if wErr := writeRunDetails(ctx, client, u, status); wErr != nil {
				log.Printf("[AnsibleRun/%s/%s] could not record partial host state: %v", run.Namespace, run.Name, wErr)
			}
			return fmt.Errorf("upserting AWX host %q: %w", t.Name, hErr)
		}
		status.Hosts = append(status.Hosts, RunHostStatus{
			Name:           t.Name,
			Address:        t.Address,
			AWXHostID:      int64(hostID),
			AWXInventoryID: int64(*tmpl.Inventory),
			AWXHostCreated: owned,
		})
		limits = append(limits, t.Name)
	}

	// Record the attempt before making it. If this write fails there is no
	// job yet and the next pass simply retries; if it succeeds and the
	// launch result is then lost, the recovery path in applyAnsibleRun
	// refuses to launch a second time.
	status.LaunchAttemptedAt = nowRFC3339()
	if err := writeRunDetails(ctx, client, u, status); err != nil {
		return fmt.Errorf("recording the launch attempt: %w", err)
	}

	limit := strings.Join(limits, ",")
	var jobID int
	var lErr error
	if spec.Template.Type == TemplateTypeWorkflow {
		jobID, lErr = awxClient.LaunchWorkflowJobTemplate(ctx, tmpl.ID, limit, extraVars)
	} else {
		jobID, lErr = awxClient.LaunchJobTemplate(ctx, tmpl.ID, limit, extraVars)
	}

	if jobID != 0 {
		// Record the job even alongside an error (AWX ignored fields):
		// it is real, it is running, and it must stay traceable.
		status.JobID = int64(jobID)
		status.JobURL = awxClient.JobURL(jobID, spec.Template.Type == TemplateTypeWorkflow)
		status.JobStatus = "pending"
		status.StartedAt = nowRFC3339()
	}
	if lErr != nil {
		if jobID == 0 {
			// Nothing launched, so clear the attempt marker: an ordinary
			// failure to retry, not an outcome that was lost.
			status.LaunchAttemptedAt = ""
			if wErr := writeRunDetails(ctx, client, u, status); wErr != nil {
				return wErr
			}
			return fmt.Errorf("launching template %q: %w", spec.Template.Name, lErr)
		}
		// A job that launched with fields ignored ran with the wrong
		// scope. It cannot be un-run, so this ends here rather than
		// pretending the run did what was asked.
		return &terminalError{lErr}
	}
	return writeRunDetails(ctx, client, u, status)
}

// gatherRunVars merges spec.extraVars with everything spec.varsFrom
// resolves, and returns the varsFrom names for status.
func gatherRunVars(ctx context.Context, client *dynamic.DynamicClient, run *AnsibleRun) (map[string]string, []string, error) {
	merged := map[string]string{}
	for k, v := range run.Spec.ExtraVars {
		merged[k] = v
	}
	if len(run.Spec.VarsFrom) == 0 {
		return merged, nil, nil
	}
	resolved, names, err := resolveVarsFrom(ctx, client, varsFromRESTMapper, run.Namespace, run.Spec.VarsFrom, run.Spec.ExtraVars)
	if err != nil {
		return nil, nil, err
	}
	for k, v := range resolved {
		merged[k] = v
	}
	return merged, names, nil
}

// runTarget is one resolved inventory target, from spec.hosts or from
// the VM spec.vmRef names.
type runTarget struct {
	Name      string
	Address   string
	Variables map[string]string
}

// runTargets resolves what this run points at. An empty result means the
// run is unscoped: nothing is written to the inventory and no limit is
// sent, so the template's own inventory and scope apply. That is the
// right shape for a playbook that runs on localhost and talks to an
// external API.
func runTargets(ctx context.Context, client *dynamic.DynamicClient, run *AnsibleRun) ([]runTarget, error) {
	spec := run.Spec

	if len(spec.Hosts) > 0 {
		seen := map[string]bool{}
		targets := make([]runTarget, 0, len(spec.Hosts))
		for i, h := range spec.Hosts {
			if h.Name == "" {
				return nil, terminalf("spec.hosts[%d].name is required", i)
			}
			if seen[h.Name] {
				return nil, terminalf("spec.hosts names host %q more than once", h.Name)
			}
			seen[h.Name] = true
			// No hostNamePrefix here, deliberately. These are literal
			// names, usually of hosts that already exist in the inventory;
			// prefixing one would match nothing and create a duplicate.
			targets = append(targets, runTarget{Name: h.Name, Address: h.Address, Variables: h.Variables})
		}
		return targets, nil
	}

	if spec.VMRef == nil {
		return nil, nil
	}
	if spec.VMRef.Name == "" {
		return nil, terminalf("spec.vmRef.name is required")
	}

	vm, err := client.Resource(vmGVR).Namespace(run.Namespace).Get(ctx, spec.VMRef.Name, metav1.GetOptions{})
	if err != nil {
		// Deliberately not terminal: the VM may still be being created.
		return nil, fmt.Errorf("reading VirtualMachine %q: %w", spec.VMRef.Name, err)
	}
	ip, ready := vmReady(vm)
	if !ready {
		return nil, fmt.Errorf("VirtualMachine %q is not powered on with a reported IP yet", spec.VMRef.Name)
	}

	name := spec.VMRef.Name
	if spec.HostName != "" {
		name = spec.HostName
	}
	// A derived name does carry the connection's prefix, the same as a
	// binding's, so several supervisors sharing one AWX stay apart.
	conn, err := runConnection(ctx, client, run)
	if err != nil {
		return nil, err
	}
	return []runTarget{{Name: conn.Spec.HostNamePrefix + name, Address: ip, Variables: spec.HostVariables}}, nil
}

// deadlineExceeded reports whether the run has outlived
// spec.activeDeadlineSeconds, measured from creation.
func deadlineExceeded(run *AnsibleRun) bool {
	if run.Spec.ActiveDeadlineSeconds <= 0 {
		return false
	}
	created := run.CreationTimestamp.Time
	if created.IsZero() {
		return false
	}
	return time.Since(created) > time.Duration(run.Spec.ActiveDeadlineSeconds)*time.Second
}

// collectFinishedRun deletes a finished run once its TTL is up. Deleting
// the CR runs the finalizer, which takes the AWX hosts it created with
// it. The TTL is evaluated on the ordinary reconcile path, so its
// granularity is the resync period - close enough for a garbage
// collector, and it keeps the engine free of a requeue-after channel.
func collectFinishedRun(ctx context.Context, client *dynamic.DynamicClient, run *AnsibleRun, status AnsibleRunStatus) error {
	if run.Spec.TTLSecondsAfterFinished == nil {
		return nil
	}
	finished, err := time.Parse(time.RFC3339, status.FinishedAt)
	if err != nil {
		log.Printf("[AnsibleRun/%s/%s] cannot parse status.finishedAt %q, not collecting: %v",
			run.Namespace, run.Name, status.FinishedAt, err)
		return nil
	}
	if time.Since(finished) < time.Duration(*run.Spec.TTLSecondsAfterFinished)*time.Second {
		return nil
	}

	log.Printf("[AnsibleRun/%s/%s] ttlSecondsAfterFinished elapsed, deleting", run.Namespace, run.Name)
	if err := client.Resource(ansRunGVR).Namespace(run.Namespace).Delete(ctx, run.Name, metav1.DeleteOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("deleting finished AnsibleRun: %w", err)
	}
	return nil
}

// failRun ends a run for good: it stamps finishedAt so the TTL can
// collect it, records why in a field the aggregate status updater can
// still read on later passes, and returns nil so the workqueue stops
// retrying something that cannot improve.
func failRun(ctx context.Context, client *dynamic.DynamicClient, u *unstructured.Unstructured, status *AnsibleRunStatus, reason string) error {
	status.FinishedAt = nowRFC3339()
	status.FailureReason = reason
	if status.JobStatus == "" {
		status.JobStatus = "unknown"
	}
	log.Printf("[AnsibleRun/%s/%s] terminal failure: %s", u.GetNamespace(), u.GetName(), reason)
	return writeRunDetails(ctx, client, u, status)
}

// writeRunDetails persists the detail half of status.
func writeRunDetails(ctx context.Context, client *dynamic.DynamicClient, obj *unstructured.Unstructured, status *AnsibleRunStatus) error {
	data, err := structToMap(runStatusDetails(status))
	if err != nil {
		return fmt.Errorf("encoding AnsibleRun status: %w", err)
	}
	return patchStatus(ctx, client, ansRunGVR, obj, data, runDetailsFieldManager)
}

// runStatusDetails is the status fields this controller owns - all of
// them except the generic four the engine writes under its own field
// manager.
func runStatusDetails(status *AnsibleRunStatus) AnsibleRunStatus {
	return AnsibleRunStatus{
		JobID:             status.JobID,
		JobURL:            status.JobURL,
		JobStatus:         status.JobStatus,
		StartedAt:         status.StartedAt,
		FinishedAt:        status.FinishedAt,
		LaunchAttemptedAt: status.LaunchAttemptedAt,
		FailureReason:     status.FailureReason,
		ResolvedVars:      status.ResolvedVars,
		Hosts:             status.Hosts,
	}
}

// cleanupAnsibleRun deletes the AWX hosts this run created before its
// finalizer is released, on the same terms as an AnsibleBinding's
// cleanup: only hosts we created, never adopted ones, retried rather
// than leaked, and abandoned only when there is genuinely no way left to
// reach AWX.
func cleanupAnsibleRun(ctx context.Context, client *dynamic.DynamicClient, obj interface{}) error {
	u, err := toUnstructured(obj)
	if err != nil {
		return nil
	}
	run, err := convertAnsibleRun(u)
	if err != nil || run.Spec == nil || run.Status == nil {
		return nil
	}
	if run.Spec.CleanupPolicy == CleanupPolicyRetain {
		return nil
	}

	var toDelete []RunHostStatus
	for _, h := range run.Status.Hosts {
		if h.AWXHostID != 0 && h.AWXHostCreated {
			toDelete = append(toDelete, h)
		}
	}
	if len(toDelete) == 0 {
		return nil
	}

	abandon := func(reason string, err error) {
		log.Printf("[AnsibleRun/%s/%s] cleanup: %s, abandoning %d AWX host(s): %v",
			run.Namespace, run.Name, reason, len(toDelete), err)
	}

	connObj, err := client.Resource(awxConnGVR).Namespace(run.Namespace).Get(ctx, run.Spec.AWXConnectionRef, metav1.GetOptions{})
	if err != nil {
		if !isPermanent(err) {
			return fmt.Errorf("fetching AWXConnection %q to clean up %d AWX host(s): %w", run.Spec.AWXConnectionRef, len(toDelete), err)
		}
		abandon(fmt.Sprintf("AWXConnection %q is gone", run.Spec.AWXConnectionRef), err)
		return nil
	}
	conn, err := convertAWXConnection(connObj)
	if err != nil || conn.Spec == nil {
		abandon(fmt.Sprintf("AWXConnection %q is malformed", run.Spec.AWXConnectionRef), err)
		return nil
	}
	token, err := getSecretValue(ctx, client, run.Namespace, conn.Spec.SecretRef, "token")
	if err != nil {
		if !isPermanent(err) {
			return fmt.Errorf("reading the AWX token to clean up %d AWX host(s): %w", len(toDelete), err)
		}
		abandon("the AWX token is gone", err)
		return nil
	}
	awxClient, _, err := awxClientFor(ctx, client, conn, token)
	if err != nil {
		return fmt.Errorf("resolving the AWX API base path to clean up %d AWX host(s) "+
			"(set spec.cleanupPolicy: Retain to release this run and leave them in place): %w", len(toDelete), err)
	}

	var firstErr error
	for _, h := range toDelete {
		if err := awxClient.DeleteHost(ctx, int(h.AWXHostID)); err != nil {
			log.Printf("[AnsibleRun/%s/%s] cleanup: failed to delete AWX host %d (%q): %v", run.Namespace, run.Name, h.AWXHostID, h.Name, err)
			if firstErr == nil {
				firstErr = fmt.Errorf("deleting AWX host %d (%q): %w", h.AWXHostID, h.Name, err)
			}
		}
	}
	return firstErr
}

// updateAnsibleRunStatus derives the run's aggregate state from what the
// detail fields say.
//
// The generic updater cannot do this: it only sees whether the reconcile
// returned an error, and an AWX job that ran and failed is not a
// reconcile error - the controller did exactly its job. Reporting Ready
// off the back of that would mark a run healthy while its playbook
// failed.
func updateAnsibleRunStatus(u *unstructured.Unstructured, success bool, reconcileErr error) map[string]interface{} {
	state := func(state, message string, ready bool) map[string]interface{} {
		return map[string]interface{}{
			"state":       state,
			"message":     message,
			"ready":       ready,
			"lastUpdated": metav1.Now(),
		}
	}

	run, err := convertAnsibleRun(u)
	if err != nil {
		return updateGenericStatus(u, success, reconcileErr)
	}
	status := AnsibleRunStatus{}
	if run.Status != nil {
		status = *run.Status
	}

	// A terminal outcome is the last word, whatever this particular pass
	// returned - including on later passes that find nothing left to do.
	if status.FinishedAt != "" {
		if status.JobStatus == "successful" {
			return state("Ready", fmt.Sprintf("AWX job %d completed successfully.", status.JobID), true)
		}
		switch {
		case status.FailureReason != "":
			return state("Failed", status.FailureReason, false)
		case status.JobID != 0:
			return state("Failed", fmt.Sprintf("AWX job %d finished %s.", status.JobID, status.JobStatus), false)
		default:
			return state("Failed", "Run failed.", false)
		}
	}

	// Below here the run has not finished, so it is still being retried and
	// is emphatically not Failed - that state is reserved for a terminal
	// outcome. Reporting Failed for a retryable condition would make the
	// word mean two different things, and the common one is transient: an
	// object that has not appeared yet, AWX briefly unreachable. What
	// bounds this is spec.activeDeadlineSeconds, not the state name.
	if status.JobID != 0 {
		if reconcileErr != nil {
			return state("Running", fmt.Sprintf("AWX job %d is %s; last error: %s",
				status.JobID, status.JobStatus, reconcileErr.Error()), false)
		}
		return state("Running", fmt.Sprintf("AWX job %d is %s.", status.JobID, status.JobStatus), false)
	}
	if reconcileErr != nil {
		return state("Pending", fmt.Sprintf("Not launched yet, retrying: %s", reconcileErr.Error()), false)
	}
	return state("Pending", "Waiting to launch.", false)
}
