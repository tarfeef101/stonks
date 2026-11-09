package fisync

import (
	"sort"
)

// Registry maps institution codes to their adapters. The sync job looks
// up each credential's institution code here; a code with no registered
// adapter is a per-credential notification, not a batch failure.
type Registry struct {
	byCode map[string]InstitutionAdapter
}

// NewRegistry returns a Registry over adapters. A duplicate Code() is a
// programmer error (adapters are wired once in cmd/worker at startup,
// before any tick can run) and panics.
func NewRegistry(adapters ...InstitutionAdapter) *Registry {
	byCode := make(map[string]InstitutionAdapter, len(adapters))
	for _, a := range adapters {
		code := a.Code()
		if _, dup := byCode[code]; dup {
			panic("fisync: duplicate adapter code " + code)
		}
		byCode[code] = a
	}
	return &Registry{byCode: byCode}
}

// Get returns the adapter registered for code, or ok=false when none is.
func (r *Registry) Get(code string) (InstitutionAdapter, bool) {
	a, ok := r.byCode[code]
	return a, ok
}

// Codes lists the registered institution codes, sorted.
func (r *Registry) Codes() []string {
	codes := make([]string, 0, len(r.byCode))
	for code := range r.byCode {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	return codes
}
