// fakeawx is a minimal in-memory stand-in for the AWX/Tower API, used only
// by the e2e suite (test/e2e.sh) so it can exercise the controller's AWX
// client without a real AWX/Tower instance. It implements just the
// handful of endpoints controller/awx_client.go calls, including the
// launch-time ask_*_on_launch semantics that decide whether AWX honors
// or silently ignores a limit / extra vars.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type templateRec struct {
	ID                   int
	Name                 string
	Inventory            *int
	AskLimitOnLaunch     bool
	AskVariablesOnLaunch bool
}

type hostRec struct {
	ID          int    `json:"id"`
	Inventory   int    `json:"inventory"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Variables   string `json:"variables"`
	Deleted     bool   `json:"deleted"`
}

type jobRec struct {
	ID       int
	Workflow bool
	Polls    int
}

type server struct {
	mu sync.Mutex

	// basePath is the API root this instance serves: "/api/v2" like AWX,
	// Tower and AAP <= 2.4, or "/api/controller/v2" like AAP 2.5+ behind
	// the platform gateway. Anything outside it 404s, so the controller's
	// base-path detection has something real to discover.
	basePath string
	// ignoreNameFilter simulates an instance that does not honor the
	// ?name= field lookup (it is not part of the published API schema),
	// so host lookups come back unfiltered.
	ignoreNameFilter bool

	nextHostID int
	nextJobID  int

	jobTemplates      []templateRec
	workflowTemplates []templateRec
	hosts             map[int]*hostRec
	jobs              map[int]*jobRec
}

func newServer(basePath string, ignoreNameFilter bool) *server {
	inv := 1
	return &server{
		basePath:         basePath,
		ignoreNameFilter: ignoreNameFilter,
		nextHostID:       100,
		nextJobID:        1000,
		jobTemplates: []templateRec{
			{ID: 1, Name: "Configure Webserver", Inventory: &inv, AskLimitOnLaunch: true, AskVariablesOnLaunch: true},
			// Deliberately does NOT accept a limit at launch: the
			// controller must refuse to launch against this one rather
			// than let AWX run it against the whole inventory.
			{ID: 3, Name: "No Prompt Template", Inventory: &inv},
		},
		workflowTemplates: []templateRec{
			{ID: 2, Name: "Configure Webserver Workflow", Inventory: &inv, AskLimitOnLaunch: true, AskVariablesOnLaunch: true},
		},
		hosts: map[int]*hostRec{},
		jobs:  map[int]*jobRec{},
	}
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *server) handlePing(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]interface{}{"version": "fake", "ha": false})
}

func (s *server) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]interface{}{"id": 1, "username": "fake"})
}

func findTemplate(list []templateRec, name string) *templateRec {
	for i := range list {
		if list[i].Name == name {
			return &list[i]
		}
	}
	return nil
}

func findTemplateByID(list []templateRec, id int) *templateRec {
	for i := range list {
		if list[i].ID == id {
			return &list[i]
		}
	}
	return nil
}

func (s *server) handleTemplateList(list []templateRec) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		t := findTemplate(list, name)
		if t == nil {
			writeJSON(w, 200, map[string]interface{}{"count": 0, "results": []interface{}{}})
			return
		}
		writeJSON(w, 200, map[string]interface{}{
			"count": 1,
			"results": []map[string]interface{}{{
				"id":                      t.ID,
				"inventory":               t.Inventory,
				"ask_limit_on_launch":     t.AskLimitOnLaunch,
				"ask_variables_on_launch": t.AskVariablesOnLaunch,
			}},
		})
	}
}

// /api/v2/inventories/{id}/hosts/?name=X  (GET list, POST create)
func (s *server) handleInventoryHosts(w http.ResponseWriter, r *http.Request, inventoryID int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch r.Method {
	case http.MethodGet:
		name := r.URL.Query().Get("name")
		results := []map[string]interface{}{}
		ids := make([]int, 0, len(s.hosts))
		for id := range s.hosts {
			ids = append(ids, id)
		}
		sort.Ints(ids)
		for _, id := range ids {
			h := s.hosts[id]
			if h.Inventory != inventoryID || h.Deleted {
				continue
			}
			// An instance that ignores ?name= hands back everything.
			if !s.ignoreNameFilter && h.Name != name {
				continue
			}
			results = append(results, map[string]interface{}{
				"id":          h.ID,
				"name":        h.Name,
				"variables":   h.Variables,
				"description": h.Description,
			})
		}
		writeJSON(w, 200, map[string]interface{}{"count": len(results), "results": results})
	case http.MethodPost:
		var body struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Variables   string `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		id := s.nextHostID
		s.nextHostID++
		s.hosts[id] = &hostRec{ID: id, Inventory: inventoryID, Name: body.Name, Description: body.Description, Variables: body.Variables}
		log.Printf("fakeawx: created host %d (%s) in inventory %d", id, body.Name, inventoryID)
		writeJSON(w, 201, map[string]interface{}{"id": id})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// /api/v2/hosts/{id}/ (PATCH update, DELETE)
func (s *server) handleHost(w http.ResponseWriter, r *http.Request, id int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	h, ok := s.hosts[id]
	switch r.Method {
	case http.MethodPatch:
		if !ok || h.Deleted {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body struct {
			Variables string `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		h.Variables = body.Variables
		log.Printf("fakeawx: patched host %d variables=%s", id, h.Variables)
		writeJSON(w, 200, map[string]interface{}{"id": h.ID})
	case http.MethodDelete:
		if ok {
			h.Deleted = true
			log.Printf("fakeawx: deleted host %d", id)
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// handleLaunch models AWX's real behavior: fields the template isn't
// configured to accept at launch time are dropped and reported back in
// ignored_fields, while the job still launches.
func (s *server) handleLaunch(list []templateRec, workflow bool) func(http.ResponseWriter, *http.Request, int) {
	return func(w http.ResponseWriter, r *http.Request, templateID int) {
		s.mu.Lock()
		defer s.mu.Unlock()

		t := findTemplateByID(list, templateID)
		if t == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)

		ignored := map[string]interface{}{}
		if v, ok := body["limit"]; ok && !t.AskLimitOnLaunch {
			ignored["limit"] = v
		}
		if v, ok := body["extra_vars"]; ok && !t.AskVariablesOnLaunch {
			ignored["extra_vars"] = v
		}

		id := s.nextJobID
		s.nextJobID++
		s.jobs[id] = &jobRec{ID: id, Workflow: workflow}
		log.Printf("fakeawx: launched job %d (template=%d workflow=%v limit=%v ignored=%v)", id, templateID, workflow, body["limit"], ignored)

		resp := map[string]interface{}{}
		if workflow {
			resp["workflow_job"] = id
		} else {
			resp["job"] = id
		}
		if len(ignored) > 0 {
			resp["ignored_fields"] = ignored
		}
		writeJSON(w, 201, resp)
	}
}

// Job status: "running" on the first poll, "successful" from the second
// poll on - exercises both the in-flight-poll and terminal reconcile
// paths without needing real async job execution.
func (s *server) handleJobStatus(w http.ResponseWriter, r *http.Request, id int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	j, ok := s.jobs[id]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	j.Polls++
	status := "running"
	if j.Polls >= 2 {
		status = "successful"
	}
	writeJSON(w, 200, map[string]interface{}{"id": id, "status": status})
}

// /_test/hosts lets the e2e script inspect and seed inventory state
// without needing a real AWX UI: GET lists every host (including deleted
// ones), POST seeds a pre-existing host to test adoption.
func (s *server) handleTestHosts(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch r.Method {
	case http.MethodGet:
		hosts := make([]hostRec, 0, len(s.hosts))
		for _, h := range s.hosts {
			hosts = append(hosts, *h)
		}
		writeJSON(w, 200, hosts)
	case http.MethodPost:
		var body struct {
			Inventory   int    `json:"inventory"`
			Name        string `json:"name"`
			Description string `json:"description"`
			Variables   string `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		id := s.nextHostID
		s.nextHostID++
		s.hosts[id] = &hostRec{ID: id, Inventory: body.Inventory, Name: body.Name, Description: body.Description, Variables: body.Variables}
		log.Printf("fakeawx: seeded pre-existing host %d (%s)", id, body.Name)
		writeJSON(w, 201, map[string]interface{}{"id": id})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *server) handleTestDeletedHosts(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	deleted := []int{}
	for id, h := range s.hosts {
		if h.Deleted {
			deleted = append(deleted, id)
		}
	}
	writeJSON(w, 200, deleted)
}

// pathID pulls the numeric id out of "/api/v2/<collection>/<id>/<rest>".
func pathID(path, prefix, suffix string) (int, bool) {
	trimmed := strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix)
	id, err := strconv.Atoi(trimmed)
	if err != nil {
		return 0, false
	}
	return id, true
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// Test-only endpoints sit outside the API tree.
	switch path {
	case "/_test/hosts":
		s.handleTestHosts(w, r)
		return
	case "/_test/deleted-hosts":
		s.handleTestDeletedHosts(w, r)
		return
	}

	if !strings.HasPrefix(path, s.basePath+"/") {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	path = strings.TrimPrefix(path, s.basePath)

	switch {
	case path == "/ping/":
		s.handlePing(w, r)
	case path == "/me/":
		s.handleMe(w, r)
	case path == "/job_templates/":
		s.handleTemplateList(s.jobTemplates)(w, r)
	case path == "/workflow_job_templates/":
		s.handleTemplateList(s.workflowTemplates)(w, r)
	case strings.HasPrefix(path, "/job_templates/") && strings.HasSuffix(path, "/launch/"):
		id, ok := pathID(path, "/job_templates/", "/launch/")
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.handleLaunch(s.jobTemplates, false)(w, r, id)
	case strings.HasPrefix(path, "/workflow_job_templates/") && strings.HasSuffix(path, "/launch/"):
		id, ok := pathID(path, "/workflow_job_templates/", "/launch/")
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.handleLaunch(s.workflowTemplates, true)(w, r, id)
	case strings.HasPrefix(path, "/inventories/") && strings.HasSuffix(path, "/hosts/"):
		id, ok := pathID(path, "/inventories/", "/hosts/")
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.handleInventoryHosts(w, r, id)
	case strings.HasPrefix(path, "/hosts/"):
		id, ok := pathID(path, "/hosts/", "/")
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.handleHost(w, r, id)
	case strings.HasPrefix(path, "/jobs/"):
		id, ok := pathID(path, "/jobs/", "/")
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.handleJobStatus(w, r, id)
	case strings.HasPrefix(path, "/workflow_jobs/"):
		id, ok := pathID(path, "/workflow_jobs/", "/")
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.handleJobStatus(w, r, id)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func main() {
	addr := flag.String("addr", ":8756", "listen address")
	basePath := flag.String("api-base-path", "/api/v2", "API root to serve: /api/v2 like AWX/Tower/AAP<=2.4, or /api/controller/v2 like AAP 2.5+")
	ignoreNameFilter := flag.Bool("ignore-name-filter", false, "ignore the ?name= host lookup filter, returning every host in the inventory")
	flag.Parse()

	s := newServer(strings.TrimRight(*basePath, "/"), *ignoreNameFilter)
	fmt.Printf("fakeawx listening on %s serving %s\n", *addr, s.basePath)
	log.Fatal(http.ListenAndServe(*addr, s))
}
