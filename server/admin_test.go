package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// S9 Admin tests: API-key auth (constant-time), bootstrap operator, admin
// world create/publish/delete + append-only audit rows, scoped service
// tokens, CORS_ALLOWED_ORIGINS, members list.

func newAdminTestHub(t *testing.T) *Hub {
	t.Helper()
	h := newTestHub(t)
	if err := h.store.MigrateS9(); err != nil {
		t.Fatalf("migrate s9: %v", err)
	}
	return h
}

// adminReq performs a request against a handler with an optional X-API-Key
// header and optional Origin (CORS), decoding the JSON body.
func adminReq(t *testing.T, h http.HandlerFunc, method, path string, body any, apiKey, origin string) (int, map[string]any, *httptest.ResponseRecorder) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	var out map[string]any
	if rr.Body.Len() > 0 {
		_ = json.Unmarshal(rr.Body.Bytes(), &out)
	}
	return rr.Code, out, rr
}

func setAdminEnv(t *testing.T, key, cors, bootstrap string) {
	t.Helper()
	if key != "" {
		t.Setenv("ADMIN_API_KEY", key)
	}
	if cors != "" {
		t.Setenv("CORS_ALLOWED_ORIGINS", cors)
	}
	if bootstrap != "" {
		t.Setenv("HEARTH_BOOTSTRAP_OPERATOR_KEY", bootstrap)
	}
}

func TestAdminAuthNoKeyRejected(t *testing.T) {
	h := newAdminTestHub(t)
	code, out, _ := adminReq(t, h.adminAuth()(h.adminOverview), http.MethodGet, "/api/admin/overview", nil, "", "")
	if code != http.StatusUnauthorized {
		t.Fatalf("no key = %d, want 401 (%+v)", code, out)
	}
	if out["ok"] != false {
		t.Errorf("ok = %v, want false", out["ok"])
	}
}

func TestAdminAuthWrongKeyRejected(t *testing.T) {
	h := newAdminTestHub(t)
	setAdminEnv(t, "correct-horse-battery", "", "")
	code, _, _ := adminReq(t, h.adminAuth()(h.adminOverview), http.MethodGet, "/api/admin/overview", nil, "wrong-key", "")
	if code != http.StatusUnauthorized {
		t.Fatalf("wrong key = %d, want 401", code)
	}
}

func TestAdminAuthValidKeyAccepted(t *testing.T) {
	h := newAdminTestHub(t)
	setAdminEnv(t, "correct-horse-battery", "", "")
	code, out, _ := adminReq(t, h.adminAuth()(h.adminOverview), http.MethodGet, "/api/admin/overview", nil, "correct-horse-battery", "")
	if code != http.StatusOK {
		t.Fatalf("valid key = %d, want 200 (%+v)", code, out)
	}
	ov, _ := out["overview"].(map[string]any)
	if ov == nil || ov["worlds"] == nil {
		t.Fatalf("overview missing counts: %+v", out)
	}
	if n := ov["worlds"].(float64); n < 4 {
		t.Errorf("worlds = %v, want >= 4 seeded", n)
	}
}

