//go:build darwin

package main

import (
	"strings"
	"testing"
)

// Rev5 regressions for rework-brief-03 (Q1 required presence, Q2 producer
// bounds). Every case drives the production run(status --json) path over a
// real kernel-attested process and AF_UNIX peer; the broker replies are
// hostile test evidence, never a healthy-engine proof. Hostile cases must
// refuse with an empty readings set; valid-boundary controls must publish 17
// facts. The numeric boundaries below pin the producer's own bounds for this
// fixture (catalog entry with stress startup 1 and shutdown 30): linger at
// most 9223372036-30-2, broker timeout at least 1+30+30, restart backoff
// below the broker timeout, millisecond backoffs at most 60000, fatal
// markers per the catalog content rule.
const (
	reviewLingerTightMax     = int64(9223372004)
	reviewDurationMaxSeconds = int64(9223372036)
	reviewStaleMaxSeconds    = int64(4611686018)
	reviewBrokerTimeoutMin   = 61
	reviewDelayMaxMillis     = 60000
)

// Q1 panel reproducer (2zz0hl, 36h963, 177gne): each required zero-capable
// sharing member, removed and nulled, in hello and status, must refuse.
func TestReviewRequiredSharingMembersRefuse(t *testing.T) {
	env := outerEnvelopeFixture(t)
	code, stdout, stderr := env.execute(env.resources, nil, nil)
	requireOuterValid(t, code, stdout, stderr)
	for _, phase := range []string{"hello", "status"} {
		for _, field := range []string{"linger_seconds", "restart_limit", "restart_initial_backoff_seconds", "restart_max_backoff_seconds"} {
			for _, shape := range []string{"missing", "null"} {
				t.Run(phase+"/"+field+"/"+shape, func(t *testing.T) {
					mutate := func(reply map[string]any) {
						outerMutateSharing(t, reply, func(sharing map[string]any) {
							switch shape {
							case "missing":
								delete(sharing, field)
							default:
								sharing[field] = nil
							}
						})
					}
					var code int
					var stdout, stderr string
					if phase == "hello" {
						code, stdout, stderr = env.execute(env.resources, mutate, nil)
					} else {
						code, stdout, stderr = env.execute(env.resources, nil, mutate)
					}
					requireOuterRefusal(t, code, stdout, stderr, "protocol_violation")
				})
			}
		}
	}
}

// Q1 panel reproducer (2zz0hl): pressure eviction grace, removed and nulled,
// in hello and status, must refuse. Explicit zero stays valid.
func TestReviewRequiredPressureGraceRefuses(t *testing.T) {
	env := outerEnvelopeFixture(t)
	for _, phase := range []string{"hello", "status"} {
		for _, shape := range []string{"valid-zero", "missing", "null"} {
			t.Run(phase+"/"+shape, func(t *testing.T) {
				mutate := func(reply map[string]any) {
					outerMutateSharing(t, reply, func(sharing map[string]any) {
						sharing["resource_pressure_mode"] = "provider"
						pressure := outerProviderPressure()
						pressure["eviction_grace_seconds"] = 0
						switch shape {
						case "missing":
							delete(pressure, "eviction_grace_seconds")
						case "null":
							pressure["eviction_grace_seconds"] = nil
						}
						sharing["resource_pressure"] = pressure
					})
				}
				var code int
				var stdout, stderr string
				if phase == "hello" {
					code, stdout, stderr = env.execute(env.resources, mutate, nil)
				} else {
					code, stdout, stderr = env.execute(env.resources, nil, mutate)
				}
				if shape == "valid-zero" {
					requireOuterValid(t, code, stdout, stderr)
					return
				}
				requireOuterRefusal(t, code, stdout, stderr, "protocol_violation")
			})
		}
	}
}

