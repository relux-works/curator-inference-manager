package main

import (
	"bytes"
	"encoding/json"
	"reflect"
	"runtime/debug"
	"strings"
	"testing"
)

// Build metadata is transcribed in text and JSON; unavailable or empty metadata
// stays explicit, and unexpected arguments never produce a version success.
func TestVersion(t *testing.T) {
	old := readBuildInfo
	t.Cleanup(func() { readBuildInfo = old })
	for _, missing := range []bool{false, true} {
		readBuildInfo = func() (*debug.BuildInfo, bool) {
			if missing {
				return nil, false
			}
			return &debug.BuildInfo{GoVersion: "go1.25.5", Main: debug.Module{Version: "v1.2.3"}, Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "abc123"}, {Key: "vcs.time", Value: "2026-10-01T00:00:00Z"}, {Key: "vcs.modified", Value: "false"}}}, true
		}
		want := map[string]string{"module_version": "v1.2.3", "vcs_revision": "abc123", "vcs_time": "2026-10-01T00:00:00Z", "vcs_modified": "false", "go_version": "go1.25.5"}
		if missing {
			for key := range want {
				want[key] = "unknown"
			}
		}
		for _, jsonOutput := range []bool{false, true} {
			args := []string{"version"}
			if jsonOutput {
				args = append(args, "--json")
			}
			var out, stderr bytes.Buffer
			if code := run(args, &out, &stderr); code != 0 {
				t.Fatalf("%d: %s", code, &stderr)
			}
			if jsonOutput {
				var got map[string]string
				if err := json.Unmarshal(out.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("%v want %v", got, want)
				}
			} else {
				for _, value := range want {
					if !strings.Contains(out.String(), value) {
						t.Fatalf("missing %s: %s", value, &out)
					}
				}
			}
		}
	}
	readBuildInfo = func() (*debug.BuildInfo, bool) { return &debug.BuildInfo{}, true }
	var out, stderr bytes.Buffer
	if run([]string{"version", "--json"}, &out, &stderr) != 0 || strings.Count(out.String(), "unknown") != 5 {
		t.Fatal(&out, &stderr)
	}
	if run([]string{"version", "unexpected"}, &out, &stderr) != 2 {
		t.Fatal("accepted positional argument")
	}
}
