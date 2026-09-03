package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	hosts   map[int]map[string]interface{}
	next    int
	patched int
	created int
	deleted map[int]bool
}

func newHostStore() *hostStore {
	return &hostStore{hosts: map[int]map[string]interface{}{}, next: 1, deleted: map[int]bool{}}
}

func (s *hostStore) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, APIBasePathLegacy)
		switch {
		case strings.HasPrefix(path, "/inventories/") && r.Method == http.MethodGet:
			name := r.URL.Query().Get("name")
			results := []map[string]interface{}{}
			for id, h := range s.hosts {
				if s.deleted[id] || h["name"] != name {
					continue
				}
				results = append(results, h)
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"count": len(results), "results": results})
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
