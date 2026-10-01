//go:build darwin

package main

import (
	"encoding/json"
	"testing"

	"github.com/relux-works/curator-inference-manager/internal/infra"
)

// Rev6 regressions for rework-brief-04 (frozen round-4 merge: R1 read
// failure vs absence, R2 null array elements, R3 present-empty marker
// list). Every case drives the production run(status --json) path over a
// real kernel-attested process and AF_UNIX peer; the broker replies are
// hostile test evidence, never a healthy-engine proof. Hostile cases must
// refuse with an empty stdout; valid-boundary controls must publish 17
// facts. Panel reproducers: ethqu7 (R1), 22rq5j (R2/R3), 2h153t (R3).

// R1 panel reproducer (ethqu7 F1): after a successful connect, peer
// identity check and hello write, a truncated hello frame (client reads
// EOF) or a hello stall (client 1s deadline fires) is a failed
// observation of an established peer. It must refuse atomically, never
// fall back to an absent-broker success with 17 facts.
func TestReviewHelloReadFailureMustRefuse(t *testing.T) {
	env := outerEnvelopeFixture(t)
	code, stdout, stderr := env.execute(env.resources, nil, nil)
	requireOuterValid(t, code, stdout, stderr)
	for _, tc := range []struct {
		mode     string
		wantCode string
	}{
		{"partial", "shared_runtime_unavailable"},
		{"timeout", "broker_unreachable"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			code, stdout, stderr := env.execute(env.resources, func(reply map[string]any) {
				reply["__fixture_hello_read_failure"] = tc.mode
			}, nil)
			var report infra.SharedRuntimeStatus
			_ = json.Unmarshal([]byte(stdout), &report)
			t.Logf("hello read mode=%s CLI exit=%d facts=%d broker=%s stderr=%q", tc.mode, code, len(report.Readings.Facts), report.Broker.State, stderr)
			requireOuterRefusal(t, code, stdout, stderr, tc.wantCode)
		})
	}
}

// R1 status-phase control: the same transport failures after a successful
// hello exchange already refused before the fix and must keep refusing.
func TestReviewStatusReadFailureRefuses(t *testing.T) {
	env := outerEnvelopeFixture(t)
	code, stdout, stderr := env.execute(env.resources, nil, nil)
	requireOuterValid(t, code, stdout, stderr)
	for _, mode := range []string{"partial", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			code, stdout, stderr := env.execute(env.resources, nil, func(reply map[string]any) {
				reply["__fixture_status_read_failure"] = mode
			})
			t.Logf("status read mode=%s CLI exit=%d output bytes=%d stderr=%q", mode, code, len(stdout), stderr)
			requireOuterRefusal(t, code, stdout, stderr, "broker_unreachable")
		})
	}
}

// R1 absence control: only a failure before a peer is established (no
// socket here) may fall back to the cold record-status success. This
// locks the pre-peer fallback the R1 fix preserves.
func TestReviewAbsentBrokerKeepsColdStatus(t *testing.T) {
	entry := readingsCatalogEntry()
	prepareCatalogCLIEntry(t, "readings", entry)
	report, _ := runStatus(t, []string{"status", "--engine", "readings", "--json"})
	if report.Broker.State != "absent" {
		t.Fatalf("cold broker state=%q want absent", report.Broker.State)
	}
	if len(report.Readings.Facts) != 17 {
		t.Fatalf("cold facts=%d want 17 profile facts", len(report.Readings.Facts))
	}
}