// Q2 panel reproducer (36h963 overflow_linger) plus the exact producer linger
// boundary: MaxInt64 and anything above linger+shutdown+handoff refuse; the
// boundary value itself and zero stay valid.
func TestReviewSharingLingerBoundsRefuse(t *testing.T) {
	env := outerEnvelopeFixture(t)
	code, stdout, stderr := env.execute(env.resources, nil, nil)
	requireOuterValid(t, code, stdout, stderr)
	t.Run("overflow_maxint64", func(t *testing.T) {
		for _, phase := range []string{"hello", "status"} {
			mutate := func(reply map[string]any) {
				outerMutateSharing(t, reply, func(sharing map[string]any) {
					sharing["linger_seconds"] = int64(9223372036854775807)
				})
			}
			if phase == "hello" {
				code, stdout, stderr = env.execute(env.resources, mutate, nil)
			} else {
				code, stdout, stderr = env.execute(env.resources, nil, mutate)
			}
			requireOuterRefusal(t, code, stdout, stderr, "protocol_violation")
		}
	})
	for _, tc := range []struct {
		name   string
		linger int64
		valid  bool
	}{
		{"above_duration_max", reviewDurationMaxSeconds + 1, false},
		{"above_tight_max", reviewLingerTightMax + 1, false},
		{"at_tight_max", reviewLingerTightMax, true},
		{"negative", -1, false},
		{"zero", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mutate := func(reply map[string]any) {
				outerMutateSharing(t, reply, func(sharing map[string]any) {
					sharing["linger_seconds"] = tc.linger
				})
			}
			code, stdout, stderr := env.execute(env.resources, nil, mutate)
			if tc.valid {
				requireOuterValid(t, code, stdout, stderr)
			} else {
				requireOuterRefusal(t, code, stdout, stderr, "protocol_violation")
			}
		})
	}
}

// Q2 panel reproducer (36h963 invalid_fatal_output) plus the catalog marker
// boundaries: empty, oversize, NUL and over-count markers refuse; absent,
// full-count and full-size markers stay valid.
func TestReviewSharingFatalMarkersRefuse(t *testing.T) {
	env := outerEnvelopeFixture(t)
	code, stdout, stderr := env.execute(env.resources, nil, nil)
	requireOuterValid(t, code, stdout, stderr)
	t.Run("empty_marker", func(t *testing.T) {
		for _, phase := range []string{"hello", "status"} {
			mutate := func(reply map[string]any) {
				outerMutateSharing(t, reply, func(sharing map[string]any) {
					sharing["fatal_output_substrings"] = []string{""}
				})
			}
			if phase == "hello" {
				code, stdout, stderr = env.execute(env.resources, mutate, nil)
			} else {
				code, stdout, stderr = env.execute(env.resources, nil, mutate)
			}
			requireOuterRefusal(t, code, stdout, stderr, "protocol_violation")
		}
	})
	repeat := func(n int, marker string) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = marker
		}
		return out
	}
	for _, tc := range []struct {
		name    string
		markers []string
		absent  bool
		valid   bool
	}{
		{"seventeen_entries", repeat(17, "x"), false, false},
		{"oversize_marker", []string{strings.Repeat("x", 513)}, false, false},
		{"nul_marker", []string{"a\x00b"}, false, false},
		{"absent_markers", nil, true, true},
		{"sixteen_entries", repeat(16, "x"), false, true},
		{"max_bytes_marker", []string{strings.Repeat("x", 512)}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mutate := func(reply map[string]any) {
				outerMutateSharing(t, reply, func(sharing map[string]any) {
					if tc.absent {
						delete(sharing, "fatal_output_substrings")
					} else {
						sharing["fatal_output_substrings"] = tc.markers
					}
				})
			}
			code, stdout, stderr := env.execute(env.resources, nil, mutate)
			if tc.valid {
				requireOuterValid(t, code, stdout, stderr)
			} else {
				requireOuterRefusal(t, code, stdout, stderr, "protocol_violation")
			}
		})
	}
}

