// Route tests for the dashboard endpoints (round 6, DASHBOARDS-REWORK.md):
// GET/POST /api/v1/dashboards, PATCH/DELETE /api/v1/dashboards/{id},
// GET/POST /api/v1/dashboards/{id}/widgets,
// PATCH/DELETE /api/v1/dashboards/{id}/widgets/{wid}, and
// PUT /api/v1/dashboards/{id}/widgets/order.
// All in-memory: memDashStore/memWidgetStore share one backing world and
// mirror the repo semantics the handlers depend on (user-id scoping —
// a foreign id is treated as absent, existence is not disclosed; names
// trimmed on write; a no-op rename keeps updated_at; a new widget's
// sort_order is the dashboard's MAX+1, gaps from deletes allowed;
// deleting a dashboard cascades to its widgets; SetOrder rewrites
// sort_order = index+1) and inject per-operation errors for the 500
// mapping table. The shared helpers (authHdr, apiErr, idFrom,
// nextTestID, doReq) live in accounts_test.go and settings_test.go.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"finance-tracker/internal/repo"

	"github.com/lestrrat-go/jwx/v3/jwk"
)

// memDashRow is one fake dashboard row.
type memDashRow struct {
	userID string
	d      repo.Dashboard
}

// memWidgetRow is one fake widget row.
type memWidgetRow struct {
	userID      string
	dashboardID string
	w           repo.DashboardWidget
}

// memDashWorld is the shared backing state of the in-memory stores:
// dashboards in creation order (created_at asc) and widgets in creation
// order (display order is sort_order; List sorts a copy).
type memDashWorld struct {
	dashboards []memDashRow
	widgets    []memWidgetRow
}

// memDashStore is an in-memory DashboardStore.
type memDashStore struct {
	world *memDashWorld
	// Per-operation error injection for the 500 mapping table.
	failList, failCreate, failUpdate, failDelete error
}

// memWidgetStore is an in-memory DashboardWidgetStore.
type memWidgetStore struct {
	world *memDashWorld
	// Per-operation error injection for the 500 mapping table.
	failList, failGet, failCreate, failUpdate, failDelete, failSetOrder error
}

// newMemDashStores builds the two stores over one shared world.
func newMemDashStores() (*memDashStore, *memWidgetStore) {
	world := &memDashWorld{}
	return &memDashStore{world: world}, &memWidgetStore{world: world}
}

// memNameOK mirrors the repo's name rule (empty or >255 runes is the
// same invalid-name sentinel the handlers map to 400s upstream of the
// store, so any non-nil error is fine here).
func memNameOK(name string) error {
	if strings.TrimSpace(name) == "" || len([]rune(name)) > 255 {
		return errors.New("invalid name")
	}
	return nil
}

func (w *memDashWorld) hasDashboard(userID, dashboardID string) bool {
	for _, r := range w.dashboards {
		if r.userID == userID && r.d.ID == dashboardID {
			return true
		}
	}
	return false
}