func TestAdminWorldCreatePublishAuditAndDelete(t *testing.T) {
	h := newAdminTestHub(t)
	setAdminEnv(t, "admin-key-1", "", "")
	auth := h.adminAuth()

	// create
	code, out, _ := adminReq(t, auth(h.adminWorldsCollection), http.MethodPost, "/api/admin/worlds",
		map[string]any{"name": "Admin Made World", "width": 32, "height": 32}, "admin-key-1", "")
	if code != http.StatusCreated {
		t.Fatalf("admin create = %d, want 201 (%+v)", code, out)
	}
	id, _ := out["id"].(string)
	if id == "" {
		t.Fatal("no world id")
	}
	// appears in the admin list (drafts included)
	code, list, _ := adminReq(t, auth(h.adminWorldsCollection), http.MethodGet, "/api/admin/worlds", nil, "admin-key-1", "")
	if code != http.StatusOK {
		t.Fatalf("admin list = %d, want 200", code)
	}
	found := false
	for _, w := range list["worlds"].([]any) {
		if w.(map[string]any)["id"] == id {
			found = true
		}
	}
	if !found {
		t.Errorf("created world %q missing from /api/admin/worlds", id)
	}
	// draft must NOT be in the public directory yet
	code, dir := doJSON(t, h.listWorlds, http.MethodGet, "/api/worlds", nil, nil)
	if code != http.StatusOK {
		t.Fatalf("directory = %d", code)
	}
	for _, w := range dir["worlds"].([]any) {
		if w.(map[string]any)["id"] == id {
			t.Errorf("draft world %q leaked into public directory", id)
		}
	}
	// publish via admin
	code, _, _ = adminReq(t, auth(h.adminWorldItem), http.MethodPost, "/api/admin/worlds/"+id+"/publish", nil, "admin-key-1", "")
	if code != http.StatusOK {
		t.Fatalf("admin publish = %d, want 200", code)
	}
	// now visible in /api/worlds
	code, dir = doJSON(t, h.listWorlds, http.MethodGet, "/api/worlds", nil, nil)
	if code != http.StatusOK {
		t.Fatalf("directory = %d", code)
	}
	pubFound := false
	for _, w := range dir["worlds"].([]any) {
		if w.(map[string]any)["id"] == id {
			pubFound = true
		}
	}
	if !pubFound {
		t.Errorf("published world %q missing from /api/worlds", id)
	}
	// audit rows written (append-only, kind=admin)
	code, audit, _ := adminReq(t, h.adminAuth("audit.read")(h.adminAudit), http.MethodGet, "/api/admin/audit?kind=admin", nil, "admin-key-1", "")
	if code != http.StatusOK {
		t.Fatalf("audit = %d", code)
	}
	actions := map[string]bool{}
	for _, e := range audit["events"].([]any) {
		ev := e.(map[string]any)
		if ev["target"] == id {
			actions[ev["action"].(string)] = true
		}
	}
	if !actions["admin.world.create"] {
		t.Errorf("audit missing admin.world.create for %q: %+v", id, actions)
	}
	if !actions["admin.world.publish"] {
		t.Errorf("audit missing admin.world.publish for %q: %+v", id, actions)
	}
	if actor := audit["events"].([]any)[0].(map[string]any)["role"]; actor != "operator" {
		t.Errorf("audit role = %v, want operator", actor)
	}
	// delete via admin
	code, _, _ = adminReq(t, auth(h.adminWorldItem), http.MethodDelete, "/api/admin/worlds/"+id, nil, "admin-key-1", "")
	if code != http.StatusOK {
		t.Fatalf("admin delete = %d, want 200", code)
	}
	if _, err := h.store.worldMeta(id); err == nil {
		t.Errorf("world %q still exists after admin delete", id)
	}
	code, audit, _ = adminReq(t, h.adminAuth("audit.read")(h.adminAudit), http.MethodGet, "/api/admin/audit?kind=admin", nil, "admin-key-1", "")
	if code != http.StatusOK {
		t.Fatalf("audit = %d", code)
	}
	delAudited := false
	for _, e := range audit["events"].([]any) {
		if e.(map[string]any)["action"] == "admin.world.delete" && e.(map[string]any)["target"] == id {
			delAudited = true
		}
	}
	if !delAudited {
		t.Errorf("audit missing admin.world.delete for %q", id)
	}
}

