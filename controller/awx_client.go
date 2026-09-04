package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TLSOptions is how an AWXConnection wants its TLS handled: verify
// against the system roots (the zero value), verify against the system
// roots plus a supplied PEM bundle, or skip verification entirely.
type TLSOptions struct {
	// InsecureSkipVerify skips certificate verification. Mutually
	// exclusive with CABundlePEM - see awxTLSOptionsFor.
	InsecureSkipVerify bool
	// CABundlePEM is a PEM bundle of CA certificates to trust in
	// addition to the system roots, for an AWX behind a private CA.
	CABundlePEM string
}

// cacheKey identifies the transport these options need. Bundles are
// keyed by digest so two connections sharing a CA share a transport.
func (o TLSOptions) cacheKey() string {
	switch {
	case o.InsecureSkipVerify:
		return "insecure"
	case o.CABundlePEM != "":
		return "ca:" + fmt.Sprintf("%x", sha256.Sum256([]byte(o.CABundlePEM)))
	default:
		return "system"
	}
}

// Transports are shared process-wide rather than built per client: a
// fresh http.Transport per reconcile would leak its own idle connection
// pool (and the goroutines servicing it) on every pass. The cache is
// keyed by TLS configuration, so the number of transports is bounded by
// the number of distinct CA bundles in use, not by reconcile count.
var (
	transportMu    sync.Mutex
	transportCache = map[string]*http.Transport{}
)

func transportFor(opts TLSOptions) (*http.Transport, error) {
	key := opts.cacheKey()

	transportMu.Lock()
	defer transportMu.Unlock()
	if t, ok := transportCache[key]; ok {
		return t, nil
	}
	t, err := newTransport(opts)
	if err != nil {
		return nil, err
	}
	transportCache[key] = t
	return t, nil
}

func newTransport(opts TLSOptions) (*http.Transport, error) {
	t := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        50,
		MaxIdleConnsPerHost: 5,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	switch {
	case opts.InsecureSkipVerify:
		t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // opt-in via AWXConnection.spec.insecureSkipVerify
	case opts.CABundlePEM != "":
		pool, err := certPoolWith(opts.CABundlePEM)
		if err != nil {
			return nil, err
		}
		t.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return t, nil
}

// certPoolWith returns the system roots plus the supplied PEM bundle.
// Appending rather than replacing keeps a publicly-trusted AWX reachable
// through a connection that also names an internal CA (for a proxy, say).
func certPoolWith(pemBundle string) (*x509.CertPool, error) {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		// Distroless/scratch images and non-Linux platforms may carry
		// no system bundle. Trusting only the supplied CA is still
		// better than refusing to connect at all.
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM([]byte(pemBundle)) {
		return nil, fmt.Errorf("the CA bundle contains no PEM certificates: %w", errPermanentConfig)
	}
	return pool, nil
}

// AWXClient is a minimal, hand-rolled REST client for the handful of
// AWX/Tower API endpoints this controller needs: resolving job/workflow
// templates, upserting inventory hosts, launching runs, and polling their
// status. A full SDK isn't worth the dependency for this surface area.
type AWXClient struct {
	baseURL string
	// basePath is the API root: "/api/v2" on AWX, Tower and AAP up to
	// 2.4, but "/api/controller/v2" on AAP 2.5+, where the platform
	// gateway moved the controller endpoints. See DetectAPIBasePath.
	basePath   string
	token      string
	httpClient *http.Client
}

// API base paths this controller knows how to talk to, in probe order.
const (
	APIBasePathGateway = "/api/controller/v2" // AAP 2.5+ behind the platform gateway
	APIBasePathLegacy  = "/api/v2"            // AWX, Ansible Tower, AAP <= 2.4
)

var apiBasePathCandidates = []string{APIBasePathGateway, APIBasePathLegacy}

// maxResponseBytes caps how much of an AWX response is read into memory.
// The endpoints used here return small JSON documents; anything larger is
// a misconfigured URL pointing at something that isn't AWX, and reading
// it unbounded would let a tenant-supplied spec.url balloon the
// controller's memory.
const maxResponseBytes = 1 << 20

// maxErrorBodyChars bounds how much of a response body is quoted into an
// error, and therefore into a CR's status.message.
const maxErrorBodyChars = 512

// readCappedBody reads at most maxResponseBytes of a response body.
func readCappedBody(r io.Reader) []byte {
	b, _ := io.ReadAll(io.LimitReader(r, maxResponseBytes))
	return b
}

// truncate shortens s for embedding in an error message.
func truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "... (truncated)"
}

