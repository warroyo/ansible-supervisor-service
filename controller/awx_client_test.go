package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
)

func testClient(t *testing.T, h http.Handler) *AWXClient {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := NewAWXClient(srv.URL, APIBasePathLegacy, "token", TLSOptions{})
	if err != nil {
		t.Fatalf("NewAWXClient: %v", err)
	}
	return c
}

// templateList serves whatever results are given, regardless of the
// ?name= filter - which is what an AWX that does not honor the
// undocumented field lookup does.
func templateList(results ...map[string]interface{}) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"count": len(results), "results": results,
		})
	})
}

func TestFindJobTemplateIgnoresNonMatchingResults(t *testing.T) {
	// The dangerous case: the filter is ignored and an unrelated
	// template comes back first. Taking results[0] would launch it.
	c := testClient(t, templateList(
		map[string]interface{}{"id": 7, "name": "Destroy Everything", "ask_limit_on_launch": true},
		map[string]interface{}{"id": 9, "name": "Configure Webserver", "ask_limit_on_launch": true},
	))
	tmpl, err := c.FindJobTemplate(context.Background(), "Configure Webserver")
	if err != nil {
		t.Fatalf("FindJobTemplate: %v", err)
	}
	if tmpl.ID != 9 {
		t.Errorf("resolved template ID = %d, want 9", tmpl.ID)
	}
}

func TestFindJobTemplateAmbiguousNameIsRefused(t *testing.T) {
	c := testClient(t, templateList(
		map[string]interface{}{"id": 3, "name": "Configure Webserver"},
		map[string]interface{}{"id": 4, "name": "Configure Webserver"},
	))
	_, err := c.FindJobTemplate(context.Background(), "Configure Webserver")
	if err == nil {
		t.Fatal("expected an error for two templates sharing a name")
	}
	for _, want := range []string{"ambiguous", "3", "4"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestFindJobTemplateNotFound(t *testing.T) {
	c := testClient(t, templateList(map[string]interface{}{"id": 1, "name": "Something Else"}))
	_, err := c.FindJobTemplate(context.Background(), "Configure Webserver")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v, want a not-found error", err)
	}
}

func TestErrorBodyIsTruncated(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(strings.Repeat("x", 4096)))
	}))
	err := c.Ping(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	// The message reaches a CR's status.message, so it must stay bounded.
	if len(err.Error()) > maxErrorBodyChars+256 {
		t.Errorf("error message is %d bytes, want it truncated", len(err.Error()))
	}
	if !strings.Contains(err.Error(), "truncated") {
		t.Errorf("error %q should say it was truncated", err)
	}
}

func TestResponseBodyIsCapped(t *testing.T) {
	// A URL pointing at something that isn't AWX can stream forever;
	// reading it unbounded is what the cap exists to prevent.
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < (maxResponseBytes/1024)+64; i++ {
			_, _ = w.Write([]byte(strings.Repeat("y", 1024)))
		}
	}))
	var out map[string]interface{}
	err := c.do(context.Background(), http.MethodGet, "/api/v2/me/", nil, &out)
	if err == nil {
		t.Fatal("expected a decode error from a capped, truncated body")
	}
}

// hostStore is a minimal stand-in for an AWX inventory.
type hostStore struct {
	hosts    map[int]map[string]interface{}
	next     int
	patched  int
	created  int
	gets     int
	pageSize int
	deleted  map[int]bool
}

func newHostStore() *hostStore {
	return &hostStore{hosts: map[int]map[string]interface{}{}, next: 1, deleted: map[int]bool{}}
}

// ids returns host IDs in insertion order, so paging over them is stable
// rather than following Go's map iteration order.
func (s *hostStore) ids() []int {
	out := make([]int, 0, len(s.hosts))
	for id := range s.hosts {
		out = append(out, id)
	}
	sort.Ints(out)
	return out
}

