//go:build darwin

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/relux-works/curator-inference-manager/internal/infra"
	"golang.org/x/sys/unix"
)

// Both commands select the explicit catalog/cache, round-trip manual quarantine,
// and refuse active brokers or missing engines without changing the ledger.
func TestTopLevelQuarantineSelectionAndRefusals(t *testing.T) {
	config, cache := operatorPathFixture(t, "/bin/echo")
	common := []string{"--engine", "selected", "--config", config, "--cache-root", cache, "--json"}
	for _, command := range []string{"quarantine", "unquarantine"} {
		var out, stderr bytes.Buffer
		code := run(append([]string{command}, common...), &out, &stderr)
		var ledger infra.SharedRuntimeRestartLedger
		if err := json.Unmarshal(out.Bytes(), &ledger); err != nil || code != 0 || stderr.Len() != 0 || ledger.ManualQuarantine != (command == "quarantine") {
			t.Fatalf("exit=%d out=%s err=%s decode=%v", code, &out, &stderr, err)
		}
	}
	// Compat keeps its existing default-catalog semantics and uses the same ledger.
	body, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(os.Getenv("HOME"), ".curator", "engines.toml"), body, 0600); err != nil {
		t.Fatal(err)
	}
	for _, alias := range []string{"qwen-infra", "model-harness"} {
		for _, command := range []string{"quarantine", "unquarantine"} {
			var out, stderr bytes.Buffer
			code := run([]string{"compat", alias, command, "--engine", "selected", "--cache-root", cache}, &out, &stderr)
			var ledger infra.SharedRuntimeRestartLedger
			if err := json.Unmarshal(out.Bytes(), &ledger); err != nil || code != 0 || stderr.Len() != 0 || ledger.ManualQuarantine != (command == "quarantine") {
				t.Fatalf("compat exit=%d out=%s err=%s decode=%v", code, &out, &stderr, err)
			}
		}
	}

	status, _ := runStatus(t, append([]string{"status"}, common...))
	before, err := os.ReadFile(status.Paths.RestartLedger)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(status.Paths.BrokerLock, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	for _, command := range []string{"quarantine", "unquarantine"} {
		for _, tc := range []struct{ engine, code string }{{"selected", "shared_runtime_active"}, {"missing", "engine_catalog_read_failed"}} {
			args := append(append([]string{command}, common...), "--engine", tc.engine)
			var out, stderr bytes.Buffer
			code := run(args, &out, &stderr)
			var refusal struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(stderr.Bytes(), &refusal); err != nil || code != 1 || out.Len() != 0 || refusal.Code != tc.code {
				t.Fatalf("exit=%d out=%s err=%s decode=%v", code, &out, &stderr, err)
			}
			after, err := os.ReadFile(status.Paths.RestartLedger)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("refusal changed ledger", err)
			}
		}
	}
}

// The JSON publishing boundary emits a typed refusal and zero stdout bytes for
// an invalid report, rather than publishing a document the consumer cannot read.
func TestStatusJSONPublicationRefusesInvalidReport(t *testing.T) {
	var out, stderr bytes.Buffer
	code := writeStatusJSON(&out, infra.SharedRuntimeStatus{}, "selected", &stderr)
	var refusal struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(stderr.Bytes(), &refusal); err != nil || code != 1 || out.Len() != 0 || refusal.Code != "status_contract_violation" {
		t.Fatalf("exit=%d out=%s err=%s decode=%v", code, &out, &stderr, err)
	}
}