// normalizeAPIBasePath makes a user-supplied path safe to concatenate.
func normalizeAPIBasePath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return strings.TrimRight(path, "/")
}

func httpClientFor(opts TLSOptions) (*http.Client, error) {
	transport, err := transportFor(opts)
	if err != nil {
		return nil, err
	}
	return &http.Client{Timeout: 30 * time.Second, Transport: transport}, nil
}

func NewAWXClient(baseURL, basePath, token string, opts TLSOptions) (*AWXClient, error) {
	if basePath == "" {
		basePath = APIBasePathLegacy
	}
	httpClient, err := httpClientFor(opts)
	if err != nil {
		return nil, err
	}
	return &AWXClient{
		baseURL:    strings.TrimRight(baseURL, "/"),
		basePath:   normalizeAPIBasePath(basePath),
		token:      token,
		httpClient: httpClient,
	}, nil
}

// DetectAPIBasePath finds which API root an instance serves by probing
// each candidate's unauthenticated ping endpoint. Probing status codes
// rather than parsing /api/ keeps this independent of how any given
// version shapes its discovery document, and because ping needs no
// credentials, "wrong API path" stays clearly distinguishable from "bad
// token" - the confusion behind AAP 2.5's "Failed to validate
// credentials" reports.
func DetectAPIBasePath(ctx context.Context, baseURL string, opts TLSOptions) (string, error) {
	client, err := httpClientFor(opts)
	if err != nil {
		return "", err
	}
	trimmed := strings.TrimRight(baseURL, "/")

	var attempts []string
	for _, candidate := range apiBasePathCandidates {
		pingURL := trimmed + candidate + "/ping/"
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, pingURL, nil)
		if err != nil {
			return "", fmt.Errorf("building request: %w", err)
		}
		req.Header.Set("Accept", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			// A transport-level failure means the host is unreachable,
			// not that this candidate is wrong - report it as-is.
			return "", fmt.Errorf("probing %s: %w", pingURL, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return candidate, nil
		}
		attempts = append(attempts, fmt.Sprintf("%s -> %d", candidate, resp.StatusCode))
	}

	return "", fmt.Errorf("could not determine the AWX/AAP API base path at %s (tried %s); "+
		"set spec.apiBasePath on the AWXConnection if this instance serves the API somewhere else",
		trimmed, strings.Join(attempts, ", "))
}

// awxStatusError is a non-2xx response. Typed so a caller that cares
// about one particular status - a 404 meaning "that host is gone" rather
// than "AWX is broken" - can tell without matching on the message.
type awxStatusError struct {
	method string
	path   string
	status int
	body   string
}

func (e *awxStatusError) Error() string {
	return fmt.Sprintf("awx request %s %s: status %d: %s", e.method, e.path, e.status, e.body)
}

// awxStatusIs reports whether err is an AWX response with this status.
func awxStatusIs(err error, status int) bool {
	var e *awxStatusError
	return errors.As(err, &e) && e.status == status
}

func (c *AWXClient) do(ctx context.Context, method, path string, body, out interface{}) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encoding request body: %w", err)
		}
		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("awx request %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	respBody := readCappedBody(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &awxStatusError{method: method, path: path, status: resp.StatusCode, body: truncate(string(respBody), maxErrorBodyChars)}
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("awx request %s %s: decoding response: %w", method, path, err)
		}
	}
	return nil
}

// Ping validates the connection/credentials without depending on any
// particular object existing yet.
func (c *AWXClient) Ping(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, c.basePath+"/me/", nil, nil)
}