func (s *hostStore) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, APIBasePathLegacy)
		switch {
		case strings.HasPrefix(path, "/inventories/") && r.Method == http.MethodGet:
			s.gets++
			name := r.URL.Query().Get("name")
			description := r.URL.Query().Get("description")
			results := []map[string]interface{}{}
			for _, id := range s.ids() {
				h := s.hosts[id]
				if s.deleted[id] {
					continue
				}
				if description != "" {
					if h["description"] != description {
						continue
					}
				} else if h["name"] != name {
					continue
				}
				results = append(results, h)
			}
			// Serve a page at a time so the caller's pagination is
			// exercised, the way a real AWX does past its page size.
			body := map[string]interface{}{"count": len(results)}
			if s.pageSize > 0 && len(results) > s.pageSize {
				page := 1
				if p := r.URL.Query().Get("page"); p != "" {
					_, _ = fmt.Sscanf(p, "%d", &page)
				}
				start := (page - 1) * s.pageSize
				end := start + s.pageSize
				if end < len(results) {
					body["next"] = fmt.Sprintf("%s/inventories/1/hosts/?description=%s&page=%d",
						APIBasePathLegacy, url.QueryEscape(description), page+1)
				} else {
					end = len(results)
				}
				if start > len(results) {
					start = len(results)
				}
				results = results[start:end]
			}
			body["results"] = results
			_ = json.NewEncoder(w).Encode(body)
		case strings.HasPrefix(path, "/inventories/") && r.Method == http.MethodPost:
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			id := s.next
			s.next++
			body["id"] = id
			s.hosts[id] = body
			s.created++
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": id})
		case strings.HasPrefix(path, "/hosts/") && r.Method == http.MethodPatch:
			var id int
			_, _ = fmt.Sscanf(path, "/hosts/%d/", &id)
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.hosts[id]["variables"] = body["variables"]
			s.patched++
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": id})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func (s *hostStore) seed(name, description, variables string) int {
	id := s.next
	s.next++
	s.hosts[id] = map[string]interface{}{
		"id": id, "name": name, "description": description, "variables": variables,
	}
	return id
}

func TestUpsertHostRecreatesAHostDeletedOutOfBand(t *testing.T) {
	store := newHostStore()
	c := testClient(t, store.handler())
	marker := "ansible-supervisor:sup-1:ns/binding"
	vars := map[string]string{"ansible_host": "10.0.0.5"}

	id, owned, err := c.UpsertHost(context.Background(), 1, "web-1", marker, vars)
	if err != nil || !owned {
		t.Fatalf("first upsert: id=%d owned=%v err=%v", id, owned, err)
	}

	// Someone deletes it in the AWX UI.
	store.deleted[id] = true

	newID, owned, err := c.UpsertHost(context.Background(), 1, "web-1", marker, vars)
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if newID == id {
		t.Errorf("expected the host to be recreated under a new ID, got %d twice", id)
	}
	if !owned {
		t.Error("a recreated host is ours")
	}
	if store.created != 2 {
		t.Errorf("created %d hosts, want 2", store.created)
	}
}

func TestUpsertHostRepairsEditedVariablesButIsQuietWhenUnchanged(t *testing.T) {
	store := newHostStore()
	c := testClient(t, store.handler())
	marker := "ansible-supervisor:sup-1:ns/binding"
	vars := map[string]string{"ansible_host": "10.0.0.5"}

	id, _, err := c.UpsertHost(context.Background(), 1, "web-1", marker, vars)
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	// A steady state must not write on every pass.
	if _, _, err := c.UpsertHost(context.Background(), 1, "web-1", marker, vars); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if store.patched != 0 {
		t.Errorf("unchanged host was PATCHed %d times, want 0", store.patched)
	}

	// Someone edits ansible_host in the AWX UI; the next pass repairs it,
	// while a variable the controller does not manage is left alone.
	store.hosts[id]["variables"] = `{"ansible_host":"10.99.99.99","set_by_hand":"keep"}`
	if _, _, err := c.UpsertHost(context.Background(), 1, "web-1", marker, vars); err != nil {
		t.Fatalf("third upsert: %v", err)
	}
	if store.patched != 1 {
		t.Fatalf("drifted host PATCHed %d times, want 1", store.patched)
	}
	got := store.hosts[id]["variables"].(string)
	if !strings.Contains(got, `"ansible_host":"10.0.0.5"`) {
		t.Errorf("ansible_host was not repaired: %s", got)
	}
	if !strings.Contains(got, `"set_by_hand":"keep"`) {
		t.Errorf("unmanaged variable was dropped: %s", got)
	}
}

