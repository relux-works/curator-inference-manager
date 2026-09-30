//go:build !darwin

package infra

import (
	"errors"
	"testing"
	"time"
)

func TestDecodeSharedRuntimeStatusRejectsMalformedPayloads(t *testing.T) {
	for _, testCase := range []struct {
		name string
		data string
		code string
	}{
		{name: "malformed JSON", data: `{"contract_version":`, code: "shared_runtime_status_undecodable"},
		{name: "contract version below minimum", data: `{"contract_version":0}`, code: "shared_runtime_status_unsupported_contract_version"},
		{name: "contract version above maximum", data: `{"contract_version":2}`, code: "shared_runtime_status_unsupported_contract_version"},
		{name: "malformed status body", data: `{"contract_version":1,"restart_count":"bad"}`, code: "shared_runtime_status_undecodable"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := DecodeSharedRuntimeStatus([]byte(testCase.data))
			var shared *SharedRuntimeError
			if !errors.As(err, &shared) || shared.Code != testCase.code {
				t.Fatalf("status decode returned %v, want code %q", err, testCase.code)
			}
		})
	}
}

func TestSetSharedRuntimeManualQuarantineRefusesUnsupportedPlatform(t *testing.T) {
	_, err := SetSharedRuntimeManualQuarantine(SharedRuntimeOperatorOptions{}, true)
	var shared *SharedRuntimeError
	if !errors.As(err, &shared) || shared.Code != "shared_runtime_platform_unsupported" {
		t.Fatalf("manual quarantine returned %v, want shared_runtime_platform_unsupported", err)
	}
}

func TestRunSharedRuntimeBrokerRefusesUnsupportedPlatform(t *testing.T) {
	err := RunSharedRuntimeBroker(SharedRuntimeBrokerOptions{})
	var shared *SharedRuntimeError
	if !errors.As(err, &shared) || shared.Code != "shared_runtime_platform_unsupported" {
		t.Fatalf("broker returned %v, want shared_runtime_platform_unsupported", err)
	}
}

func TestRunSharedRuntimeLauncherRefusesUnsupportedPlatform(t *testing.T) {
	err := RunSharedRuntimeLauncher(SharedRuntimeLauncherOptions{})
	var shared *SharedRuntimeError
	if !errors.As(err, &shared) || shared.Code != "shared_runtime_platform_unsupported" {
		t.Fatalf("launcher returned %v, want shared_runtime_platform_unsupported", err)
	}
}

func TestSharedRuntimeStatusReportRefusesUnsupportedPlatform(t *testing.T) {
	_, err := SharedRuntimeStatusReport(SharedRuntimeOperatorOptions{})
	var shared *SharedRuntimeError
	if !errors.As(err, &shared) || shared.Code != "shared_runtime_platform_unsupported" {
		t.Fatalf("status returned %v, want shared_runtime_platform_unsupported", err)
	}
}

func TestStopSharedRuntimeRefusesUnsupportedPlatform(t *testing.T) {
	_, err := StopSharedRuntime(SharedRuntimeOperatorOptions{}, false, time.Second)
	var shared *SharedRuntimeError
	if !errors.As(err, &shared) || shared.Code != "shared_runtime_platform_unsupported" {
		t.Fatalf("stop returned %v, want shared_runtime_platform_unsupported", err)
	}
}