// AWXTemplate is a resolved job or workflow template. The ask*OnLaunch
// flags matter a great deal: if AWX isn't configured to accept a limit
// or extra vars at launch time, it silently drops them from the launch
// request and runs the template against its whole inventory instead of
// the VM we meant to target.
type AWXTemplate struct {
	ID                   int
	Inventory            *int
	AskLimitOnLaunch     bool
	AskVariablesOnLaunch bool
}

type templateResult struct {
	ID                   int    `json:"id"`
	Name                 string `json:"name"`
	Inventory            *int   `json:"inventory"`
	AskLimitOnLaunch     bool   `json:"ask_limit_on_launch"`
	AskVariablesOnLaunch bool   `json:"ask_variables_on_launch"`
}

type listTemplatesResponse struct {
	Count   int              `json:"count"`
	Results []templateResult `json:"results"`
}

func (c *AWXClient) findTemplate(ctx context.Context, listPath, kind, name string) (*AWXTemplate, error) {
	var lr listTemplatesResponse
	if err := c.do(ctx, http.MethodGet, listPath+"?name="+url.QueryEscape(name), nil, &lr); err != nil {
		return nil, err
	}
	// AWX's ?name= filter is a field lookup, not part of the published
	// schema: an instance that ignored it would hand back every template
	// in the deployment, and results[0] would launch an arbitrary one.
	// Names are also only unique per organization, so a genuine exact
	// match can legitimately be ambiguous - refuse rather than guess
	// which one the user meant.
	var exact []templateResult
	for i := range lr.Results {
		if lr.Results[i].Name == name {
			exact = append(exact, lr.Results[i])
		}
	}
	if len(exact) == 0 {
		return nil, fmt.Errorf("%s %q not found", kind, name)
	}
	if len(exact) > 1 {
		ids := make([]string, 0, len(exact))
		for _, e := range exact {
			ids = append(ids, strconv.Itoa(e.ID))
		}
		return nil, fmt.Errorf("%s %q is ambiguous: %d templates share that name (IDs %s), "+
			"most likely in different AWX organizations. Rename one, or point this binding at an "+
			"instance where the name is unique", kind, name, len(exact), strings.Join(ids, ", "))
	}
	r := exact[0]
	return &AWXTemplate{
		ID:                   r.ID,
		Inventory:            r.Inventory,
		AskLimitOnLaunch:     r.AskLimitOnLaunch,
		AskVariablesOnLaunch: r.AskVariablesOnLaunch,
	}, nil
}

// FindJobTemplate resolves a Job Template by name. A nil Inventory means
// the template has no inventory configured (possible with
// prompt-on-launch), in which case there's nowhere to create a host.
func (c *AWXClient) FindJobTemplate(ctx context.Context, name string) (*AWXTemplate, error) {
	return c.findTemplate(ctx, c.basePath+"/job_templates/", "job template", name)
}

// FindWorkflowJobTemplate resolves a Workflow Template by name. Workflow
// templates commonly have no inventory of their own (each node can carry
// its own); callers should treat a nil Inventory as "can't target a
// single host".
func (c *AWXClient) FindWorkflowJobTemplate(ctx context.Context, name string) (*AWXTemplate, error) {
	return c.findTemplate(ctx, c.basePath+"/workflow_job_templates/", "workflow job template", name)
}

type hostResult struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Variables   string `json:"variables"`
	Description string `json:"description"`
}

// findHostByName picks the exact name match out of a host list response.
//
// The lookup asks AWX to filter with ?name=, which its field-lookup
// filtering supports - but that parameter is not part of the published
// API schema, so an instance that ignored it would hand back every host
// in the inventory instead. Trusting results[0] there would mean
// adopting, repointing and eventually deleting an unrelated host, so the
// name is re-checked here rather than assumed.
func findHostByName(results []hostResult, hostname string) *hostResult {
	for i := range results {
		if results[i].Name == hostname {
			return &results[i]
		}
	}
	return nil
}

type listHostsResponse struct {
	Count   int          `json:"count"`
	Next    string       `json:"next"`
	Results []hostResult `json:"results"`
}

// maxHostListPages bounds pagination so a filter an instance ignored, or
// a "next" link that loops, cannot spin forever. At AWX's maximum page
// size this is 20,000 hosts, far past the point where the status object
// holding them would already have hit etcd's size limit.
const maxHostListPages = 100