func (s *memDashStore) List(_ context.Context, userID string, _ []byte) ([]repo.Dashboard, error) {
	if s.failList != nil {
		return nil, s.failList
	}
	var out []repo.Dashboard
	for _, r := range s.world.dashboards {
		if r.userID == userID {
			out = append(out, r.d)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (s *memDashStore) Create(_ context.Context, userID string, in repo.DashboardIn, _ []byte) (repo.Dashboard, error) {
	if s.failCreate != nil {
		return repo.Dashboard{}, s.failCreate
	}
	name := strings.TrimSpace(in.Name)
	if name == "" || len([]rune(name)) > 255 {
		return repo.Dashboard{}, errors.New("repo: invalid dashboard name")
	}
	now := time.Now()
	d := repo.Dashboard{ID: nextTestID(), Name: name, WidgetCount: 0, CreatedAt: now, UpdatedAt: now}
	s.world.dashboards = append(s.world.dashboards, memDashRow{userID: userID, d: d})
	return d, nil
}

func (s *memDashStore) Update(_ context.Context, userID, id string, p repo.DashboardPatch, _ []byte) (repo.Dashboard, error) {
	if s.failUpdate != nil {
		return repo.Dashboard{}, s.failUpdate
	}
	i := -1
	for j, r := range s.world.dashboards {
		if r.userID == userID && r.d.ID == id {
			i = j
			break
		}
	}
	if i < 0 {
		return repo.Dashboard{}, repo.ErrDashboardNotFound
	}
	if p.Name != nil {
		name := strings.TrimSpace(*p.Name)
		if name == "" || len([]rune(name)) > 255 {
			return repo.Dashboard{}, errors.New("repo: invalid dashboard name")
		}
		if name != s.world.dashboards[i].d.Name {
			s.world.dashboards[i].d.Name = name
			s.world.dashboards[i].d.UpdatedAt = time.Now()
		}
	}
	return s.world.dashboards[i].d, nil
}

func (s *memDashStore) Delete(_ context.Context, userID, id string) error {
	if s.failDelete != nil {
		return s.failDelete
	}
	for i, r := range s.world.dashboards {
		if r.userID == userID && r.d.ID == id {
			s.world.dashboards = append(s.world.dashboards[:i], s.world.dashboards[i+1:]...)
			// Cascade: the 0013 dashboard_widgets FK is ON DELETE CASCADE.
			for j := len(s.world.widgets) - 1; j >= 0; j-- {
				if s.world.widgets[j].userID == userID && s.world.widgets[j].dashboardID == id {
					s.world.widgets = append(s.world.widgets[:j], s.world.widgets[j+1:]...)
				}
			}
			return nil
		}
	}
	return repo.ErrDashboardNotFound
}

func (s *memWidgetStore) List(_ context.Context, userID, dashboardID string, _ []byte) ([]repo.DashboardWidget, error) {
	if s.failList != nil {
		return nil, s.failList
	}
	// A dashboard id that is not the user's behaves as absent (the repo
	// checks dashboard ownership first).
	if !s.world.hasDashboard(userID, dashboardID) {
		return nil, repo.ErrDashboardNotFound
	}
	var out []repo.DashboardWidget
	for _, r := range s.world.widgets {
		if r.userID == userID && r.dashboardID == dashboardID {
			out = append(out, r.w)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SortOrder != out[j].SortOrder {
			return out[i].SortOrder < out[j].SortOrder
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (s *memWidgetStore) Get(_ context.Context, userID, dashboardID, widgetID string, _ []byte) (repo.DashboardWidget, error) {
	if s.failGet != nil {
		return repo.DashboardWidget{}, s.failGet
	}
	for _, r := range s.world.widgets {
		if r.userID == userID && r.dashboardID == dashboardID && r.w.ID == widgetID {
			return r.w, nil
		}
	}
	return repo.DashboardWidget{}, repo.ErrDashboardWidgetNotFound
}

func (s *memWidgetStore) Create(_ context.Context, userID, dashboardID string, in repo.DashboardWidgetIn, _ []byte) (repo.DashboardWidget, error) {
	if s.failCreate != nil {
		return repo.DashboardWidget{}, s.failCreate
	}
	if !s.world.hasDashboard(userID, dashboardID) {
		return repo.DashboardWidget{}, repo.ErrDashboardNotFound
	}
	name := strings.TrimSpace(in.Name)
	if name == "" || len([]rune(name)) > 255 {
		return repo.DashboardWidget{}, errors.New("repo: invalid widget name")
	}
	max := 0
	for _, r := range s.world.widgets {
		if r.userID == userID && r.dashboardID == dashboardID && r.w.SortOrder > max {
			max = r.w.SortOrder
		}
	}
	now := time.Now()
	w := repo.DashboardWidget{
		ID:        nextTestID(),
		Name:      name,
		Kind:      in.Kind,
		Content:   in.Content,
		Span:      in.Span,
		RowSpan:   in.RowSpan,
		SortOrder: max + 1,
		CreatedAt: now,
		UpdatedAt: now,
	}
	s.world.widgets = append(s.world.widgets, memWidgetRow{userID: userID, dashboardID: dashboardID, w: w})
	for i, r := range s.world.dashboards {
		if r.d.ID == dashboardID {
			s.world.dashboards[i].d.WidgetCount++
		}
	}
	return w, nil
}

func (s *memWidgetStore) Update(_ context.Context, userID, dashboardID, widgetID string, p repo.DashboardWidgetPatch, _ []byte) (repo.DashboardWidget, error) {
	if s.failUpdate != nil {
		return repo.DashboardWidget{}, s.failUpdate
	}
	i := -1
	for j, r := range s.world.widgets {
		if r.userID == userID && r.dashboardID == dashboardID && r.w.ID == widgetID {
			i = j
			break
		}
	}
	if i < 0 {
		return repo.DashboardWidget{}, repo.ErrDashboardWidgetNotFound
	}
	if p.Name != nil {
		name := strings.TrimSpace(*p.Name)
		if name == "" || len([]rune(name)) > 255 {
			return repo.DashboardWidget{}, errors.New("repo: invalid widget name")
		}
		s.world.widgets[i].w.Name = name
	}
	if p.Kind != nil {
		s.world.widgets[i].w.Kind = *p.Kind
	}
	if p.Content != nil {
		s.world.widgets[i].w.Content = *p.Content
	}
	if p.Span != nil {
		s.world.widgets[i].w.Span = *p.Span
	}
	if p.RowSpan != nil {
		s.world.widgets[i].w.RowSpan = *p.RowSpan
	}
	s.world.widgets[i].w.UpdatedAt = time.Now()
	return s.world.widgets[i].w, nil
}

func (s *memWidgetStore) Delete(_ context.Context, userID, dashboardID, widgetID string) error {
	if s.failDelete != nil {
		return s.failDelete
	}
	for i, r := range s.world.widgets {
		if r.userID == userID && r.dashboardID == dashboardID && r.w.ID == widgetID {
			s.world.widgets = append(s.world.widgets[:i], s.world.widgets[i+1:]...)
			for j, d := range s.world.dashboards {
				if d.d.ID == dashboardID && d.d.WidgetCount > 0 {
					s.world.dashboards[j].d.WidgetCount--
				}
			}
			return nil
		}
	}
	return repo.ErrDashboardWidgetNotFound
}

func (s *memWidgetStore) SetOrder(_ context.Context, userID, dashboardID string, ids []string) error {
	if s.failSetOrder != nil {
		return s.failSetOrder
	}
	if !s.world.hasDashboard(userID, dashboardID) {
		return repo.ErrDashboardNotFound
	}
	byID := make(map[string]int, len(s.world.widgets))
	for j, r := range s.world.widgets {
		if r.userID == userID && r.dashboardID == dashboardID {
			byID[r.w.ID] = j
		}
	}
	if len(byID) != len(ids) {
		return repo.ErrDashboardWidgetNotFound
	}
	for i, id := range ids {
		j, ok := byID[id]
		if !ok {
			return repo.ErrDashboardWidgetNotFound
		}
		s.world.widgets[j].w.SortOrder = i + 1
	}
	return nil
}

// newDashServer builds the router wired to the given dashboard stores
// (nil allowed — that is the 503 path). The key is returned so tests
// can mint tokens for several users under the same verifier.
func newDashServer(t *testing.T, dash DashboardStore, wid DashboardWidgetStore) (http.Handler, jwk.Key) {
	t.Helper()
	key := mintKey(t)
	b, err := NewBootstrap(newMemStore(), testKEK(t), nil)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return New(
		fakeReady{}, BuildInfo{}, liveAuth(t, key), b,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		dash, wid, nil, nil, false, nil, nil, nil,
	), key
}

// dashJSON decodes a response body into a map, failing the test if it
// is not a JSON object.
func dashJSON(t *testing.T, out string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, out)
	}
	return m
}

// TestDashboardsCRUD walks the dashboard collection and item routes:
// fresh empty list, create (name trimmed, widget_count 0, no
// timestamps in the DTO), list in creation order, rename, a no-op
// rename that must not move updated_at, delete (204, empty body) and
// the 404 on a second delete.
func TestDashboardsCRUD(t *testing.T) {
	dash, wid := newMemDashStores()
	h, key := newDashServer(t, dash, wid)
	tok := mintIdentityToken(t, key, "u-a", "u-a@example.com", "User A", "ua")
	hdr := authHdr(tok)

	// Fresh list is exactly [].
	code, out := doReq(t, h, http.MethodGet, "/api/v1/dashboards", "", hdr)
	if code != http.StatusOK || strings.TrimSpace(out) != "[]" {
		t.Fatalf("fresh list = %d %q, want 200 []", code, out)
	}

	// Create: the name is trimmed on write, widget_count 0, and the
	// DTO carries no timestamps.
	code, out = doReq(t, h, http.MethodPost, "/api/v1/dashboards", `{"name":"  Home  "}`, hdr)
	if code != http.StatusCreated {
		t.Fatalf("create: status = %d, want 201 (body %s)", code, out)
	}
	row := dashJSON(t, out)
	if row["name"] != "Home" || row["widget_count"] != float64(0) {
		t.Fatalf("create row = %v, want name Home widget_count 0", row)
	}
	if _, ok := row["created_at"]; ok {
		t.Errorf("dashboard DTO leaks created_at: %v", row)
	}
	id := idFrom(t, out)

	code, out = doReq(t, h, http.MethodPost, "/api/v1/dashboards", `{"name":"Work"}`, hdr)
	if code != http.StatusCreated {
		t.Fatalf("create 2: status = %d (body %s)", code, out)
	}
	id2 := idFrom(t, out)

	// List is in creation order.
	code, out = doReq(t, h, http.MethodGet, "/api/v1/dashboards", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("list: status = %d (body %s)", code, out)
	}
	type dashRow struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		WidgetCount int    `json:"widget_count"`
	}
	var rows []dashRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("list body is not JSON: %v (%s)", err, out)
	}
	if len(rows) != 2 || rows[0].ID != id || rows[1].ID != id2 {
		t.Fatalf("list = %+v, want creation order [%s, %s]", rows, id, id2)
	}
	if rows[0].Name != "Home" || rows[1].Name != "Work" {
		t.Fatalf("names = %q/%q, want Home/Work", rows[0].Name, rows[1].Name)
	}

	// Rename: 200, the name changes, widget_count untouched.
	code, out = doReq(t, h, http.MethodPatch, "/api/v1/dashboards/"+id, `{"name":"Home (renamed)"}`, hdr)
	if code != http.StatusOK {
		t.Fatalf("patch: status = %d (body %s)", code, out)
	}
	row = dashJSON(t, out)
	if row["name"] != "Home (renamed)" || row["widget_count"] != float64(0) {
		t.Fatalf("patch row = %v, want the rename", row)
	}

	// A no-op rename (trimmed-equal) must not move updated_at.
	before := dash.world.dashboards[0].d.UpdatedAt
	time.Sleep(5 * time.Millisecond)
	code, out = doReq(t, h, http.MethodPatch, "/api/v1/dashboards/"+id, `{"name":"  Home (renamed)  "}`, hdr)
	if code != http.StatusOK {
		t.Fatalf("no-op patch: status = %d (body %s)", code, out)
	}
	if after := dash.world.dashboards[0].d.UpdatedAt; !after.Equal(before) {
		t.Errorf("no-op rename moved updated_at: %s -> %s", before, after)
	}

	// Delete: 204 with an empty body; re-delete: 404.
	code, out = doReq(t, h, http.MethodDelete, "/api/v1/dashboards/"+id, "", hdr)
	if code != http.StatusNoContent || out != "" {
		t.Fatalf("delete = %d %q, want 204 empty", code, out)
	}
	code, out = doReq(t, h, http.MethodDelete, "/api/v1/dashboards/"+id, "", hdr)
	if code != http.StatusNotFound {
		t.Fatalf("re-delete: status = %d, want 404 (body %s)", code, out)
	}
	if c, m := apiErr(t, out); c != "not_found" || m != "dashboard not found" {
		t.Fatalf("re-delete envelope = %s %q, want not_found/dashboard not found", c, m)
	}
}

// TestDashboardWidgetsCRUD walks the widget routes on one dashboard:
// create with the explore defaults ("" content, span 6, row_span 4),
// an explicit row_span, the markdown row_span default (2), MAX+1
// sort_order, widget_count tracking, rename/content PATCHes that keep
// the other fields, a kind swap carrying fresh in-cap content, span and
// row_span edits, delete (204, count back down, 404 re-delete) and
// MAX+1 after a gap.
func TestDashboardWidgetsCRUD(t *testing.T) {
	dash, wid := newMemDashStores()
	h, key := newDashServer(t, dash, wid)
	tok := mintIdentityToken(t, key, "u-a", "u-a@example.com", "User A", "ua")
	hdr := authHdr(tok)

	code, out := doReq(t, h, http.MethodPost, "/api/v1/dashboards", `{"name":"Board"}`, hdr)
	if code != http.StatusCreated {
		t.Fatalf("create dashboard: %d (%s)", code, out)
	}
	d := idFrom(t, out)

	// Explore with no content, no span and no row_span: the defaults
	// "", 6 and 4, sort 1.
	code, out = doReq(t, h, http.MethodPost, "/api/v1/dashboards/"+d+"/widgets", `{"name":"All","kind":"explore"}`, hdr)
	if code != http.StatusCreated {
		t.Fatalf("create widget: %d (%s)", code, out)
	}
	w1 := dashJSON(t, out)
	if w1["name"] != "All" || w1["kind"] != "explore" || w1["content"] != "" ||
		w1["span"] != float64(6) || w1["row_span"] != float64(4) || w1["sort_order"] != float64(1) {
		t.Fatalf("widget 1 = %v, want the explore defaults", w1)
	}
	if _, ok := w1["created_at"]; ok {
		t.Errorf("widget DTO leaks created_at: %v", w1)
	}
	id1 := idFrom(t, out)

	// Explore with a query codec and an explicit row_span: both
	// round-trip verbatim.
	code, out = doReq(t, h, http.MethodPost, "/api/v1/dashboards/"+d+"/widgets",
		`{"name":"Savings bar","kind":"explore","content":"from=2025-01-01&to=2025-02-01&chart=bar&group=month","span":4,"row_span":3}`, hdr)
	if code != http.StatusCreated {
		t.Fatalf("create widget 2: %d (%s)", code, out)
	}
	w2 := dashJSON(t, out)
	if w2["content"] != "from=2025-01-01&to=2025-02-01&chart=bar&group=month" ||
		w2["span"] != float64(4) || w2["row_span"] != float64(3) || w2["sort_order"] != float64(2) {
		t.Fatalf("widget 2 = %v", w2)
	}
	id2 := idFrom(t, out)

	// Markdown: the row_span default is 2 (explore is 4); the
	// dashboard reports widget_count 3.
	code, out = doReq(t, h, http.MethodPost, "/api/v1/dashboards/"+d+"/widgets",
		`{"name":"Notes","kind":"markdown","content":"# hi","span":12}`, hdr)
	if code != http.StatusCreated {
		t.Fatalf("create widget 3: %d (%s)", code, out)
	}
	if w3 := dashJSON(t, out); w3["span"] != float64(12) || w3["row_span"] != float64(2) || w3["sort_order"] != float64(3) {
		t.Fatalf("widget 3 = %v, want the markdown row_span default 2", w3)
	}
	code, out = doReq(t, h, http.MethodGet, "/api/v1/dashboards", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("list dashboards: %d (%s)", code, out)
	}
	type dashRow struct {
		ID          string `json:"id"`
		WidgetCount int    `json:"widget_count"`
	}
	var drows []dashRow
	if err := json.Unmarshal([]byte(out), &drows); err != nil {
		t.Fatalf("list dashboards: %v (%s)", err, out)
	}
	if len(drows) != 1 || drows[0].WidgetCount != 3 {
		t.Fatalf("widget_count = %+v, want 3", drows)
	}

	// List widgets: sort order, all three.
	code, out = doReq(t, h, http.MethodGet, "/api/v1/dashboards/"+d+"/widgets", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("list widgets: %d (%s)", code, out)
	}
	type widgetRow struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		Content   string `json:"content"`
		Span      int    `json:"span"`
		RowSpan   int    `json:"row_span"`
		SortOrder int    `json:"sort_order"`
	}
	var wrows []widgetRow
	if err := json.Unmarshal([]byte(out), &wrows); err != nil {
		t.Fatalf("list widgets: %v (%s)", err, out)
	}
	if len(wrows) != 3 || wrows[0].ID != id1 || wrows[1].ID != id2 {
		t.Fatalf("widget list = %+v, want [w1 w2 w3] in sort order", wrows)
	}

	// Rename: content, span, row_span and sort_order are untouched.
	code, out = doReq(t, h, http.MethodPatch, "/api/v1/dashboards/"+d+"/widgets/"+id1, `{"name":"All (renamed)"}`, hdr)
	if code != http.StatusOK {
		t.Fatalf("rename: %d (%s)", code, out)
	}
	w1 = dashJSON(t, out)
	if w1["name"] != "All (renamed)" || w1["content"] != "" ||
		w1["span"] != float64(6) || w1["row_span"] != float64(4) || w1["sort_order"] != float64(1) {
		t.Fatalf("after rename = %v", w1)
	}

	// Content: the name is untouched.
	code, out = doReq(t, h, http.MethodPatch, "/api/v1/dashboards/"+d+"/widgets/"+id1, `{"content":"chart=bar"}`, hdr)
	if code != http.StatusOK {
		t.Fatalf("patch content: %d (%s)", code, out)
	}
	if w1 = dashJSON(t, out); w1["name"] != "All (renamed)" || w1["content"] != "chart=bar" {
		t.Fatalf("after content patch = %v", w1)
	}

	// A kind swap carrying fresh in-cap content: 200.
	code, out = doReq(t, h, http.MethodPatch, "/api/v1/dashboards/"+d+"/widgets/"+id2,
		`{"kind":"markdown","content":"# now markdown"}`, hdr)
	if code != http.StatusOK {
		t.Fatalf("kind swap: %d (%s)", code, out)
	}
	w2 = dashJSON(t, out)
	if w2["kind"] != "markdown" || w2["content"] != "# now markdown" || w2["name"] != "Savings bar" {
		t.Fatalf("after kind swap = %v", w2)
	}

	// Span edits: 12 and 1 are both valid.
	code, out = doReq(t, h, http.MethodPatch, "/api/v1/dashboards/"+d+"/widgets/"+id1, `{"span":12}`, hdr)
	if code != http.StatusOK || dashJSON(t, out)["span"] != float64(12) {
		t.Fatalf("patch span 12: %d (%s)", code, out)
	}
	code, out = doReq(t, h, http.MethodPatch, "/api/v1/dashboards/"+d+"/widgets/"+id1, `{"span":1}`, hdr)
	if code != http.StatusOK || dashJSON(t, out)["span"] != float64(1) {
		t.Fatalf("patch span 1: %d (%s)", code, out)
	}

	// RowSpan edits: 8 (the ceiling) and 1 are both valid.
	code, out = doReq(t, h, http.MethodPatch, "/api/v1/dashboards/"+d+"/widgets/"+id1, `{"row_span":8}`, hdr)
	if code != http.StatusOK || dashJSON(t, out)["row_span"] != float64(8) {
		t.Fatalf("patch row_span 8: %d (%s)", code, out)
	}
	code, out = doReq(t, h, http.MethodPatch, "/api/v1/dashboards/"+d+"/widgets/"+id1, `{"row_span":1}`, hdr)
	if code != http.StatusOK || dashJSON(t, out)["row_span"] != float64(1) {
		t.Fatalf("patch row_span 1: %d (%s)", code, out)
	}

	// Delete: 204, widget_count back to 2; re-delete: 404.
	code, out = doReq(t, h, http.MethodDelete, "/api/v1/dashboards/"+d+"/widgets/"+id2, "", hdr)
	if code != http.StatusNoContent || out != "" {
		t.Fatalf("delete widget = %d %q, want 204 empty", code, out)
	}
	code, out = doReq(t, h, http.MethodGet, "/api/v1/dashboards", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("list dashboards: %d (%s)", code, out)
	}
	if err := json.Unmarshal([]byte(out), &drows); err != nil {
		t.Fatalf("list dashboards: %v (%s)", err, out)
	}
	if len(drows) != 1 || drows[0].WidgetCount != 2 {
		t.Fatalf("widget_count after delete = %+v, want 2", drows)
	}
	code, out = doReq(t, h, http.MethodDelete, "/api/v1/dashboards/"+d+"/widgets/"+id2, "", hdr)
	if code != http.StatusNotFound {
		t.Fatalf("re-delete widget: %d (%s)", code, out)
	}
	if c, m := apiErr(t, out); c != "not_found" || m != "dashboard widget not found" {
		t.Fatalf("re-delete envelope = %s %q, want not_found/dashboard widget not found", c, m)
	}

	// Create after a delete: sort_order = MAX+1 = 4 (gaps allowed).
	code, out = doReq(t, h, http.MethodPost, "/api/v1/dashboards/"+d+"/widgets", `{"name":"Late","kind":"explore"}`, hdr)
	if code != http.StatusCreated {
		t.Fatalf("create after delete: %d (%s)", code, out)
	}
	if w := dashJSON(t, out); w["sort_order"] != float64(4) {
		t.Fatalf("late widget sort_order = %v, want 4 (MAX 3 + 1)", w["sort_order"])
	}
}

// TestDashboardValidation pins the 400 surface: the name rules, the
// kind enum, the kind-dependent content caps (1024 explore / 16384
// markdown, counted in runes), the span and row_span ranges, the empty
// patch, the non-object body, and the combined error (all failures
// joined with "; ").
func TestDashboardValidation(t *testing.T) {
	dash, wid := newMemDashStores()
	h, key := newDashServer(t, dash, wid)
	tok := mintIdentityToken(t, key, "u-a", "u-a@example.com", "User A", "ua")
	hdr := authHdr(tok)

	code, out := doReq(t, h, http.MethodPost, "/api/v1/dashboards", `{"name":"D"}`, hdr)
	if code != http.StatusCreated {
		t.Fatalf("setup: %d (%s)", code, out)
	}
	d := idFrom(t, out)
	wpath := "/api/v1/dashboards/" + d + "/widgets"

	cases := []struct {
		name string
		path string
		body string
		want string
	}{
		{"empty dashboard name", "/api/v1/dashboards", `{"name":""}`, "name must not be empty"},
		{"blank dashboard name", "/api/v1/dashboards", `{"name":"   "}`, "name must not be empty"},
		{"long dashboard name", "/api/v1/dashboards", `{"name":"` + strings.Repeat("x", 256) + `"}`, "name must be at most 255 characters"},
		{"bad kind", wpath, `{"name":"W","kind":"chart"}`, `kind must be "explore" or "markdown"`},
		{"explore over content cap", wpath, `{"name":"W","kind":"explore","content":"` + strings.Repeat("a", 1025) + `"}`, "content must be at most 1024 characters"},
		{"markdown over content cap", wpath, `{"name":"W","kind":"markdown","content":"` + strings.Repeat("b", 16385) + `"}`, "content must be at most 16384 characters"},
		{"span zero", wpath, `{"name":"W","kind":"explore","span":0}`, "span must be between 1 and 12"},
		{"span thirteen", wpath, `{"name":"W","kind":"explore","span":13}`, "span must be between 1 and 12"},
		{"row_span zero", wpath, `{"name":"W","kind":"explore","row_span":0}`, "row_span must be between 1 and 8"},
		{"row_span nine", wpath, `{"name":"W","kind":"explore","row_span":9}`, "row_span must be between 1 and 8"},
		{"empty widget name", wpath, `{"name":"  ","kind":"explore"}`, "name must not be empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, out := doReq(t, h, http.MethodPost, tc.path, tc.body, hdr)
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", code, out)
			}
			if _, m := apiErr(t, out); !strings.Contains(m, tc.want) {
				t.Fatalf("message = %q, want it to contain %q", m, tc.want)
			}
		})
	}

	// Every validation runs before the store: the combined error joins
	// all failures with "; " (name, kind, span, row_span; content is
	// in-cap).
	code, out = doReq(t, h, http.MethodPost, wpath, `{"name":"  ","kind":"chart","span":13,"row_span":9}`, hdr)
	if code != http.StatusBadRequest {
		t.Fatalf("combined: status = %d (body %s)", code, out)
	}
	want := `name must not be empty; kind must be "explore" or "markdown"; span must be between 1 and 12; row_span must be between 1 and 8`
	if _, m := apiErr(t, out); m != want {
		t.Fatalf("combined message = %q, want %q", m, want)
	}

	// Empty patch: 400 before any store lookup (works on unknown ids).
	code, out = doReq(t, h, http.MethodPatch, "/api/v1/dashboards/"+d, `{}`, hdr)
	if code != http.StatusBadRequest {
		t.Fatalf("empty dashboard patch: %d (%s)", code, out)
	}
	if _, m := apiErr(t, out); m != "empty patch: provide at least one field" {
		t.Fatalf("empty dashboard patch message = %q", m)
	}
	code, out = doReq(t, h, http.MethodPatch, wpath+"/"+nextTestID(), `{}`, hdr)
	if code != http.StatusBadRequest {
		t.Fatalf("empty widget patch: %d (%s)", code, out)
	}
	if _, m := apiErr(t, out); m != "empty patch: provide at least one field" {
		t.Fatalf("empty widget patch message = %q", m)
	}

	// A non-object body: 400 with the decode detail.
	code, out = doReq(t, h, http.MethodPost, "/api/v1/dashboards", `[1]`, hdr)
	if code != http.StatusBadRequest {
		t.Fatalf("array body: %d (%s)", code, out)
	}
	if _, m := apiErr(t, out); !strings.HasPrefix(m, "body must be a JSON object:") {
		t.Fatalf("array body message = %q", m)
	}

	// The caps are inclusive: exactly 1024 / 16384 create fine.
	code, out = doReq(t, h, http.MethodPost, wpath,
		`{"name":"Edge","kind":"explore","content":"`+strings.Repeat("a", 1024)+`"}`, hdr)
	if code != http.StatusCreated {
		t.Fatalf("explore at cap: %d (%s)", code, out)
	}
	code, out = doReq(t, h, http.MethodPost, wpath,
		`{"name":"Edge md","kind":"markdown","content":"`+strings.Repeat("b", 16384)+`"}`, hdr)
	if code != http.StatusCreated {
		t.Fatalf("markdown at cap: %d (%s)", code, out)
	}
}

