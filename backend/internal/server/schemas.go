// Import-schema endpoints (plan step 3.3): GET /api/v1/import-schemas
// over the repo.SchemaStore — the three global builtin presets plus the
// caller's custom schemas, builtins first (repo contract). Custom
// schemas are created/edited by the import flow (step 4.x); this step
// only serves the list.
package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"finance-tracker/internal/repo"
)

// SchemaStore is the persistence surface the import-schema handlers
// need. *repo.SchemaStore satisfies it (its Get/Create/Update/Delete
// are used by the import flow, step 4.x); route tests use an
// in-memory implementation.
type SchemaStore interface {
	List(ctx context.Context, userID string, dek []byte) ([]repo.Schema, error)
	EnsureBuiltins(ctx context.Context) (int, error)
}

// schemaOut is the wire row. Spec is the raw mapping document (a JSON
// object, {} for the untouched builtins) passed through as-is.
type schemaOut struct {
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	Kind    string          `json:"kind"`
	Builtin bool            `json:"builtin"`
	Spec    json.RawMessage `json:"spec"`
}

func toSchemaOut(sc repo.Schema) schemaOut {
	return schemaOut{
		ID:      sc.ID,
		Name:    sc.Name,
		Kind:    sc.Kind,
		Builtin: sc.Builtin,
		Spec:    sc.Spec,
	}
}

// schemasList serves GET /api/v1/import-schemas. It ensures the
// builtins before listing so a fresh database answers the presets on
// its first request (the repo's EnsureBuiltins is idempotent and
// serialized under an advisory lock).
func (h *handler) schemasList(w http.ResponseWriter, r *http.Request) {
	if h.schemas == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	if _, err := h.schemas.EnsureBuiltins(r.Context()); err != nil {
		slog.Error("ensure builtin import schemas failed", "error", err)
		writeAPIError(w, http.StatusInternalServerError, codeInternal, "builtin schemas could not be ensured")
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