// ListOwnedHosts returns every host in the inventory whose description is
// exactly ownerMarker, keyed by host name.
//
// One call replaces the per-host lookup UpsertHost would otherwise make
// for each VM, which is what dominated a binding's AWX traffic: the
// steady state for a binding matching N VMs goes from N requests to one.
// Drift detection is unchanged - this still reads AWX itself rather than
// trusting status, so a host deleted or edited in the AWX UI is still
// seen. A host missing from the result is simply not one we own yet, and
// the caller falls back to the per-host path that adopts it.
func (c *AWXClient) ListOwnedHosts(ctx context.Context, inventoryID int, ownerMarker string) (map[string]hostResult, error) {
	owned := map[string]hostResult{}
	// page_size is capped by the instance's own max_page_size (200 by
	// default); asking for more than it allows is clamped, not rejected.
	path := fmt.Sprintf("%s/inventories/%d/hosts/?description=%s&page_size=200",
		c.basePath, inventoryID, url.QueryEscape(ownerMarker))

	for pages := 0; path != "" && pages < maxHostListPages; pages++ {
		var lr listHostsResponse
		if err := c.do(ctx, http.MethodGet, path, nil, &lr); err != nil {
			return nil, fmt.Errorf("listing AWX hosts owned by %q: %w", ownerMarker, err)
		}
		for _, h := range lr.Results {
			// Re-checked for the same reason findHostByName re-checks the
			// name: ?description= is field-lookup filtering, not published
			// API, so an instance that ignored it would hand back every
			// host in the inventory and we would claim all of them.
			if strings.TrimSpace(h.Description) == ownerMarker {
				owned[h.Name] = h
			}
		}
		// AWX returns "next" as a path on this same host. Anything else is
		// not something to follow blindly with our own credentials.
		if !strings.HasPrefix(lr.Next, "/") {
			break
		}
		path = lr.Next
	}
	return owned, nil
}

// mergeHostVariables merges ours into an existing AWX host variables
// document. AWX stores host variables as a YAML/JSON *string*. If an
// existing document isn't empty and isn't a JSON object we can safely
// merge into, this refuses rather than destroying whatever an operator
// put there by hand.
func mergeHostVariables(existing string, ours map[string]string) (string, error) {
	trimmed := strings.TrimSpace(existing)
	if trimmed == "" || trimmed == "---" || trimmed == "{}" {
		b, err := json.Marshal(ours)
		return string(b), err
	}

	var current map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &current); err != nil {
		return "", fmt.Errorf("existing host variables are not a JSON object this controller can merge into; "+
			"remove the conflicting host in AWX or point spec.hostName elsewhere: %w", err)
	}
	for k, v := range ours {
		current[k] = v
	}
	b, err := json.Marshal(current)
	return string(b), err
}

// UpsertHost creates or updates a host named hostname in the given
// inventory, stamping ownerMarker into its description so ownership
// outlives the CR that requested it.
//
// AWX host names are unique per inventory, so one AWX shared by several
// supervisors (or several tenant namespaces) can collide on a name. The
// marker makes that collision explicit rather than letting one owner
// silently repoint another's host at a different machine:
//
//   - marked as ours       -> updated, owned (deletable during cleanup)
//   - marked by another    -> refused, nothing is touched
//   - unmarked (pre-existing, someone made it by hand) -> adopted:
//     variables merged, description left alone, never deleted
func (c *AWXClient) UpsertHost(ctx context.Context, inventoryID int, hostname, ownerMarker string, vars map[string]string) (id int, owned bool, err error) {
	return c.upsertHost(ctx, inventoryID, hostname, ownerMarker, vars, nil)
}

// UpsertKnownHost is UpsertHost for a host already read by
// ListOwnedHosts, skipping the per-host lookup. known must be the host
// AWX currently holds under this name, not what status remembers of it,
// or a hand-edit in the AWX UI would go unrepaired.
func (c *AWXClient) UpsertKnownHost(ctx context.Context, inventoryID int, hostname, ownerMarker string, vars map[string]string, known hostResult) (id int, owned bool, err error) {
	return c.upsertHost(ctx, inventoryID, hostname, ownerMarker, vars, &known)
}