// Q2 sweep: every producer duration bound through the status path, hostile
// and boundary-valid shapes.
func TestReviewSharingDurationBoundsRefuse(t *testing.T) {
	env := outerEnvelopeFixture(t)
	code, stdout, stderr := env.execute(env.resources, nil, nil)
	requireOuterValid(t, code, stdout, stderr)
	set := func(field string, value any) func(map[string]any) {
		return func(reply map[string]any) {
			outerMutateSharing(t, reply, func(sharing map[string]any) {
				sharing[field] = value
			})
		}
	}
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
		valid  bool
	}{
		{"heartbeat_zero", set("heartbeat_interval_seconds", 0), false},
		{"heartbeat_above_max", set("heartbeat_interval_seconds", reviewDurationMaxSeconds+1), false},
		{"stale_above_max", set("lease_stale_seconds", reviewStaleMaxSeconds+1), false},
		{"stale_at_max", set("lease_stale_seconds", reviewStaleMaxSeconds), true},
		{"broker_timeout_zero", set("broker_start_timeout_seconds", 0), false},
		{"broker_timeout_above_max", set("broker_start_timeout_seconds", reviewDurationMaxSeconds+1), false},
		{"broker_timeout_at_max", set("broker_start_timeout_seconds", reviewDurationMaxSeconds), true},
		{"broker_timeout_below_minimum", set("broker_start_timeout_seconds", reviewBrokerTimeoutMin-1), false},
		{"broker_timeout_at_minimum", set("broker_start_timeout_seconds", reviewBrokerTimeoutMin), true},
		{"restart_backoff_at_timeout", set("restart_max_backoff_seconds", reviewBrokerTimeoutMin), false},
		{"restart_backoff_below_timeout", func(reply map[string]any) {
			outerMutateSharing(t, reply, func(sharing map[string]any) {
				sharing["restart_max_backoff_seconds"] = reviewBrokerTimeoutMin - 1
				sharing["restart_initial_backoff_seconds"] = 0
			})
		}, true},
		{"restart_max_above_max", set("restart_max_backoff_seconds", reviewDurationMaxSeconds+1), false},
		{"restart_initial_above_max", set("restart_initial_backoff_seconds", reviewDurationMaxSeconds+1), false},
		{"restart_initial_negative", set("restart_initial_backoff_seconds", -1), false},
		// At-max initial backoff still refuses: the backoff ordering gate
		// fires first (max 0 < initial max), which is also what entails the
		// initial upper bound.
		{"restart_initial_at_max", set("restart_initial_backoff_seconds", reviewDurationMaxSeconds), false},
		{"stable_above_max", set("stable_run_seconds", reviewDurationMaxSeconds+1), false},
		{"stable_at_max", set("stable_run_seconds", reviewDurationMaxSeconds), true},
		{"stable_zero", set("stable_run_seconds", 0), false},
		{"quarantine_above_max", set("quarantine_seconds", reviewDurationMaxSeconds+1), false},
		{"quarantine_at_max", set("quarantine_seconds", reviewDurationMaxSeconds), true},
		{"quarantine_zero", set("quarantine_seconds", 0), false},
		{"initial_millis_above_max", set("restart_initial_backoff_milliseconds", reviewDelayMaxMillis+1), false},
		{"initial_millis_at_max", set("restart_initial_backoff_milliseconds", reviewDelayMaxMillis), true},
		{"initial_millis_negative", set("restart_initial_backoff_milliseconds", -1), false},
		{"max_millis_above_max", set("restart_max_backoff_milliseconds", reviewDelayMaxMillis+1), false},
		{"max_millis_at_max", set("restart_max_backoff_milliseconds", reviewDelayMaxMillis), true},
		{"max_millis_negative", set("restart_max_backoff_milliseconds", -1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := env.execute(env.resources, nil, tc.mutate)
			if tc.valid {
				requireOuterValid(t, code, stdout, stderr)
			} else {
				requireOuterRefusal(t, code, stdout, stderr, "protocol_violation")
			}
		})
	}
}

// Q2 sweep: provider pressure grace upper bound and observation-path shape
// through the status path.
func TestReviewPressurePolicyBoundsRefuse(t *testing.T) {
	env := outerEnvelopeFixture(t)
	code, stdout, stderr := env.execute(env.resources, nil, nil)
	requireOuterValid(t, code, stdout, stderr)
	withPressure := func(change func(map[string]any)) func(map[string]any) {
		return func(reply map[string]any) {
			outerMutateSharing(t, reply, func(sharing map[string]any) {
				sharing["resource_pressure_mode"] = "provider"
				pressure := outerProviderPressure()
				change(pressure)
				sharing["resource_pressure"] = pressure
			})
		}
	}
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
		valid  bool
	}{
		{"grace_above_max", withPressure(func(p map[string]any) { p["eviction_grace_seconds"] = reviewDurationMaxSeconds + 1 }), false},
		{"grace_at_max", withPressure(func(p map[string]any) { p["eviction_grace_seconds"] = reviewDurationMaxSeconds }), true},
		{"grace_negative", withPressure(func(p map[string]any) { p["eviction_grace_seconds"] = -1 }), false},
		{"path_relative", withPressure(func(p map[string]any) { p["observation_path"] = "relative" }), false},
		{"path_query", withPressure(func(p map[string]any) { p["observation_path"] = "/observations?x=1" }), false},
		{"path_unclean", withPressure(func(p map[string]any) { p["observation_path"] = "/a/../b" }), false},
		{"path_empty", withPressure(func(p map[string]any) { p["observation_path"] = "" }), false},
		{"path_clean_absolute", withPressure(func(p map[string]any) { p["observation_path"] = "/observations/live" }), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := env.execute(env.resources, nil, tc.mutate)
			if tc.valid {
				requireOuterValid(t, code, stdout, stderr)
			} else {
				requireOuterRefusal(t, code, stdout, stderr, "protocol_violation")
			}
		})
	}
}