func TestUpsertHostRefusesAHostOwnedByAnotherBinding(t *testing.T) {
	store := newHostStore()
	store.seed("web-1", "ansible-supervisor:sup-2:other/binding", "{}")
	c := testClient(t, store.handler())

	_, _, err := c.UpsertHost(context.Background(), 1, "web-1",
		"ansible-supervisor:sup-1:ns/binding", map[string]string{"ansible_host": "10.0.0.5"})
	if err == nil || !strings.Contains(err.Error(), "already owned") {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if store.patched != 0 {
		t.Error("a foreign host must not be touched")
	}
}

func TestUpsertHostAdoptsAnUnmarkedHostWithoutClaimingIt(t *testing.T) {
	store := newHostStore()
	id := store.seed("web-1", "", `{"made_by_hand":"yes"}`)
	c := testClient(t, store.handler())

	gotID, owned, err := c.UpsertHost(context.Background(), 1, "web-1",
		"ansible-supervisor:sup-1:ns/binding", map[string]string{"ansible_host": "10.0.0.5"})
	if err != nil {
		t.Fatalf("UpsertHost: %v", err)
	}
	if gotID != id {
		t.Errorf("adopted host ID = %d, want %d", gotID, id)
	}
	if owned {
		t.Error("an unmarked host was made by hand and is not ours to delete")
	}
	if !strings.Contains(store.hosts[id]["variables"].(string), "made_by_hand") {
		t.Error("adopting a host must not discard its existing variables")
	}
}

func TestListOwnedHostsPagesAndClaimsOnlyItsOwn(t *testing.T) {
	store := newHostStore()
	store.pageSize = 2
	c := testClient(t, store.handler())
	marker := "ansible-supervisor:sup-1:ns/binding"

	for _, name := range []string{"web-1", "web-2", "web-3", "web-4", "web-5"} {
		store.seed(name, marker, "{}")
	}
	// Neither of these is ours, and neither may be claimed.
	store.seed("db-1", "ansible-supervisor:sup-1:ns/other-binding", "{}")
	store.seed("hand-made", "", "{}")

	owned, err := c.ListOwnedHosts(context.Background(), 1, marker)
	if err != nil {
		t.Fatalf("ListOwnedHosts: %v", err)
	}
	if len(owned) != 5 {
		t.Fatalf("claimed %d hosts, want 5: %v", len(owned), owned)
	}
	// Five results at two per page is three requests; without following
	// "next" only the first two hosts would come back.
	if store.gets != 3 {
		t.Errorf("made %d requests, want 3 (pagination not followed)", store.gets)
	}
	for _, name := range []string{"db-1", "hand-made"} {
		if _, claimed := owned[name]; claimed {
			t.Errorf("claimed %q, which this binding does not own", name)
		}
	}
}

// An AWX that ignores the undocumented ?description= field lookup hands
// back every host in the inventory. Claiming those would mean deleting
// hosts belonging to another binding on the next cleanup pass.
func TestListOwnedHostsRechecksTheMarkerItFilteredOn(t *testing.T) {
	marker := "ansible-supervisor:sup-1:ns/binding"
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"count": 2,
			"results": []map[string]interface{}{
				{"id": 1, "name": "web-1", "description": marker},
				{"id": 2, "name": "db-1", "description": "ansible-supervisor:sup-1:ns/other"},
			},
		})
	}))

	owned, err := c.ListOwnedHosts(context.Background(), 1, marker)
	if err != nil {
		t.Fatalf("ListOwnedHosts: %v", err)
	}
	if len(owned) != 1 {
		t.Fatalf("claimed %d hosts, want 1: %v", len(owned), owned)
	}
	if _, claimed := owned["db-1"]; claimed {
		t.Error("claimed a host owned by another binding when the filter was ignored")
	}
}

