package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
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
func applyAnsibleRun(ctx context.Context, client *dynamic.DynamicClient, obj interface{}) (Result, error) {
	cached, err := toUnstructured(obj)
	if err != nil {
		return Result{}, err
	}

	// The informer's copy describes the run; it does not authorize
	// anything. It can be behind by exactly the write that recorded a
	// launch, and a stale "no job yet" is how one AnsibleRun becomes two
	// AWX jobs: the pass reads no job id, launches, and the write that
	// would have stopped it was already there.
	//
	// A binding can absorb that - it reconciles standing state and the
	// next pass corrects it. A launch cannot be un-launched, so every
	// pass over a run works from the record as the API server holds it,
	// at the cost of one GET.
	u, err := readRunLive(ctx, client, cached)
	if err != nil {
		return Result{}, err
	}
	if u == nil {
		// Gone, or replaced under the same name. Either way this pass is
		// about an object that no longer exists; a replacement is a new
		// request with its own UID and arrives with its own event.
		return Result{}, nil
	}

	run, err := convertAnsibleRun(u)
	if err != nil {
		return Result{}, fmt.Errorf("decoding AnsibleRun: %w", err)
	}
	if run.Spec == nil {
		return Result{}, fmt.Errorf("spec is required")
	}
	status := AnsibleRunStatus{}
	if run.Status != nil {
		status = *run.Status
	}

	// Return the status written in this pass; the informer may still be behind.
	finish := func(reconcileErr error) (Result, error) {
		updated := u.DeepCopy()
		data, err := structToMap(&status)
		if err != nil {
			return Result{}, err
		}
		updated.Object["status"] = data
		return Result{Object: updated}, reconcileErr
	}

	// Already finished: nothing left but to collect it when its TTL is up.
	if status.FinishedAt != "" {
		return finish(collectFinishedRun(ctx, client, &run, status))
	}

	// The deadline is checked before anything else so a run wedged on a
	// retryable condition - AWX down, a referenced object that never
	// appears, a job stuck non-terminal - still reaches an end state.
	if deadlineExceeded(&run) {
		return finish(expireRun(ctx, client, u, &run, &status))
	}

	// Everything below can fail terminally from several layers down, so
	// the classification is handled once, here: a terminal error stamps
	// finishedAt and the reason, and is not retried.
	rErr := reconcileRun(ctx, client, u, &run, &status)
	if isTerminalError(rErr) {
		return finish(failRun(ctx, client, u, &status, rErr.Error()))
	}
	return finish(rErr)
}

// readRunLive re-reads a run from the API server, and reports nil when
// the object this pass was queued for is no longer there to act on.
//
// The UID check is what separates "the run I was handed" from "whatever
// now holds that name". Deleting an AnsibleRun and creating another with
// the same name is the documented way to request a second execution, and
// the job id, host ids and terminal outcome recorded by the first belong
// to neither the second nor to any decision made about it.
func readRunLive(ctx context.Context, client *dynamic.DynamicClient, cached *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	live, err := client.Resource(ansRunGVR).Namespace(cached.GetNamespace()).Get(ctx, cached.GetName(), metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("re-reading AnsibleRun %q: %w", cached.GetName(), err)
	}
	if cached.GetUID() != "" && live.GetUID() != cached.GetUID() {
		log.Printf("[AnsibleRun/%s/%s] the object queued for this pass has been replaced by another of the same name, skipping",
			cached.GetNamespace(), cached.GetName())
		return nil, nil
	}
	return live, nil
}

// runAWXContext is everything one pass needs to talk to AWX for a run:
// the connection as it reads now, a client for it, and the fingerprint
// of the instance that client points at.
type runAWXContext struct {
	conn       AWXConnection
	client     *AWXClient
	endpoint   string
	isWorkflow bool
}

// reconcileRun polls an in-flight job, recovers a launch whose answer
// was lost, or does everything up to and including the single launch. It
// writes the detail half of status as it goes, so work already done -
// inventory hosts upserted, a job launched - survives a later failure.
func reconcileRun(ctx context.Context, client *dynamic.DynamicClient, u *unstructured.Unstructured, run *AnsibleRun, status *AnsibleRunStatus) error {
	awx, err := awxContextForRun(ctx, client, run)
	if err != nil {
		return err
	}

	// Every id in status was issued by one AWX instance, and means
	// something else entirely on another. A binding recovers from this
	// by forgetting the ids and rediscovering the host, because it is
	// standing state with more runs ahead of it. A run has exactly one
	// job, already launched somewhere this connection no longer names,
	// and an immutable spec that cannot be pointed back - so there is
	// nothing left for it to do but say so.
	//
	// A run launched before this field existed has none, and is left to
	// the behaviour it launched under rather than fingerprinted here
	// against whatever the connection says now - which, if it had been
	// repointed, is the wrong instance to pin it to.
	if status.AWXEndpoint != "" && status.AWXEndpoint != awx.endpoint {
		what := "the inventory host(s) this run made cannot be identified there"
		if status.JobID != 0 {
			what = fmt.Sprintf("job %d cannot be tracked there, and neither can the inventory host(s) this run made", status.JobID)
		}
		return terminalf("AWXConnection %q now points at a different AWX instance than the one this run went to, so %s. "+
			"Check the original instance, and create a new AnsibleRun to run against the new one",
			run.Spec.AWXConnectionRef, what)
	}

	if status.JobID != 0 {
		return pollRun(ctx, client, u, run, status, awx.client)
	}
	if status.LaunchAttemptedAt != "" {
		// A run with an unresolved attempt never reaches launchRun
		// again: it is either adopted onto the job it started, still
		// waiting to find out, or ended.
		return recoverLostLaunch(ctx, client, u, run, status, awx)
	}
	return launchRun(ctx, client, u, run, status, awx)
}