// TestDashboardOrder covers PUT widgets/order: the 204 reorder (the
// list comes back re-sorted with fresh sort_order values), [] on an
// empty dashboard, the body-shape 400, the non-uuid 400, the
// missing/duplicate "exactly once" 400, the 404 for a widget id from
// another dashboard, and the 404 for an unknown dashboard.
func TestDashboardOrder(t *testing.T) {
	dash, wid := newMemDashStores()
	h, key := newDashServer(t, dash, wid)
	tok := mintIdentityToken(t, key, "u-a", "u-a@example.com", "User A", "ua")
	hdr := authHdr(tok)

	createDash := func(name string) string {
		t.Helper()
		code, out := doReq(t, h, http.MethodPost, "/api/v1/dashboards", `{"name":"`+name+`"}`, hdr)
		if code != http.StatusCreated {
			t.Fatalf("create %s: %d (%s)", name, code, out)
		}
		return idFrom(t, out)
	}
	mk := func(dd, name, kind string) string {
		t.Helper()
		code, out := doReq(t, h, http.MethodPost, "/api/v1/dashboards/"+dd+"/widgets",
			`{"name":"`+name+`","kind":"`+kind+`"}`, hdr)
		if code != http.StatusCreated {
			t.Fatalf("create widget %s: %d (%s)", name, code, out)
		}
		return idFrom(t, out)
	}

	d := createDash("B")
	w1, w2, w3 := mk(d, "a", "explore"), mk(d, "b", "explore"), mk(d, "c", "markdown")

	// Reorder: 204, then the list comes back in the new order.
	code, out := doReq(t, h, http.MethodPut, "/api/v1/dashboards/"+d+"/widgets/order",
		`["`+w3+`","`+w1+`","`+w2+`"]`, hdr)
	if code != http.StatusNoContent || out != "" {
		t.Fatalf("order = %d %q, want 204 empty", code, out)
	}
	code, out = doReq(t, h, http.MethodGet, "/api/v1/dashboards/"+d+"/widgets", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("list: %d (%s)", code, out)
	}
	type widgetRow struct {
		ID        string `json:"id"`
		SortOrder int    `json:"sort_order"`
	}
	var rows []widgetRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("list: %v (%s)", err, out)
	}
	if len(rows) != 3 || rows[0].ID != w3 || rows[1].ID != w1 || rows[2].ID != w2 {
		t.Fatalf("list order = %+v, want [w3 w1 w2]", rows)
	}
	if rows[0].SortOrder != 1 || rows[1].SortOrder != 2 || rows[2].SortOrder != 3 {
		t.Errorf("sort orders = %d/%d/%d, want 1/2/3", rows[0].SortOrder, rows[1].SortOrder, rows[2].SortOrder)
	}

	// [] on an empty dashboard: 204.
	empty := createDash("Empty")
	code, out = doReq(t, h, http.MethodPut, "/api/v1/dashboards/"+empty+"/widgets/order", `[]`, hdr)
	if code != http.StatusNoContent {
		t.Fatalf("empty order: %d (%s)", code, out)
	}

	// A non-array body: 400 with the decode detail.
	code, out = doReq(t, h, http.MethodPut, "/api/v1/dashboards/"+d+"/widgets/order", `{"ids":[]}`, hdr)
	if code != http.StatusBadRequest {
		t.Fatalf("non-array: %d (%s)", code, out)
	}
	if _, m := apiErr(t, out); !strings.HasPrefix(m, "body must be a JSON array:") {
		t.Fatalf("non-array message = %q", m)
	}

	// A non-uuid element: 400.
	code, out = doReq(t, h, http.MethodPut, "/api/v1/dashboards/"+d+"/widgets/order",
		`["`+w1+`","nope"]`, hdr)
	if code != http.StatusBadRequest {
		t.Fatalf("non-uuid element: %d (%s)", code, out)
	}
	if _, m := apiErr(t, out); m != "order must contain only widget ids" {
		t.Fatalf("non-uuid element message = %q", m)
	}

	// A missing widget: 400 exactly-once.
	code, out = doReq(t, h, http.MethodPut, "/api/v1/dashboards/"+d+"/widgets/order",
		`["`+w1+`","`+w2+`"]`, hdr)
	if code != http.StatusBadRequest {
		t.Fatalf("missing widget: %d (%s)", code, out)
	}
	if _, m := apiErr(t, out); m != "order must list every widget exactly once" {
		t.Fatalf("missing widget message = %q", m)
	}

	// A duplicated widget: 400 exactly-once.
	code, out = doReq(t, h, http.MethodPut, "/api/v1/dashboards/"+d+"/widgets/order",
		`["`+w1+`","`+w1+`","`+w2+`"]`, hdr)
	if code != http.StatusBadRequest {
		t.Fatalf("duplicate widget: %d (%s)", code, out)
	}
	if _, m := apiErr(t, out); m != "order must list every widget exactly once" {
		t.Fatalf("duplicate widget message = %q", m)
	}

	// A widget id from another dashboard: 404 widget not found.
	d2 := createDash("Other")
	wo := mk(d2, "x", "explore")
	code, out = doReq(t, h, http.MethodPut, "/api/v1/dashboards/"+d+"/widgets/order",
		`["`+w1+`","`+w2+`","`+wo+`"]`, hdr)
	if code != http.StatusNotFound {
		t.Fatalf("foreign widget: %d (%s)", code, out)
	}
	if _, m := apiErr(t, out); m != "dashboard widget not found" {
		t.Fatalf("foreign widget message = %q", m)
	}

	// Unknown and malformed dashboard ids: 404 dashboard not found.
	code, out = doReq(t, h, http.MethodPut, "/api/v1/dashboards/"+nextTestID()+"/widgets/order", `[]`, hdr)
	if code != http.StatusNotFound {
		t.Fatalf("unknown dash order: %d (%s)", code, out)
	}
	if _, m := apiErr(t, out); m != "dashboard not found" {
		t.Fatalf("unknown dash order message = %q", m)
	}
	code, out = doReq(t, h, http.MethodPut, "/api/v1/dashboards/not-a-uuid/widgets/order", `[]`, hdr)
	if code != http.StatusNotFound {
		t.Fatalf("malformed dash order: %d (%s)", code, out)
	}
	if _, m := apiErr(t, out); m != "dashboard not found" {
		t.Fatalf("malformed dash order message = %q", m)
	}
}