func (c *AWXClient) upsertHost(ctx context.Context, inventoryID int, hostname, ownerMarker string, vars map[string]string, existing *hostResult) (id int, owned bool, err error) {
	if existing == nil {
		var lr listHostsResponse
		listPath := fmt.Sprintf("%s/inventories/%d/hosts/?name=%s", c.basePath, inventoryID, url.QueryEscape(hostname))
		if err := c.do(ctx, http.MethodGet, listPath, nil, &lr); err != nil {
			return 0, false, fmt.Errorf("looking up host %q: %w", hostname, err)
		}
		existing = findHostByName(lr.Results, hostname)
	}

	if existing != nil {
		existingMarker := strings.TrimSpace(existing.Description)

		if existingMarker != ownerMarker && strings.HasPrefix(existingMarker, hostMarkerPrefix) {
			return 0, false, fmt.Errorf("inventory host %q is already owned by another ansible-supervisor binding (%s); "+
				"refusing to take it over - set spec.hostNamePrefix on the AWXConnection (or spec.hostName) so this "+
				"binding uses a distinct inventory host name", hostname, existingMarker)
		}

		merged, mErr := mergeHostVariables(existing.Variables, vars)
		if mErr != nil {
			return 0, false, fmt.Errorf("updating host %q: %w", hostname, mErr)
		}
		if merged != strings.TrimSpace(existing.Variables) {
			body := map[string]interface{}{"variables": merged}
			if err := c.do(ctx, http.MethodPatch, fmt.Sprintf("%s/hosts/%d/", c.basePath, existing.ID), body, nil); err != nil {
				return 0, false, fmt.Errorf("updating host %q: %w", hostname, err)
			}
		}
		// Ours if the marker says so - including a host left behind by an
		// earlier incarnation of this same binding. An unmarked host stays
		// unclaimed: it was made by hand, so it isn't ours to delete.
		return existing.ID, existingMarker == ownerMarker, nil
	}

	// No match, so create it. If the host does exist but the lookup
	// somehow failed to surface it (an unfiltered list longer than one
	// page), AWX rejects this with "already exists" on the unique
	// (name, inventory) constraint - a loud error, which is the right
	// outcome rather than silently operating on the wrong host.
	varsJSON, err := json.Marshal(vars)
	if err != nil {
		return 0, false, fmt.Errorf("encoding host variables: %w", err)
	}
	var createdHost hostResult
	body := map[string]interface{}{
		"name":        hostname,
		"description": ownerMarker,
		"variables":   string(varsJSON),
	}
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("%s/inventories/%d/hosts/", c.basePath, inventoryID), body, &createdHost); err != nil {
		return 0, false, fmt.Errorf("creating host %q: %w", hostname, err)
	}
	return createdHost.ID, true, nil
}