// awxContextForRun resolves the connection and a client for it, reusing
// the process-wide caches the binding path uses rather than reading the
// Secret and re-probing the API base path on every pass.
//
// A missing or malformed AWXConnection is terminal - it is a spec error,
// and the spec cannot be edited to fix it.
func awxContextForRun(ctx context.Context, client *dynamic.DynamicClient, run *AnsibleRun) (runAWXContext, error) {
	conn, err := runConnection(ctx, client, run)
	if err != nil {
		return runAWXContext{}, err
	}
	awxClient, basePath, err := awxClientForConnection(ctx, client, conn)
	if err != nil {
		if isPermanent(err) {
			return runAWXContext{}, terminalf("preparing a client for AWXConnection %q: %v", run.Spec.AWXConnectionRef, err)
		}
		return runAWXContext{}, fmt.Errorf("preparing a client for AWXConnection %q: %w", run.Spec.AWXConnectionRef, err)
	}
	return runAWXContext{
		conn:       conn,
		client:     awxClient,
		endpoint:   awxEndpointFingerprint(conn.Spec.URL, basePath),
		isWorkflow: run.Spec.Template.Type == TemplateTypeWorkflow,
	}, nil
}

func runConnection(ctx context.Context, client *dynamic.DynamicClient, run *AnsibleRun) (AWXConnection, error) {
	if run.Spec.AWXConnectionRef == "" {
		return AWXConnection{}, terminalf("spec.awxConnectionRef is required")
	}
	connObj, err := getAWXConnection(ctx, client, run.Namespace, run.Spec.AWXConnectionRef)
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

// launchRecoveryGrace is how long AWX is given to show a job it created
// in its own job list before the controller concludes that a launch
// whose answer was lost started nothing at all.
//
// AWX writes the job row before it answers a launch, so this only has to
// cover an answer lost on the way back rather than AWX's own scheduling.
// It is a whole minute anyway: the cost of waiting is one more reconcile
// on a run that has not started, and the cost of being wrong is a
// playbook that runs twice.
const launchRecoveryGrace = time.Minute

// recoverLostLaunch decides what to do about a launch that was recorded
// as attempted and never recorded as having produced a job: the process
// died between the POST and the write, or AWX answered into a connection
// that had already gone.
//
// The job may well be running. Relaunching regardless is how one run
// decommissions a machine, or opens a ticket, twice - so instead this
// does what the Job controller does with pods it may have lost: it goes
// and looks. AWX takes no idempotency key on a launch, so the job cannot
// be tagged going out and has to be recognised coming back, by the
// template that made it, when it was made, and the limit it ran with.
//
// What it will not do is launch again. Recognising a job proves one
// exists; failing to recognise one proves nothing - the job list is a
// single page, history can be trimmed, jobs can be hidden by
// permissions, and the run's own POST may have been answered by an AWX
// whose clock disagrees. Every one of those reads as "no job" and every
// one of them can be wrong. A run that cannot account for its launch
// ends as a failure a human can act on, not as permission to try again.
func recoverLostLaunch(ctx context.Context, client *dynamic.DynamicClient, u *unstructured.Unstructured, run *AnsibleRun, status *AnsibleRunStatus, awx runAWXContext) error {
	attempted, err := time.Parse(time.RFC3339, status.LaunchAttemptedAt)
	if err != nil {
		return terminalf("status.launchAttemptedAt %q cannot be read, so it is not possible to tell whether "+
			"this run's launch reached AWX. Check the template's recent jobs there, and create a new AnsibleRun if it did not run",
			status.LaunchAttemptedAt)
	}

	matched, err := matchLostLaunchJobs(ctx, awx, run, status, attempted)
	if err != nil {
		if isPermanent(err) {
			return terminalf("a launch was sent at %s and its result was never recorded, and the job it may have "+
				"started cannot be looked for (%v). Check template %q's recent jobs in AWX, and create a new "+
				"AnsibleRun if it did not run", status.LaunchAttemptedAt, err, run.Spec.Template.Name)
		}
		return err
	}

	switch len(matched) {
	case 1:
		job := matched[0]
		log.Printf("[AnsibleRun/%s/%s] adopting AWX job %d, which the launch sent at %s started but never recorded",
			run.Namespace, run.Name, job.ID, status.LaunchAttemptedAt)
		status.JobID = int64(job.ID)
		status.JobURL = awx.client.JobURL(job.ID, awx.isWorkflow)
		status.JobStatus = job.Status
		status.StartedAt = job.Created.UTC().Format(time.RFC3339)
		return writeRunDetails(ctx, client, u, status)
	case 0:
		if time.Since(attempted) < launchRecoveryGrace {
			// Not yet a conclusion. AWX writes the job row before it
			// answers, but it may still be committing one it has already
			// accepted, and adopting it is the good outcome here.
			return fmt.Errorf("a launch was sent at %s and its result was never recorded; waiting to see "+
				"whether it started a job in AWX", status.LaunchAttemptedAt)
		}
		return terminalf("a launch was sent at %s and its result was never recorded, and no job matching it has "+
			"appeared in template %q since. AWX may have run it anyway - a trimmed job list, a restricted view or a "+
			"clock that disagrees all look like this - so this run will not send it again. Check the template's "+
			"recent jobs in AWX, and create a new AnsibleRun if it did not run",
			status.LaunchAttemptedAt, run.Spec.Template.Name)
	default:
		ids := make([]string, 0, len(matched))
		for _, job := range matched {
			ids = append(ids, fmt.Sprint(job.ID))
		}
		return terminalf("a launch was sent at %s and its result was never recorded, and template %q has more "+
			"than one job (%s) that could be it, so this run will not guess which. Check them in AWX, and create a "+
			"new AnsibleRun if none of them did what this one asked for",
			status.LaunchAttemptedAt, run.Spec.Template.Name, strings.Join(ids, ", "))
	}
}

// matchLostLaunchJobs returns the jobs in a template's recent history
// that could be the one an unrecorded launch started.
//
// AWX takes no idempotency key on a launch, so a job cannot be tagged
// going out and has to be recognised coming back: by the template that
// made it, the limit it ran with, and its creation time. Only jobs AWX
// created at or after the attempt was recorded qualify - an earlier one
// cannot be this run's, since the attempt marker is written before the
// POST, and adopting it would report another run's outcome here and
// cancel that job when this run is deleted.
//
// The window was once widened by five minutes so that clocks disagreeing
// by seconds could not cause a relaunch. Nothing relaunches now, so that
// tolerance only bought the chance to adopt the wrong job. Clock skew
// costs an unresolved run that says so, which is the direction to be
// wrong in.
func matchLostLaunchJobs(ctx context.Context, awx runAWXContext, run *AnsibleRun, status *AnsibleRunStatus, attempted time.Time) ([]AWXJob, error) {
	// Cached deliberately, unlike the launch path's own lookup: nothing
	// is being launched here, so the ask_*_on_launch flags this carries
	// are not what makes it safe - only the template id is used.
	tmpl, err := resolveTemplateCached(ctx, awx.client, connectionKey(awx.conn), run.Spec.Template)
	if err != nil {
		return nil, err
	}
	jobs, err := awx.client.RecentTemplateJobs(ctx, tmpl.ID, awx.isWorkflow)
	if err != nil {
		return nil, fmt.Errorf("looking for the job a lost launch may have started: %w", err)
	}

	limit := runLimit(status.Hosts)
	var matched []AWXJob
	for _, job := range jobs {
		if job.Limit == limit && !job.Created.Before(attempted) {
			matched = append(matched, job)
		}
	}
	return matched, nil
}

// runLimit is the --limit a run sends: the names of every inventory host
// it targets, or nothing at all for an unscoped run, which accepts the
// template's own scope.
func runLimit(hosts []RunHostStatus) string {
	names := make([]string, 0, len(hosts))
	for _, h := range hosts {
		names = append(names, h.Name)
	}
	return strings.Join(names, ",")
}

// expireRun ends a run that outlived spec.activeDeadlineSeconds, and
// stops the AWX job it launched on the way out.
//
// A Kubernetes Job terminates its pods when its deadline expires, and
// for the same reason: a deadline that only relabelled the record while
// the playbook kept changing machines would be reporting an end state
// for work that is still going on.
//
// The cancel is best effort, and deliberately so. The deadline exists to
// reach an end state even when AWX is exactly what cannot be reached, so
// a cancel that fails is written into the failure reason - where a human
// will read it - rather than allowed to hold the run open.
func expireRun(ctx context.Context, client *dynamic.DynamicClient, u *unstructured.Unstructured, run *AnsibleRun, status *AnsibleRunStatus) error {
	reason := fmt.Sprintf("Run exceeded spec.activeDeadlineSeconds (%ds) before finishing.", run.Spec.ActiveDeadlineSeconds)
	if note := cancelRunJob(ctx, client, run, status); note != "" {
		reason += " " + note
	}
	return failRun(ctx, client, u, status, reason)
}

// cancelRunJob stops this run's AWX job if it has one that is still
// going, and reports in one sentence what happened to it - which is the
// only place that survives for a human to read.
func cancelRunJob(ctx context.Context, client *dynamic.DynamicClient, run *AnsibleRun, status *AnsibleRunStatus) string {
	if status.JobID == 0 {
		if status.LaunchAttemptedAt != "" {
			return fmt.Sprintf("A launch was sent at %s and its result was never recorded, so a job may be running "+
				"in AWX that could not be canceled from here - check the template's recent jobs there.", status.LaunchAttemptedAt)
		}
		return ""
	}
	if isTerminalAWXStatus(status.JobStatus) {
		return ""
	}
	awx, err := awxContextForRun(ctx, client, run)
	if err != nil {
		return fmt.Sprintf("AWX job %d could not be canceled (%v), so it may still be running.", status.JobID, err)
	}
	if status.AWXEndpoint != "" && status.AWXEndpoint != awx.endpoint {
		return fmt.Sprintf("AWX job %d was not canceled: AWXConnection %q now points at a different AWX instance, "+
			"and canceling job %d there would stop an unrelated job.", status.JobID, run.Spec.AWXConnectionRef, status.JobID)
	}
	if err := awx.client.CancelJob(ctx, int(status.JobID), awx.isWorkflow); err != nil {
		log.Printf("[AnsibleRun/%s/%s] could not cancel AWX job %d: %v", run.Namespace, run.Name, status.JobID, err)
		return fmt.Sprintf("AWX job %d could not be canceled (%v), so it may still be running.", status.JobID, err)
	}
	log.Printf("[AnsibleRun/%s/%s] asked AWX to cancel job %d", run.Namespace, run.Name, status.JobID)
	// Requested, not finished: AWX answers a cancel with 202 Accepted and
	// the playbook stops a moment later. Writing "canceled" into
	// jobStatus here would claim, in AWX's own vocabulary, that it had
	// already stopped - and would then tell this run's own finalization
	// there was nothing left to wait for.
	status.CancelRequestedAt = nowRFC3339()
	return fmt.Sprintf("AWX job %d was canceled.", status.JobID)
}

// launchRun resolves the template, gathers variables, reconciles
// inventory hosts, then fires exactly once.
func launchRun(ctx context.Context, client *dynamic.DynamicClient, u *unstructured.Unstructured, run *AnsibleRun, status *AnsibleRunStatus, awx runAWXContext) error {
	spec := run.Spec
	awxClient := awx.client

	if len(spec.Hosts) > 0 && spec.VMRef != nil {
		return terminalf("spec.hosts and spec.vmRef are mutually exclusive: a run points at explicit hosts or at one VM, not both")
	}
	if spec.Template.Type != TemplateTypeJob && spec.Template.Type != TemplateTypeWorkflow {
		return terminalf("spec.template.type must be %q or %q, got %q", TemplateTypeJob, TemplateTypeWorkflow, spec.Template.Type)
	}

	// Recorded before anything is created in AWX, so that a pass which
	// dies part-way through still leaves behind which instance the ids
	// it did write belong to.
	status.AWXEndpoint = awx.endpoint

	// Always from AWX, never from the cache: ask_limit_on_launch is what
	// stops this run reaching a whole inventory, and it can be switched
	// off in the AWX UI between one pass and the next.
	tmpl, err := resolveTemplateForLaunch(ctx, awxClient, connectionKey(awx.conn), spec.Template)
	if err != nil {
		// A named template that is not there, or is ambiguous, is a spec
		// error: the spec cannot be edited to fix it, so failing is more
		// useful than retrying forever. AWX being unreachable or in the
		// middle of a 503 is not that, and must not end the run.
		if isPermanent(err) {
			return terminalf("%v", err)
		}
		return err
	}

	extraVars, resolvedNames, err := gatherRunVars(ctx, client, run)
	if err != nil {
		return err
	}
	status.ResolvedVars = resolvedNames

	targets, err := runTargets(ctx, client, run, awx.conn)
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
		hostID, owned, hErr := resolveRunHost(ctx, awxClient, *tmpl.Inventory, t, runHostOwnerMarker(run.Namespace, run.Name))
		if hErr != nil {
			if wErr := writeRunDetails(ctx, client, u, status); wErr != nil {
				log.Printf("[AnsibleRun/%s/%s] could not record partial host state: %v", run.Namespace, run.Name, wErr)
			}
			return fmt.Errorf("upserting AWX host %q: %w", t.Name, hErr)
		}
		status.Hosts = recordRunHost(status.Hosts, RunHostStatus{
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
	if err := claimLaunch(ctx, client, u, status); err != nil {
		return fmt.Errorf("recording the launch attempt: %w", err)
	}

	limit := strings.Join(limits, ",")
	var jobID int
	var lErr error
	if awx.isWorkflow {
		jobID, lErr = awxClient.LaunchWorkflowJobTemplate(ctx, tmpl.ID, limit, extraVars)
	} else {
		jobID, lErr = awxClient.LaunchJobTemplate(ctx, tmpl.ID, limit, extraVars)
	}

	if jobID != 0 {
		// Record the job even alongside an error (AWX ignored fields):
		// it is real, it is running, and it must stay traceable.
		status.JobID = int64(jobID)
		status.JobURL = awxClient.JobURL(jobID, awx.isWorkflow)
		status.JobStatus = "pending"
		status.StartedAt = nowRFC3339()
	}
	if lErr != nil {
		if jobID == 0 {
			// Whether the attempt marker comes off decides whether this
			// run may launch again, so it comes off only when AWX itself
			// said it refused the request. A 5xx, a timeout, a dropped
			// connection: those are answers about the conversation, not
			// about the job, and AWX may have created one and lost the
			// reply. Leaving the marker set hands that case to
			// recoverLostLaunch, which goes and looks rather than
			// guessing either way.
			if launchRefused(lErr) {
				status.LaunchAttemptedAt = ""
			}
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

// launchRefused reports whether AWX answered a launch by turning it
// down, which is the only case in which nothing can have started.
//
// A 4xx is AWX rejecting the request before it makes anything: a bad
// token, a template that has gone, a body it will not take. The two
// exceptions are the 4xx codes that describe the transport rather than
// the request - a timed-out request and a throttled one - either of
// which can still have been acted on.
func launchRefused(err error) bool {
	code, answered := awxStatusCode(err)
	if !answered {
		return false
	}
	if code == http.StatusRequestTimeout || code == http.StatusTooManyRequests {
		return false
	}
	return code >= 400 && code < 500
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
	Name string
	// Address is what ansible_host gets on a host this run creates. For
	// a vmRef that is the VM's reported address, which is a fact about
	// the target rather than something the run asked to write anywhere.
	Address string
	// RequestedAddress is an address the spec supplied by hand, which is
	// a request to write ansible_host onto the inventory host.
	RequestedAddress string
	// Overrides are host variables the spec asked to write.
	Overrides map[string]string
}

// requestsHostWrites reports whether this target asks for anything to be
// written onto an inventory entry, which is only possible on a host this
// run owns. A VM's reported address is not one of these: it is used when
// the host has to be created and ignored when it already exists.
func (t runTarget) requestsHostWrites() bool {
	return t.RequestedAddress != "" || len(t.Overrides) > 0
}

// runTargets resolves what this run points at. An empty result means the
// run is unscoped: nothing is written to the inventory and no limit is
// sent, so the template's own inventory and scope apply. That is the
// right shape for a playbook that runs on localhost and talks to an
// external API.
func runTargets(ctx context.Context, client *dynamic.DynamicClient, run *AnsibleRun, conn AWXConnection) ([]runTarget, error) {
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
			targets = append(targets, runTarget{
				Name: h.Name, Address: h.Address, RequestedAddress: h.Address, Overrides: h.Variables,
			})
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
	return []runTarget{{Name: conn.Spec.HostNamePrefix + name, Address: ip, Overrides: spec.HostVariables}}, nil
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
	// Bound to the object this decision was made about. A run collected
	// on its TTL while a replacement of the same name has already been
	// created would otherwise delete the new one - which, being a new
	// request, may not have run yet.
	opts := metav1.DeleteOptions{}
	if uid := run.UID; uid != "" {
		opts.Preconditions = &metav1.Preconditions{UID: &uid}
	}
	if err := client.Resource(ansRunGVR).Namespace(run.Namespace).Delete(ctx, run.Name, opts); err != nil {
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

// engineStatusFields are the four aggregate fields the engine writes
// under its own field manager. A server-side apply keeps the two
// managers apart on its own; claimLaunch replaces the status object
// whole, so it carries these across by hand.
var engineStatusFields = []string{"state", "message", "ready", "lastUpdated"}

// claimLaunch records the launch attempt conditionally: the write
// carries the resourceVersion this pass read the run at, so anything
// that landed in between is a conflict rather than something to
// overwrite.
//
// This is the one status write that authorizes an external side effect,
// and the only one that must not be forced. Every other write reports
// what already happened, and losing a race there costs a stale message;
// losing it here costs a second playbook run. A conflict is returned as
// an ordinary error: the next pass re-reads and re-decides rather than
// replaying a decision made against a record that has since moved.
func claimLaunch(ctx context.Context, client *dynamic.DynamicClient, obj *unstructured.Unstructured, status *AnsibleRunStatus) error {
	data, err := structToMap(runStatusDetails(status))
	if err != nil {
		return fmt.Errorf("encoding AnsibleRun status: %w", err)
	}
	if existing, found, _ := unstructured.NestedMap(obj.Object, "status"); found {
		for _, field := range engineStatusFields {
			if v, ok := existing[field]; ok {
				data[field] = v
			}
		}
	}

	claimed := obj.DeepCopy()
	claimed.Object["status"] = data
	updated, err := client.Resource(ansRunGVR).Namespace(claimed.GetNamespace()).UpdateStatus(
		ctx, claimed, metav1.UpdateOptions{FieldManager: runDetailsFieldManager})
	if err != nil {
		return err
	}
	// Later writes in this pass - the job id above all - go through
	// server-side apply against the object as it now stands.
	obj.SetResourceVersion(updated.GetResourceVersion())
	return nil
}

// recordRunHost files one host against the identity that locates it,
// rather than appending.
//
// Appending looked harmless because the loop that fills it runs once,
// but it runs once per pass: a run whose second host fails to upsert
// records the first, retries the whole loop, and records it again. The
// cleanup then deletes the same host twice, and - worse - runLimit,
// which reconstructs what was launched from these entries, produces
// "db-1,db-1" and no longer matches the job AWX actually ran.
func recordRunHost(hosts []RunHostStatus, h RunHostStatus) []RunHostStatus {
	for i, existing := range hosts {
		if existing.Name != h.Name || existing.AWXInventoryID != h.AWXInventoryID {
			continue
		}
		// Ownership is recorded the pass the host is created and cannot
		// be re-derived afterwards: a retry finds the host already there
		// and would report it as adopted, which is the one difference
		// that decides whether cleanup may delete it.
		h.AWXHostCreated = h.AWXHostCreated || existing.AWXHostCreated
		hosts[i] = h
		return hosts
	}
	return append(hosts, h)
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
		CancelRequestedAt: status.CancelRequestedAt,
		AWXEndpoint:       status.AWXEndpoint,
		FailureReason:     status.FailureReason,
		ResolvedVars:      status.ResolvedVars,
		Hosts:             status.Hosts,
	}
}

// cleanupAnsibleRun stops the AWX job this run started and deletes the
// AWX hosts it created, before its finalizer is released.
//
// Deleting a Kubernetes Job stops the pods it made, and a run that left
// its playbook executing would be doing the opposite of what deleting it
// asked for - the CR would be gone while the work it started kept
// changing machines, with nothing left pointing at the job.
//
// The hosts go on the same terms as an AnsibleBinding's cleanup: only
// hosts we created, never adopted ones, retried rather than leaked, and
// abandoned only when there is genuinely no way left to reach AWX.
func cleanupAnsibleRun(ctx context.Context, client *dynamic.DynamicClient, obj interface{}) (CleanupResult, error) {
	done := CleanupResult{Done: true}

	cached, err := toUnstructured(obj)
	if err != nil {
		return done, nil
	}

	// Live, and before anything is decided. The informer's copy can be
	// behind by exactly the write that recorded the job id or the last
	// host - the two things this function exists to act on, and neither
	// rediscoverable once the object is gone. Concluding from a cached
	// copy with no status that there is nothing to clean up is how a run
	// releases its finalizer with a playbook still running.
	live, err := readRunLive(ctx, client, cached)
	if err != nil {
		return CleanupResult{}, err
	}
	if live == nil {
		// Already gone, or replaced under the same name. Nothing
		// recorded here belongs to whatever holds that name now.
		return done, nil
	}
	run, err := convertAnsibleRun(live)
	if err != nil || run.Spec == nil {
		return done, nil
	}
	status := AnsibleRunStatus{}
	if run.Status != nil {
		status = *run.Status
	}

	var toDelete []RunHostStatus
	if run.Spec.CleanupPolicy != CleanupPolicyRetain {
		for _, h := range status.Hosts {
			if h.AWXHostID != 0 && h.AWXHostCreated {
				toDelete = append(toDelete, h)
			}
		}
	}
	// A job still going is the other reason to reach AWX. cleanupPolicy
	// governs inventory hosts, not whether work this run started is left
	// running after it is gone, so Retain does not exempt it.
	cancelling := status.JobID != 0 && !isTerminalAWXStatus(status.JobStatus)
	// And a launch whose answer was never recorded may have started a job
	// nothing here has an id for. Releasing on the strength of not having
	// one is the same mistake as launching again on it.
	unresolved := status.JobID == 0 && status.LaunchAttemptedAt != ""
	if len(toDelete) == 0 && !cancelling && !unresolved {
		return done, nil
	}

	abandon := func(reason string, err error) {
		log.Printf("[AnsibleRun/%s/%s] cleanup: %s, abandoning %d AWX host(s) and any running job: %v",
			run.Namespace, run.Name, reason, len(toDelete), err)
	}

	conn, err := runConnection(ctx, client, &run)
	if err != nil {
		if !isTerminalError(err) {
			return CleanupResult{}, fmt.Errorf("fetching AWXConnection %q to clean up after this run: %w", run.Spec.AWXConnectionRef, err)
		}
		abandon(fmt.Sprintf("AWXConnection %q is gone or malformed", run.Spec.AWXConnectionRef), err)
		return done, nil
	}
	awxClient, basePath, err := awxClientForConnection(ctx, client, conn)
	if err != nil {
		if isPermanent(err) {
			abandon("the AWX token is gone or the connection is malformed", err)
			return done, nil
		}
		return CleanupResult{}, fmt.Errorf("preparing a client to clean up after this run "+
			"(set spec.cleanupPolicy: Retain to release it and leave its AWX hosts in place): %w", err)
	}
	awx := runAWXContext{
		conn:       conn,
		client:     awxClient,
		endpoint:   awxEndpointFingerprint(conn.Spec.URL, basePath),
		isWorkflow: run.Spec.Template.Type == TemplateTypeWorkflow,
	}

	// Canceling a job id, or deleting a host id, on an instance that did
	// not issue that id would act on whatever unrelated object holds it
	// there. What this run made is on the instance the connection no
	// longer names, and there is no reaching it from here.
	if status.AWXEndpoint != "" && status.AWXEndpoint != awx.endpoint {
		abandon(fmt.Sprintf("AWXConnection %q now points at a different AWX instance", run.Spec.AWXConnectionRef), nil)
		return done, nil
	}

	// One more look for a job the unrecorded launch may have started,
	// under the same rules the reconcile path uses. This is the last
	// chance to find it: after the finalizer comes off there is nothing
	// left pointing at it.
	if unresolved {
		if found, fErr := resolveLostJobForCleanup(ctx, client, live, &run, &status, awx); fErr != nil {
			return CleanupResult{}, fErr
		} else if found {
			cancelling = !isTerminalAWXStatus(status.JobStatus)
		}
	}

	if cancelling {
		result, cErr := cancelAndConfirm(ctx, client, live, &run, &status, awx)
		if cErr != nil {
			// Both the finalizer and the inventory stay put. A playbook
			// that may still be running is the one case where deleting
			// the host it runs against makes things worse.
			return CleanupResult{}, cErr
		}
		if !result.Done {
			return result, nil
		}
	}

	var firstErr error
	record := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}

	marker := runHostOwnerMarker(run.Namespace, run.Name)
	for _, h := range toDelete {
		host, hErr := awxClient.GetHostByID(ctx, int(h.AWXHostID))
		if hErr != nil {
			log.Printf("[AnsibleRun/%s/%s] cleanup: could not read AWX host %d (%q) to check it is still ours: %v",
				run.Namespace, run.Name, h.AWXHostID, h.Name, hErr)
			record(fmt.Errorf("reading AWX host %d (%q) before deleting it: %w", h.AWXHostID, h.Name, hErr))
			continue
		}
		if host == nil {
			// Already gone, which is the end state this wanted.
			continue
		}
		// The recorded id says this run created the host. AWX says what
		// it is now. A host rebuilt by hand under the same id, or one
		// another owner has since claimed, is not this run's to delete.
		if strings.TrimSpace(host.Description) != marker {
			log.Printf("[AnsibleRun/%s/%s] cleanup: AWX host %d is now %q (%q), not this run's, so it was left alone",
				run.Namespace, run.Name, h.AWXHostID, host.Name, strings.TrimSpace(host.Description))
			continue
		}
		if err := awxClient.DeleteHost(ctx, int(h.AWXHostID)); err != nil {
			log.Printf("[AnsibleRun/%s/%s] cleanup: failed to delete AWX host %d (%q): %v", run.Namespace, run.Name, h.AWXHostID, h.Name, err)
			record(fmt.Errorf("deleting AWX host %d (%q): %w", h.AWXHostID, h.Name, err))
		}
	}
	return CleanupResult{Done: firstErr == nil}, firstErr
}

// resolveLostJobForCleanup looks one last time for the job an unrecorded
// launch may have started, and records it if it can be identified.
//
// Not finding it is not an error. The reconcile path has already looked
// and ended the run saying so, and there is no later state in which this
// resolves itself - the job list only ages further away. Holding the
// finalizer for something that can never be answered would wedge the
// object with no way out but stripping the finalizer by hand, which
// leaves the same job running and the record gone as well.
func resolveLostJobForCleanup(ctx context.Context, client *dynamic.DynamicClient, obj *unstructured.Unstructured,
	run *AnsibleRun, status *AnsibleRunStatus, awx runAWXContext) (bool, error) {

	attempted, err := time.Parse(time.RFC3339, status.LaunchAttemptedAt)
	if err != nil {
		log.Printf("[AnsibleRun/%s/%s] cleanup: status.launchAttemptedAt %q cannot be read, so a job this run may "+
			"have started cannot be looked for - check template %q in AWX",
			run.Namespace, run.Name, status.LaunchAttemptedAt, run.Spec.Template.Name)
		return false, nil
	}

	matched, err := matchLostLaunchJobs(ctx, awx, run, status, attempted)
	if err != nil {
		if isPermanent(err) {
			log.Printf("[AnsibleRun/%s/%s] cleanup: cannot look for the job the launch sent at %s may have started: %v",
				run.Namespace, run.Name, status.LaunchAttemptedAt, err)
			return false, nil
		}
		// AWX being briefly unreachable is worth waiting out: this is the
		// last look there will be.
		return false, fmt.Errorf("looking for the job an unrecorded launch may have started, before releasing this run: %w", err)
	}
	if len(matched) != 1 {
		log.Printf("[AnsibleRun/%s/%s] cleanup: the launch sent at %s cannot be tied to a single job in template %q "+
			"(%d candidates), so anything it started is left running - check that template in AWX",
			run.Namespace, run.Name, status.LaunchAttemptedAt, run.Spec.Template.Name, len(matched))
		return false, nil
	}

	job := matched[0]
	log.Printf("[AnsibleRun/%s/%s] cleanup: the launch sent at %s did start AWX job %d after all",
		run.Namespace, run.Name, status.LaunchAttemptedAt, job.ID)
	status.JobID = int64(job.ID)
	status.JobURL = awx.client.JobURL(job.ID, awx.isWorkflow)
	status.JobStatus = job.Status
	if status.StartedAt == "" {
		status.StartedAt = job.Created.UTC().Format(time.RFC3339)
	}
	if err := writeRunDetails(ctx, client, obj, status); err != nil {
		return false, err
	}
	return true, nil
}

// cancelConfirmPoll is how often finalization looks again at a job it
// has asked AWX to cancel.
const cancelConfirmPoll = 5 * time.Second

// cancelConfirmTimeout bounds that wait. AWX answers a cancel with 202
// Accepted and stops the job within seconds; a job that has not reached
// a terminal status well past that is not going to be waited out, and
// holding the finalizer for it forever would wedge the resource - and,
// through it, whatever is waiting on the namespace to empty.
const cancelConfirmTimeout = 2 * time.Minute

// cancelAndConfirm stops this run's job and waits for AWX to say it has
// actually stopped.
//
// Those are two separate things. AWX answers a cancel with 202 Accepted,
// meaning it has taken the request - the playbook may still be part-way
// through a task. Deleting the inventory hosts that job is running
// against on the strength of the 202 is how a teardown races the work it
// is tearing down.
func cancelAndConfirm(ctx context.Context, client *dynamic.DynamicClient, obj *unstructured.Unstructured,
	run *AnsibleRun, status *AnsibleRunStatus, awx runAWXContext) (CleanupResult, error) {

	if status.CancelRequestedAt == "" {
		if err := awx.client.CancelJob(ctx, int(status.JobID), awx.isWorkflow); err != nil {
			log.Printf("[AnsibleRun/%s/%s] cleanup: failed to cancel AWX job %d: %v", run.Namespace, run.Name, status.JobID, err)
			return CleanupResult{}, fmt.Errorf("canceling AWX job %d (spec.cleanupPolicy: Retain will not skip this; "+
				"cancel it in AWX to release this run): %w", status.JobID, err)
		}
		log.Printf("[AnsibleRun/%s/%s] cleanup: asked AWX to cancel job %d", run.Namespace, run.Name, status.JobID)
		status.CancelRequestedAt = nowRFC3339()
		if err := writeRunDetails(ctx, client, obj, status); err != nil {
			return CleanupResult{}, err
		}
	}

	awxStatus, err := pollJobStatus(ctx, awx.client, run.Spec.Template.Type, status.JobID)
	if err != nil {
		return CleanupResult{}, fmt.Errorf("checking whether AWX job %d has stopped: %w", status.JobID, err)
	}
	if awxStatus != status.JobStatus {
		status.JobStatus = awxStatus
		if wErr := writeRunDetails(ctx, client, obj, status); wErr != nil {
			return CleanupResult{}, wErr
		}
	}
	if isTerminalAWXStatus(awxStatus) {
		log.Printf("[AnsibleRun/%s/%s] cleanup: AWX job %d has stopped (%s)", run.Namespace, run.Name, status.JobID, awxStatus)
		return CleanupResult{Done: true}, nil
	}

	requested, pErr := time.Parse(time.RFC3339, status.CancelRequestedAt)
	if pErr == nil && time.Since(requested) > cancelConfirmTimeout {
		log.Printf("[AnsibleRun/%s/%s] cleanup: AWX job %d is still %s more than %s after being canceled; releasing "+
			"this run anyway - check that job in AWX", run.Namespace, run.Name, status.JobID, awxStatus, cancelConfirmTimeout)
		return CleanupResult{Done: true}, nil
	}
	return CleanupResult{RequeueAfter: cancelConfirmPoll}, nil
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