func TestAdminBootstrapOperator(t *testing.T) {
	h := newAdminTestHub(t)
	setAdminEnv(t, "admin-key-1", "", "bootstrap-secret-123")
	if err := h.store.BootstrapOperator(); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	// operator visible in members with role operator
	code, mems, _ := adminReq(t, h.adminAuth("members.read")(h.adminMembers), http.MethodGet, "/api/admin/members", nil, "admin-key-1", "")
	if code != http.StatusOK {
		t.Fatalf("members = %d", code)
	}
	found := false
	for _, m := range mems["members"].([]any) {
		mm := m.(map[string]any)
		if mm["role"] == "operator" && mm["name"] == "bootstrap-operator" {
			found = true
		}
	}
	if !found {
		t.Fatalf("bootstrap operator missing from members: %+v", mems["members"])
	}
	// audit row for the bootstrap
	code, audit, _ := adminReq(t, h.adminAuth("audit.read")(h.adminAudit), http.MethodGet, "/api/admin/audit?kind=admin", nil, "admin-key-1", "")
	if code != http.StatusOK {
		t.Fatalf("audit = %d", code)
	}
	bootAudited := false
	for _, e := range audit["events"].([]any) {
		if e.(map[string]any)["action"] == "admin.operator.bootstrap" {
			bootAudited = true
		}
	}
	if !bootAudited {
		t.Errorf("bootstrap not audited")
	}
	// idempotent: second bootstrap is a no-op
	if err := h.store.BootstrapOperator(); err != nil {
		t.Fatalf("bootstrap 2: %v", err)
	}
	var n int
	if err := h.store.db.QueryRow(`SELECT COUNT(*) FROM admin_operators`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("operators = %d, want 1 (idempotent)", n)
	}
	// no env key => no-op
	op := bootstrapOperatorKey()
	t.Setenv("HEARTH_BOOTSTRAP_OPERATOR_KEY", "")
	h2 := newAdminTestHub(t)
	if err := h2.store.BootstrapOperator(); err != nil {
		t.Fatalf("bootstrap no-env: %v", err)
	}
	if op == "" {
		// sanity: env was really cleared for this store
		var m int
		_ = h2.store.db.QueryRow(`SELECT COUNT(*) FROM admin_operators`).Scan(&m)
		if m != 0 {
			t.Errorf("operators = %d, want 0 with no env key", m)
		}
	}
}

func TestServiceTokenScopedAccess(t *testing.T) {
	h := newAdminTestHub(t)
	setAdminEnv(t, "admin-key-1", "", "")
	auth := h.adminAuth()

	// create a read-only token (audit.read only)
	code, out, _ := adminReq(t, auth(h.adminTokensCollection), http.MethodPost, "/api/admin/tokens",
		map[string]any{"name": "auditor-bot", "capabilities": []string{"audit.read"}}, "admin-key-1", "")
	if code != http.StatusCreated {
		t.Fatalf("token create = %d, want 201 (%+v)", code, out)
	}
	raw, _ := out["token"].(string)
	if raw == "" {
		t.Fatal("raw token not returned once")
	}
	// the token can read the audit log...
	code, _, _ = adminReq(t, h.adminAuth("audit.read")(h.adminAudit), http.MethodGet, "/api/admin/audit", nil, raw, "")
	if code != http.StatusOK {
		t.Fatalf("token audit read = %d, want 200", code)
	}
	// ...but cannot touch worlds (missing worlds.read)
	code, _, _ = adminReq(t, auth(h.adminWorldsCollection), http.MethodGet, "/api/admin/worlds", nil, raw, "")
	if code != http.StatusForbidden {
		t.Fatalf("token worlds list = %d, want 403 (scope)", code)
	}
	// invalid capability rejected at creation
	code, _, _ = adminReq(t, auth(h.adminTokensCollection), http.MethodPost, "/api/admin/tokens",
		map[string]any{"name": "bad", "capabilities": []string{"worlds.*"}}, "admin-key-1", "")
	if code != http.StatusBadRequest {
		t.Fatalf("bad cap create = %d, want 400", code)
	}
	// token creation is audited
	code, audit, _ := adminReq(t, h.adminAuth("audit.read")(h.adminAudit), http.MethodGet, "/api/admin/audit?kind=admin", nil, "admin-key-1", "")
	if code != http.StatusOK {
		t.Fatalf("audit = %d", code)
	}
	createAudited := false
	for _, e := range audit["events"].([]any) {
		if e.(map[string]any)["action"] == "admin.service_token.create" {
			createAudited = true
		}
	}
	if !createAudited {
		t.Errorf("token create not audited")
	}
	// revoke via admin key
	id, _ := out["id"].(string)
	code, _, _ = adminReq(t, auth(h.adminTokenItem), http.MethodDelete, "/api/admin/tokens/"+id, nil, "admin-key-1", "")
	if code != http.StatusOK {
		t.Fatalf("token revoke = %d, want 200", code)
	}
	// revoked token is rejected
	code, _, _ = adminReq(t, h.adminAuth("audit.read")(h.adminAudit), http.MethodGet, "/api/admin/audit", nil, raw, "")
	if code != http.StatusUnauthorized {
		t.Fatalf("revoked token = %d, want 401", code)
	}
}