func TestUpsertKnownHostSkipsTheLookupButStillRepairsDrift(t *testing.T) {
	store := newHostStore()
	c := testClient(t, store.handler())
	marker := "ansible-supervisor:sup-1:ns/binding"
	vars := map[string]string{"ansible_host": "10.0.0.5"}

	id := store.seed("web-1", marker, `{"ansible_host":"10.0.0.5"}`)
	known := hostResult{ID: id, Name: "web-1", Description: marker, Variables: `{"ansible_host":"10.0.0.5"}`}

	gotID, owned, err := c.UpsertKnownHost(context.Background(), 1, "web-1", marker, vars, known)
	if err != nil || gotID != id || !owned {
		t.Fatalf("UpsertKnownHost: id=%d owned=%v err=%v", gotID, owned, err)
	}
	if store.gets != 0 {
		t.Errorf("made %d per-host lookups, want 0 - the host was already known", store.gets)
	}
	if store.patched != 0 {
		t.Errorf("patched %d times with nothing to change, want 0", store.patched)
	}

	// The IP moved: the variables differ from what AWX holds, so this
	// still has to be repaired without a lookup.
	drifted := hostResult{ID: id, Name: "web-1", Description: marker, Variables: `{"ansible_host":"10.0.0.9"}`}
	if _, _, err := c.UpsertKnownHost(context.Background(), 1, "web-1", marker, vars, drifted); err != nil {
		t.Fatalf("UpsertKnownHost after drift: %v", err)
	}
	if store.patched != 1 {
		t.Errorf("patched %d times, want 1", store.patched)
	}
	if store.gets != 0 {
		t.Errorf("made %d per-host lookups, want 0", store.gets)
	}
}

func TestMergeHostVariablesRefusesNonObjectVariables(t *testing.T) {
	if _, err := mergeHostVariables("- a\n- b\n", map[string]string{"x": "y"}); err == nil {
		t.Fatal("expected a refusal rather than destroying the existing variables")
	}
	for _, empty := range []string{"", "---", "{}"} {
		got, err := mergeHostVariables(empty, map[string]string{"x": "y"})
		if err != nil {
			t.Fatalf("mergeHostVariables(%q): %v", empty, err)
		}
		if got != `{"x":"y"}` {
			t.Errorf("mergeHostVariables(%q) = %s", empty, got)
		}
	}
}

func TestGetHostSeparatesGoneFromBroken(t *testing.T) {
	// The legacy cleanup path reads a host back before deleting it by an
	// id an older controller recorded, so "no such host" has to be
	// distinguishable from "AWX is unwell" - the second must not be read
	// as permission to move on.
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/hosts/7/":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id": 7, "name": "web-1", "description": "ansible-supervisor:sup-a:ns/bind",
			})
		case "/api/v2/hosts/8/":
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))

	host, err := c.GetHost(context.Background(), 7)
	if err != nil {
		t.Fatalf("GetHost: %v", err)
	}
	if host == nil || host.Name != "web-1" {
		t.Fatalf("expected the host back, got %+v", host)
	}
	if host.Description != "ansible-supervisor:sup-a:ns/bind" {
		t.Errorf("the ownership marker must survive the read, got %q", host.Description)
	}

	host, err = c.GetHost(context.Background(), 8)
	if err != nil || host != nil {
		t.Fatalf("a deleted host is (nil, nil), got (%+v, %v)", host, err)
	}

	if _, err = c.GetHost(context.Background(), 9); err == nil {
		t.Error("a 500 is not the same as a host that is gone, and must not be treated as one")
	}
}