// TestDashboardNotFoundAndScope pins the 404 semantics: unknown ids
// and other users' ids both read as "not found" (existence is not
// disclosed), widget ids resolve only under their own dashboard, and
// deleting a dashboard cascades to its widgets.
func TestDashboardNotFoundAndScope(t *testing.T) {
	dash, wid := newMemDashStores()
	h, key := newDashServer(t, dash, wid)
	tokA := mintIdentityToken(t, key, "u-a", "u-a@example.com", "User A", "ua")
	tokB := mintIdentityToken(t, key, "u-b", "u-b@example.com", "User B", "ub")
	hdrA, hdrB := authHdr(tokA), authHdr(tokB)

	// A: two dashboards, one widget each.
	code, out := doReq(t, h, http.MethodPost, "/api/v1/dashboards", `{"name":"A"}`, hdrA)
	if code != http.StatusCreated {
		t.Fatalf("create: %d (%s)", code, out)
	}
	d := idFrom(t, out)
	code, out = doReq(t, h, http.MethodPost, "/api/v1/dashboards/"+d+"/widgets", `{"name":"w1","kind":"explore"}`, hdrA)
	if code != http.StatusCreated {
		t.Fatalf("create widget 1: %d (%s)", code, out)
	}
	w1 := idFrom(t, out)
	code, out = doReq(t, h, http.MethodPost, "/api/v1/dashboards", `{"name":"A2"}`, hdrA)
	if code != http.StatusCreated {
		t.Fatalf("create 2: %d (%s)", code, out)
	}
	d2 := idFrom(t, out)
	code, out = doReq(t, h, http.MethodPost, "/api/v1/dashboards/"+d2+"/widgets", `{"name":"w2","kind":"explore"}`, hdrA)
	if code != http.StatusCreated {
		t.Fatalf("create widget 2: %d (%s)", code, out)
	}
	w2 := idFrom(t, out)

	notFound := func(op string, code int, out, wantMsg string) {
		if code != http.StatusNotFound {
			t.Fatalf("%s: status = %d, want 404 (body %s)", op, code, out)
		}
		if c, m := apiErr(t, out); c != "not_found" || m != wantMsg {
			t.Fatalf("%s: envelope = %s %q, want not_found/%q", op, c, m, wantMsg)
		}
	}

	// Unknown and malformed dashboard ids: 404 on every route.
	code, out = doReq(t, h, http.MethodGet, "/api/v1/dashboards/"+nextTestID()+"/widgets", "", hdrA)
	notFound("widgets list unknown dash", code, out, "dashboard not found")
	code, out = doReq(t, h, http.MethodGet, "/api/v1/dashboards/not-a-uuid/widgets", "", hdrA)
	notFound("widgets list malformed dash", code, out, "dashboard not found")
	code, out = doReq(t, h, http.MethodPatch, "/api/v1/dashboards/not-a-uuid", `{"name":"x"}`, hdrA)
	notFound("dashboard patch malformed", code, out, "dashboard not found")
	code, out = doReq(t, h, http.MethodDelete, "/api/v1/dashboards/"+nextTestID(), "", hdrA)
	notFound("dashboard delete unknown", code, out, "dashboard not found")
	code, out = doReq(t, h, http.MethodPost, "/api/v1/dashboards/"+nextTestID()+"/widgets", `{"name":"w","kind":"explore"}`, hdrA)
	notFound("widget create under unknown dash", code, out, "dashboard not found")

	// A widget id under another dashboard: 404 widget not found.
	code, out = doReq(t, h, http.MethodPatch, "/api/v1/dashboards/"+d2+"/widgets/"+w1, `{"name":"x"}`, hdrA)
	notFound("widget patch under other dash", code, out, "dashboard widget not found")
	code, out = doReq(t, h, http.MethodDelete, "/api/v1/dashboards/"+d2+"/widgets/"+w1, "", hdrA)
	notFound("widget delete under other dash", code, out, "dashboard widget not found")
	// A malformed widget id: the same 404.
	code, out = doReq(t, h, http.MethodPatch, "/api/v1/dashboards/"+d+"/widgets/not-a-uuid", `{"name":"x"}`, hdrA)
	notFound("widget patch malformed wid", code, out, "dashboard widget not found")

	// Cascade: deleting dashboard d takes w1; w2 (on d2) survives.
	code, out = doReq(t, h, http.MethodDelete, "/api/v1/dashboards/"+d, "", hdrA)
	if code != http.StatusNoContent {
		t.Fatalf("cascade delete: %d (%s)", code, out)
	}
	if n := len(wid.world.widgets); n != 1 {
		t.Fatalf("cascade left %d widget rows, want 1 (w2)", n)
	}

	// User B: A's rows are invisible (empty lists, 404s).
	code, out = doReq(t, h, http.MethodGet, "/api/v1/dashboards", "", hdrB)
	if code != http.StatusOK || strings.TrimSpace(out) != "[]" {
		t.Fatalf("B list = %d %q, want 200 []", code, out)
	}
	code, out = doReq(t, h, http.MethodPost, "/api/v1/dashboards/"+d2+"/widgets", `{"name":"w","kind":"explore"}`, hdrB)
	notFound("B widget create on A's dash", code, out, "dashboard not found")
	code, out = doReq(t, h, http.MethodPut, "/api/v1/dashboards/"+d2+"/widgets/order", `[]`, hdrB)
	notFound("B order on A's dash", code, out, "dashboard not found")
	code, out = doReq(t, h, http.MethodPatch, "/api/v1/dashboards/"+d2+"/widgets/"+w2, `{"name":"x"}`, hdrB)
	notFound("B patch of A's widget", code, out, "dashboard widget not found")
}