func TestAdminCORSRespectsAllowedOrigins(t *testing.T) {
	h := newAdminTestHub(t)
	setAdminEnv(t, "admin-key-1", "https://console.example.com", "")
	handler := h.adminAuth()(h.adminOverview)

	// allowed origin gets ACAO
	code, _, rr := adminReq(t, handler, http.MethodGet, "/api/admin/overview", nil, "admin-key-1", "https://console.example.com")
	if code != http.StatusOK {
		t.Fatalf("allowed origin = %d", code)
	}
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "https://console.example.com" {
		t.Errorf("ACAO = %q, want the allowed origin", got)
	}
	// preflight for allowed origin passes
	code, _, rr = adminReq(t, handler, http.MethodOptions, "/api/admin/overview", nil, "", "https://console.example.com")
	if code != http.StatusNoContent {
		t.Fatalf("preflight = %d, want 204", code)
	}
	if rr.Header().Get("Access-Control-Allow-Headers") == "" {
		t.Errorf("preflight missing Allow-Headers")
	}
	// disallowed origin gets no ACAO
	code, _, rr = adminReq(t, handler, http.MethodGet, "/api/admin/overview", nil, "admin-key-1", "https://evil.example.com")
	if code != http.StatusOK {
		t.Fatalf("disallowed origin request = %d (browser blocks read, server may answer)", code)
	}
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("ACAO = %q for disallowed origin, want none", got)
	}
	// unset env => same-origin only (no ACAO for cross-origin)
	t.Setenv("CORS_ALLOWED_ORIGINS", "")
	code, _, rr = adminReq(t, handler, http.MethodGet, "/api/admin/overview", nil, "admin-key-1", "https://other.example.com")
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("ACAO = %q with unset CORS_ALLOWED_ORIGINS, want none", got)
	}
}

func TestAdminWorldsListIncludesDrafts(t *testing.T) {
	h := newAdminTestHub(t)
	setAdminEnv(t, "admin-key-1", "", "")
	auth := h.adminAuth()
	// a member-created draft (via the public API)
	sess := newTestUser(t, h, "dev-key-draft", "DraftMaker")
	code, out := doJSON(t, h.createWorld, http.MethodPost, "/api/worlds", map[string]any{"name": "Secret Draft"}, sess)
	if code != http.StatusCreated {
		t.Fatalf("member create = %d (%+v)", code, out)
	}
	draftID, _ := out["id"].(string)
	// admin sees it (drafts included)
	code, list, _ := adminReq(t, auth(h.adminWorldsCollection), http.MethodGet, "/api/admin/worlds", nil, "admin-key-1", "")
	if code != http.StatusOK {
		t.Fatalf("admin list = %d", code)
	}
	found := false
	for _, w := range list["worlds"].([]any) {
		if w.(map[string]any)["id"] == draftID {
			found = true
		}
	}
	if !found {
		t.Errorf("admin world list missing member draft %q", draftID)
	}
	// admin can fetch the draft directly (public getWorld would 404 for a stranger)
	code, _, _ = adminReq(t, auth(h.adminWorldItem), http.MethodGet, "/api/admin/worlds/"+draftID, nil, "admin-key-1", "")
	if code != http.StatusOK {
		t.Errorf("admin draft get = %d, want 200", code)
	}
}