// DeleteHost removes a host by ID. A 404 is treated as success: the host
// is already gone, which is the desired end state.
// GetHost reads one host by id, returning nil if it no longer exists.
//
// Used before deleting a host whose id was recorded by an older version
// of this controller: an id only means anything on the instance that
// issued it, so the ownership marker on the host itself is what says
// whether the thing at that id is really ours.
func (c *AWXClient) GetHost(ctx context.Context, id int) (*hostResult, error) {
	var h hostResult
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s/hosts/%d/", c.basePath, id), nil, &h)
	if err != nil {
		if awxStatusIs(err, http.StatusNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &h, nil
}

func (c *AWXClient) DeleteHost(ctx context.Context, id int) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, fmt.Sprintf("%s%s/hosts/%d/", c.baseURL, c.basePath, id), nil)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("awx request DELETE %s/hosts/%d/: %w", c.basePath, id, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound || (resp.StatusCode >= 200 && resp.StatusCode < 300) {
		return nil
	}
	respBody := readCappedBody(resp.Body)
	return fmt.Errorf("awx request DELETE %s/hosts/%d/: status %d: %s", c.basePath, id, resp.StatusCode, truncate(string(respBody), maxErrorBodyChars))
}

func launchBody(limit string, extraVars map[string]string) (map[string]interface{}, error) {
	body := map[string]interface{}{}
	if limit != "" {
		body["limit"] = limit
	}
	if len(extraVars) > 0 {
		b, err := json.Marshal(extraVars)
		if err != nil {
			return nil, fmt.Errorf("encoding extra vars: %w", err)
		}
		body["extra_vars"] = string(b)
	}
	return body, nil
}

type launchResponse struct {
	Job           int                        `json:"job"`
	WorkflowJob   int                        `json:"workflow_job"`
	IgnoredFields map[string]json.RawMessage `json:"ignored_fields"`
}

// ignoredFieldsError reports fields AWX accepted the launch *without*.
// This is the failure mode that silently widens a run's blast radius: a
// dropped "limit" means the template ran against its entire inventory
// rather than the VM we targeted. Callers pre-flight the template's
// ask*OnLaunch flags, so this is a backstop.
func ignoredFieldsError(jobID int, ignored map[string]json.RawMessage) error {
	if len(ignored) == 0 {
		return nil
	}
	keys := make([]string, 0, len(ignored))
	for k := range ignored {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return fmt.Errorf("AWX ignored launch field(s) %s for job %d: the template does not accept them at launch time "+
		"(enable Prompt on Launch for them in AWX); the run was NOT scoped as requested", strings.Join(keys, ", "), jobID)
}

// LaunchJobTemplate launches a Job Template run. A non-zero job ID is
// returned even alongside an error, so callers can still record and
// trace a run that launched with fields ignored.
func (c *AWXClient) LaunchJobTemplate(ctx context.Context, id int, limit string, extraVars map[string]string) (int, error) {
	body, err := launchBody(limit, extraVars)
	if err != nil {
		return 0, err
	}
	var out launchResponse
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("%s/job_templates/%d/launch/", c.basePath, id), body, &out); err != nil {
		return 0, fmt.Errorf("launching job template %d: %w", id, err)
	}
	return out.Job, ignoredFieldsError(out.Job, out.IgnoredFields)
}

// LaunchWorkflowJobTemplate launches a Workflow Template run, with the
// same semantics as LaunchJobTemplate.
func (c *AWXClient) LaunchWorkflowJobTemplate(ctx context.Context, id int, limit string, extraVars map[string]string) (int, error) {
	body, err := launchBody(limit, extraVars)
	if err != nil {
		return 0, err
	}
	var out launchResponse
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("%s/workflow_job_templates/%d/launch/", c.basePath, id), body, &out); err != nil {
		return 0, fmt.Errorf("launching workflow job template %d: %w", id, err)
	}
	return out.WorkflowJob, ignoredFieldsError(out.WorkflowJob, out.IgnoredFields)
}

type jobStatusResponse struct {
	Status string `json:"status"`
}

// GetJobStatus returns a job's current AWX status string (e.g. "pending",
// "running", "successful", "failed").
func (c *AWXClient) GetJobStatus(ctx context.Context, id int) (string, error) {
	var out jobStatusResponse
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s/jobs/%d/", c.basePath, id), nil, &out); err != nil {
		return "", fmt.Errorf("getting job %d status: %w", id, err)
	}
	return out.Status, nil
}

// GetWorkflowJobStatus is GetJobStatus for workflow job runs.
func (c *AWXClient) GetWorkflowJobStatus(ctx context.Context, id int) (string, error) {
	var out jobStatusResponse
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s/workflow_jobs/%d/", c.basePath, id), nil, &out); err != nil {
		return "", fmt.Errorf("getting workflow job %d status: %w", id, err)
	}
	return out.Status, nil
}

// JobURL builds a link to the run's output in the AWX UI. The exact path
// can vary across AWX/Tower versions; this targets the common modern
// AWX UI layout.
func (c *AWXClient) JobURL(id int, isWorkflow bool) string {
	kind := "playbook"
	if isWorkflow {
		kind = "workflow"
	}
	return fmt.Sprintf("%s/#/jobs/%s/%d/output", c.baseURL, kind, id)
}
