package infra

import "fmt"

const (
	projectConfigParseField  = "project_config"
	piPrimarySessionField    = "agents.pi.primary_session"
	piStandaloneSessionField = "agents.pi.standalone_session"
)

// ProjectConfigurationError retains the source and field while keeping the
// original error available to callers that classify failures.
type ProjectConfigurationError struct {
	Source string
	Field  string
	Err    error
}

var piThinkingLevels = map[string]bool{
	"off": true, "minimal": true, "low": true, "medium": true,
	"high": true, "xhigh": true, "max": true,
}

func (e *ProjectConfigurationError) Error() string {
	return fmt.Sprintf("%s: field %s: %v", e.Source, e.Field, e.Err)
}

func (e *ProjectConfigurationError) Unwrap() error { return e.Err }

func projectConfigFieldError(path, field string, err error) error {
	return &ProjectConfigurationError{Source: path, Field: field, Err: err}
}

func projectConfigTable(parent map[string]any, key, field string) (map[string]any, bool, error) {
	value, present := parent[key]
	if !present {
		return nil, false, nil
	}
	table, ok := value.(map[string]any)
	if !ok {
		return nil, false, fmt.Errorf("expected table for %s, got %T", field, value)
	}
	return table, true, nil
}