// TestDashboardAuthAndErrors pins the auth gate and the error mapping:
// missing/invalid credentials are 401, a nil store is 503 "database
// not configured", and a store failure is 500 "dashboard operation
// failed" on every store-backed operation.
func TestDashboardAuthAndErrors(t *testing.T) {
	// No credentials / an invalid token: 401.
	{
		dd, wd := newMemDashStores()
		h, _ := newDashServer(t, dd, wd)
		if code, _ := doReq(t, h, http.MethodGet, "/api/v1/dashboards", "", nil); code != http.StatusUnauthorized {
			t.Fatalf("no header: %d, want 401", code)
		}
		if code, _ := doReq(t, h, http.MethodGet, "/api/v1/dashboards", "", authHdr("bogus")); code != http.StatusUnauthorized {
			t.Fatalf("bad token: %d, want 401", code)
		}
	}

	// Nil stores: 503 database not configured.
	{
		h, key := newDashServer(t, nil, nil)
		hdr := authHdr(mintIdentityToken(t, key, "u-a", "u-a@example.com", "User A", "ua"))
		code, out := doReq(t, h, http.MethodGet, "/api/v1/dashboards", "", hdr)
		if code != http.StatusServiceUnavailable {
			t.Fatalf("nil store: %d (%s)", code, out)
		}
		if c, m := apiErr(t, out); c != "internal" || m != "database not configured" {
			t.Fatalf("nil store envelope = %s %q, want internal/database not configured", c, m)
		}
	}

	// Store failures: 500 dashboard operation failed.
	{
		dash, wid := newMemDashStores()
		h, key := newDashServer(t, dash, wid)
		hdr := authHdr(mintIdentityToken(t, key, "u-a", "u-a@example.com", "User A", "ua"))
		want500 := func(op string, code int, out string) {
			if code != http.StatusInternalServerError {
				t.Fatalf("%s: status = %d, want 500 (body %s)", op, code, out)
			}
			if c, m := apiErr(t, out); c != "internal" || m != "dashboard operation failed" {
				t.Fatalf("%s: envelope = %s %q, want internal/dashboard operation failed", op, c, m)
			}
		}
		dash.failList = errors.New("boom")
		code, out := doReq(t, h, http.MethodGet, "/api/v1/dashboards", "", hdr)
		want500("dashboards list", code, out)

		wid.failList = errors.New("boom")
		code, out = doReq(t, h, http.MethodGet, "/api/v1/dashboards/"+nextTestID()+"/widgets", "", hdr)
		want500("widgets list", code, out)

		wid.failCreate = errors.New("boom")
		code, out = doReq(t, h, http.MethodPost, "/api/v1/dashboards/"+nextTestID()+"/widgets", `{"name":"w","kind":"explore"}`, hdr)
		want500("widget create", code, out)

		dash.failDelete = errors.New("boom")
		code, out = doReq(t, h, http.MethodDelete, "/api/v1/dashboards/"+nextTestID(), "", hdr)
		want500("dashboard delete", code, out)

		// SetOrder is reached only after a successful List (the handler
		// re-checks membership), so clear the injections and use a real
		// dashboard.
		dash.failList, dash.failDelete = nil, nil
		wid.failList, wid.failCreate = nil, nil
		code, out = doReq(t, h, http.MethodPost, "/api/v1/dashboards", `{"name":"O"}`, hdr)
		if code != http.StatusCreated {
			t.Fatalf("order setup: %d (%s)", code, out)
		}
		od := idFrom(t, out)
		wid.failSetOrder = errors.New("boom")
		code, out = doReq(t, h, http.MethodPut, "/api/v1/dashboards/"+od+"/widgets/order", `[]`, hdr)
		want500("widget order", code, out)
	}
}
