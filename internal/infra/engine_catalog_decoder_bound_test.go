package infra

import (
	"github.com/pelletier/go-toml/v2"
	"testing"
)

// The dependency initializes even an empty map. The defensive nil-map return
// in validateEngineCatalogExactKeys has no reachable rejected input with this
// decoder version. This test records the boundary; it does not certify that
// unreachable site's narrowing as killed.
func TestEngineCatalogDecoderInitializesMapAndRejectsMissingEngines(t *testing.T) {
	for _, body := range []string{"", "# empty document\n", "schema_version = 1\n", "schema_version = 1\nengines = 3\n"} {
		t.Run(body, func(t *testing.T) {
			var decoded map[string]any
			if err := toml.Unmarshal([]byte(body), &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded == nil {
				t.Fatal("dependency returned a nil map after successful decoding")
			}
			if err := validateEngineCatalogExactKeys([]byte(body)); err == nil {
				t.Fatal("missing or scalar engines admitted by production validator")
			}
		})
	}
}