// R2 panel reproducer (22rq5j F1): only the entire nullable argv member
// may be null. A null element inside broker or runtime pre/post-exec
// argv, in hello or status, must refuse before readings are published.
// Whole-array null stays valid on the same paths.
func TestReviewWireNullArrayElementsRefuse(t *testing.T) {
	env := outerEnvelopeFixture(t)
	code, stdout, stderr := env.execute(env.resources, nil, nil)
	requireOuterValid(t, code, stdout, stderr)
	probes := []struct {
		name   string
		mutate func(t *testing.T, reply map[string]any)
	}{
		{"broker_argv_null_element", func(t *testing.T, reply map[string]any) {
			broker := outerReplyMap(t, reply["broker"])
			broker["argv"] = []any{nil}
			reply["broker"] = broker
		}},
		{"runtime_pre_exec_argv_null_element", func(t *testing.T, reply map[string]any) {
			runtime := outerReplyMap(t, reply["runtime"])
			pre := outerReplyMap(t, runtime["pre_exec"])
			pre["argv"] = []any{nil}
			runtime["pre_exec"] = pre
			reply["runtime"] = runtime
		}},
		{"runtime_post_exec_argv_null_element", func(t *testing.T, reply map[string]any) {
			runtime := outerReplyMap(t, reply["runtime"])
			post := outerReplyMap(t, runtime["post_exec"])
			post["argv"] = []any{nil}
			runtime["post_exec"] = post
			reply["runtime"] = runtime
		}},
	}
	for _, phase := range []string{"hello", "status"} {
		for _, probe := range probes {
			t.Run(phase+"/"+probe.name, func(t *testing.T) {
				var code int
				var stdout, stderr string
				if phase == "hello" {
					code, stdout, stderr = env.execute(env.resources, func(reply map[string]any) { probe.mutate(t, reply) }, nil)
				} else {
					code, stdout, stderr = env.execute(env.resources, nil, func(reply map[string]any) { probe.mutate(t, reply) })
				}
				var report infra.SharedRuntimeStatus
				_ = json.Unmarshal([]byte(stdout), &report)
				t.Logf("CLI exit=%d facts=%d stderr=%q", code, len(report.Readings.Facts), stderr)
				requireOuterRefusal(t, code, stdout, stderr, "protocol_violation")
			})
		}
	}
	// Whole-array null on the same nullable members stays valid: the
	// broker emits an explicit null for an empty diagnostic argv.
	for _, tc := range []struct {
		name   string
		phase  string
		mutate func(t *testing.T, reply map[string]any)
	}{
		{"broker_argv_whole_null", "hello", func(t *testing.T, reply map[string]any) {
			broker := outerReplyMap(t, reply["broker"])
			broker["argv"] = nil
			reply["broker"] = broker
		}},
		{"runtime_pre_exec_argv_whole_null", "status", func(t *testing.T, reply map[string]any) {
			runtime := outerReplyMap(t, reply["runtime"])
			pre := outerReplyMap(t, runtime["pre_exec"])
			pre["argv"] = nil
			runtime["pre_exec"] = pre
			reply["runtime"] = runtime
		}},
		{"runtime_post_exec_argv_whole_null", "status", func(t *testing.T, reply map[string]any) {
			runtime := outerReplyMap(t, reply["runtime"])
			post := outerReplyMap(t, runtime["post_exec"])
			post["argv"] = nil
			runtime["post_exec"] = post
			reply["runtime"] = runtime
		}},
	} {
		t.Run("valid/"+tc.name, func(t *testing.T) {
			var code int
			var stdout, stderr string
			if tc.phase == "hello" {
				code, stdout, stderr = env.execute(env.resources, func(reply map[string]any) { tc.mutate(t, reply) }, nil)
			} else {
				code, stdout, stderr = env.execute(env.resources, nil, func(reply map[string]any) { tc.mutate(t, reply) })
			}
			requireOuterValid(t, code, stdout, stderr)
		})
	}
}

// R3 panel reproducer (22rq5j F2, 2h153t): an explicitly present
// fatal_output_substrings list obeys the producer's 1..16 cardinality;
// a present empty list, in hello or status, must refuse. Legitimate
// omission (a TOML profile without supervision) and the 1/16
// boundaries stay valid.
func TestReviewSharingFatalMarkersMinimumRefuse(t *testing.T) {
	env := outerEnvelopeFixture(t)
	code, stdout, stderr := env.execute(env.resources, nil, nil)
	requireOuterValid(t, code, stdout, stderr)
	for _, phase := range []string{"hello", "status"} {
		t.Run(phase+"/present_empty_refuses", func(t *testing.T) {
			mutate := func(reply map[string]any) {
				outerMutateSharing(t, reply, func(sharing map[string]any) {
					sharing["fatal_output_substrings"] = []string{}
				})
			}
			var code int
			var stdout, stderr string
			if phase == "hello" {
				code, stdout, stderr = env.execute(env.resources, mutate, nil)
			} else {
				code, stdout, stderr = env.execute(env.resources, nil, mutate)
			}
			var report infra.SharedRuntimeStatus
			_ = json.Unmarshal([]byte(stdout), &report)
			t.Logf("CLI exit=%d facts=%d stderr=%q", code, len(report.Readings.Facts), stderr)
			requireOuterRefusal(t, code, stdout, stderr, "protocol_violation")
		})
	}
	// Omission and the producer boundaries stay valid. The unmutated
	// preamble already covers the single-marker fixture entry.
	sixteen := make([]string, 16)
	for i := range sixteen {
		sixteen[i] = "fatal"
	}
	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, reply map[string]any)
	}{
		{"omitted_markers_valid", func(t *testing.T, reply map[string]any) {
			outerMutateSharing(t, reply, func(sharing map[string]any) {
				delete(sharing, "fatal_output_substrings")
			})
		}},
		{"sixteen_markers_valid", func(t *testing.T, reply map[string]any) {
			outerMutateSharing(t, reply, func(sharing map[string]any) {
				sharing["fatal_output_substrings"] = sixteen
			})
		}},
	} {
		t.Run("valid/"+tc.name, func(t *testing.T) {
			code, stdout, stderr := env.execute(env.resources, nil, func(reply map[string]any) { tc.mutate(t, reply) })
			requireOuterValid(t, code, stdout, stderr)
		})
	}
}
