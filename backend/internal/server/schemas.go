// Import-schema endpoints (plan steps 3.3 + 4.5): GET /api/v1/import-
// schemas over the repo.SchemaStore — the caller's custom schemas
// (customs only since 0018; the global presets are the code builtins
// in internal/presets, served by GET /api/v1/imports/presets, D-029)
// — and POST /api/v1/import-schemas, the "save mapping as preset"
// write: the import flow stores a confirmed mapping document as a
// custom schema (the code builtins are read-only by construction,
// step 4.4).
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"finance-tracker/internal/repo"
)

// SchemaStore is the persistence surface the import-schema handlers
// need. *repo.SchemaStore satisfies it (its Get/Create/Update/Delete
// are used by the import flow, step 4.x); route tests use an
// in-memory implementation.
type SchemaStore interface {
	List(ctx context.Context, userID string, dek []byte) ([]repo.Schema, error)
	Get(ctx context.Context, userID, id string, dek []byte) (repo.Schema, error)
	Create(ctx context.Context, userID string, in repo.SchemaIn, dek []byte) (repo.Schema, error)
}

// schemaOut is the wire row (customs only since 0018 — no builtin
// flag: the code presets carry theirs on the /imports/presets
// response). Spec is the raw mapping document (a JSON object) passed
// through as-is.
type schemaOut struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Kind string          `json:"kind"`
	Spec json.RawMessage `json:"spec"`
}

func toSchemaOut(sc repo.Schema) schemaOut {
	return schemaOut{
		ID:   sc.ID,
		Name: sc.Name,
		Kind: sc.Kind,
		Spec: sc.Spec,
	}
}

// schemasList serves GET /api/v1/import-schemas: the caller's custom
// schemas (the repo scopes the query to the user).
func (h *handler) schemasList(w http.ResponseWriter, r *http.Request) {
	if h.schemas == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	schemas, err := h.schemas.List(r.Context(), c.row.ID, c.dek)
	if err != nil {
		slog.Error("list import schemas failed", "error", err)
		writeAPIError(w, http.StatusInternalServerError, codeInternal, "schema operation failed")
		return
	}
	out := make([]schemaOut, 0, len(schemas))
	for _, sc := range schemas {
		out = append(out, toSchemaOut(sc))
	}
	writeJSON(w, http.StatusOK, out)
}

// schemaCreateIn is the POST /api/v1/import-schemas body (step 4.5,
// "save mapping as preset"): a name, a kind, and the mapping document
// as the spec (it must carry the same kind).
type schemaCreateIn struct {
	Name string          `json:"name"`
	Kind string          `json:"kind"`
	Spec json.RawMessage `json:"spec"`
}

// schemasCreate serves POST /api/v1/import-schemas: it stores the
// mapping document the user confirmed (analyze -> map -> preview) as a
// custom preset the import page offers again next time. The spec must
// be a valid mapping document whose kind matches the row's kind (the
// server checks first so the answers are 400s; the repo re-checks).
// Builtins are read-only code (step 4.4); this only writes customs.
func (h *handler) schemasCreate(w http.ResponseWriter, r *http.Request) {
	if h.schemas == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	var in schemaCreateIn
	if err := decodeBody(r, &in); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "invalid request body")
		return
	}
	if err := checkName(in.Name); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
		return
	}
	kindOK := false
	for _, k := range repo.SchemaKinds {
		if in.Kind == k {
			kindOK = true
			break
		}
	}
	if !kindOK {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest,
			fmt.Sprintf("kind must be one of %s", strings.Join(repo.SchemaKinds, ", ")))
		return
	}
	var doc Mapping
	if err := json.Unmarshal(in.Spec, &doc); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "spec must be a valid mapping document")
		return
	}
	if err := doc.validate(); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
		return
	}
	if doc.Kind != in.Kind {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "kind must match the spec")
		return
	}
	sc, err := h.schemas.Create(r.Context(), c.row.ID, repo.SchemaIn{
		Name: in.Name,
		Kind: in.Kind,
		Spec: in.Spec,
	}, c.dek)
	if err != nil {
		slog.Error("create import schema failed", "error", err)
		writeAPIError(w, http.StatusInternalServerError, codeInternal, "schema operation failed")
		return
	}
	writeJSON(w, http.StatusCreated, toSchemaOut(sc))
}
