//go:build darwin

package infra

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

type readinessErrorBody struct{}

func (readinessErrorBody) Read([]byte) (int, error) {
	return 0, errors.New("readiness body read failure")
}
func (readinessErrorBody) Close() error { return nil }

func TestWaitPiRuntimeReadyRefusesMalformedAndUnavailableReadiness(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
		client   *http.Client
		model    string
		wantCode string
		wantText string
	}{
		{name: "request construction", endpoint: "://bad-url", wantCode: "runtime_readiness_invalid"},
		{name: "response body read", endpoint: "http://readiness.test/v1/models", client: responseClient(http.StatusOK, readinessErrorBody{}), wantCode: "runtime_readiness_invalid", wantText: "readiness body read failure"},
		{name: "unexpected response status", endpoint: "http://readiness.test/v1/models", client: responseClient(http.StatusInternalServerError, io.NopCloser(strings.NewReader(`{"object":"list","data":[{"id":"Wanted"}]}`))), model: "Wanted", wantCode: "runtime_readiness_invalid"},
		{name: "malformed model list", endpoint: "http://readiness.test/v1/models", client: responseClient(http.StatusOK, io.NopCloser(strings.NewReader(`{"object":"not-list","data":[]}`))), model: "Wanted", wantCode: "runtime_readiness_invalid"},
		{name: "model id has wrong JSON type", endpoint: "http://readiness.test/v1/models", client: responseClient(http.StatusOK, io.NopCloser(strings.NewReader(`{"object":"list","data":[{"id":123}]}`))), model: "Wanted", wantCode: "runtime_readiness_invalid"},
		{name: "invalid JSON after valid model list", endpoint: "http://readiness.test/v1/models", client: responseClient(http.StatusOK, io.NopCloser(strings.NewReader(`{"object":"list","data":[{"id":"Wanted"}]}x`))), model: "Wanted", wantCode: "runtime_readiness_invalid"},
		{name: "missing model array", endpoint: "http://readiness.test/v1/models", client: responseClient(http.StatusOK, io.NopCloser(strings.NewReader(`{"object":"list"}`))), model: "Wanted", wantCode: "runtime_readiness_invalid"},
		{name: "model absent from readiness list", endpoint: "http://readiness.test/v1/models", client: responseClient(http.StatusOK, io.NopCloser(strings.NewReader(`{"object":"list","data":[{"id":"Other"}]}`))), model: "Wanted", wantCode: "runtime_model_unavailable"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			child, wait := startReadinessChild(t)
			err := waitPiRuntimeReady(context.Background(), testCase.client, testCase.endpoint, testCase.model, child.Process, wait, time.Second)
			if sharedRuntimeErrorCode(err) != testCase.wantCode {
				t.Fatalf("readiness refusal code=%q want=%q err=%v", sharedRuntimeErrorCode(err), testCase.wantCode, err)
			}
			if testCase.wantText != "" && !strings.Contains(err.Error(), testCase.wantText) {
				t.Fatalf("readiness refusal did not preserve read failure detail: %v", err)
			}
		})
	}
}

func TestCheckSharedRuntimeModelRejectsMalformedResponses(t *testing.T) {
	cases := []struct {
		name     string
		client   *http.Client
		model    string
		wantCode string
		wantText string
	}{
		{name: "request failure", client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, os.ErrPermission })}, model: "Wanted", wantCode: "runtime_model_unavailable"},
		{name: "body read failure", client: responseClient(http.StatusOK, readinessErrorBody{}), model: "Wanted", wantCode: "runtime_readiness_invalid", wantText: "readiness body read failure"},
		{name: "non-ok status", client: responseClient(http.StatusInternalServerError, io.NopCloser(strings.NewReader(`{"object":"list","data":[{"id":"Wanted"}]}`))), model: "Wanted", wantCode: "runtime_readiness_invalid"},
		{name: "wrong object type with matching model", client: responseClient(http.StatusOK, io.NopCloser(strings.NewReader(`{"object":"list","object":123,"data":[{"id":"Wanted"}]}`))), model: "Wanted", wantCode: "runtime_readiness_invalid"},
		{name: "wrong object", client: responseClient(http.StatusOK, io.NopCloser(strings.NewReader(`{"object":"other","data":[]}`))), model: "Wanted", wantCode: "runtime_readiness_invalid"},
		{name: "missing data", client: responseClient(http.StatusOK, io.NopCloser(strings.NewReader(`{"object":"list"}`))), model: "Wanted", wantCode: "runtime_readiness_invalid"},
		{name: "missing model", client: responseClient(http.StatusOK, io.NopCloser(strings.NewReader(`{"object":"list","data":[{"id":"Other"}]}`))), model: "Wanted", wantCode: "runtime_model_unavailable"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := checkSharedRuntimeModel(testCase.client, "http://readiness.test/v1/models", testCase.model)
			if piErrorCode(err) != testCase.wantCode {
				t.Fatalf("model readiness refusal code=%q want=%q err=%v", piErrorCode(err), testCase.wantCode, err)
			}
			if testCase.wantText != "" && !strings.Contains(err.Error(), testCase.wantText) {
				t.Fatalf("model readiness refusal lost original failure detail %q: %v", testCase.wantText, err)
			}
		})
	}
}

func TestCheckSharedRuntimeModelDiscoversRemoteServedIDAndRefusesUnclassifiableLists(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		body     string
		wantCode string
	}{
		{name: "single served model is discovered", body: `{"object":"list","data":[{"id":"remote-served-model-id"}]}`},
		{name: "ambiguous served ids refuse", body: `{"object":"list","data":[{"id":"one"},{"id":"two"}]}`, wantCode: "runtime_readiness_invalid"},
		{name: "no served id refuses", body: `{"object":"list","data":[{"id":""}]}`, wantCode: "runtime_model_unavailable"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			client := responseClient(http.StatusOK, io.NopCloser(strings.NewReader(testCase.body)))
			err := checkSharedRuntimeModel(client, "http://readiness.test/v1/models", "")
			if got := piErrorCode(err); got != testCase.wantCode {
				t.Fatalf("unknown-model client attestation code=%q want=%q err=%v", got, testCase.wantCode, err)
			}
		})
	}
}

func TestWaitPiRuntimeReadyRefusesExitedProcessSignalAtProductionEntry(t *testing.T) {
	previous := sharedRuntimeReadinessSignal
	sharedRuntimeReadinessSignal = func(*os.Process, os.Signal) error { return os.ErrProcessDone }
	t.Cleanup(func() { sharedRuntimeReadinessSignal = previous })
	wait := &piProcessWait{done: make(chan struct{})}
	err := waitPiRuntimeReady(context.Background(), nil, "http://127.0.0.1:1/v1/models", "Wanted", &os.Process{Pid: 1}, wait, 150*time.Millisecond)
	if sharedRuntimeErrorCode(err) != "runtime_exited_early" || !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("exited child signal refusal code=%q err=%v", sharedRuntimeErrorCode(err), err)
	}
}

func TestWaitPiRuntimeReadyRefusesStartupReadinessTimeout(t *testing.T) {
	child, wait := startReadinessChild(t)
	err := waitPiRuntimeReady(context.Background(), nil, "http://127.0.0.1:1/v1/models", "Wanted", child.Process, wait, 20*time.Millisecond)
	if sharedRuntimeErrorCode(err) != "runtime_readiness_timeout" {
		t.Fatalf("startup timeout refusal code=%q want=runtime_readiness_timeout err=%v", sharedRuntimeErrorCode(err), err)
	}
}

func TestWaitPiRuntimeReadyRefusesCancelledExitedAndFatalStates(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		wantCode string
		prepare  func() (context.Context, *piProcessWait)
	}{
		{
			name: "cancelled", wantCode: "pi_deadline_exceeded",
			prepare: func() (context.Context, *piProcessWait) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, &piProcessWait{done: make(chan struct{})}
			},
		},
		{
			name: "child exited early", wantCode: "runtime_exited_early",
			prepare: func() (context.Context, *piProcessWait) {
				wait := &piProcessWait{done: make(chan struct{})}
				close(wait.done)
				return context.Background(), wait
			},
		},
		{
			name: "fatal output", wantCode: "runtime_fatal_output",
			prepare: func() (context.Context, *piProcessWait) {
				output := newSharedRuntimeOutputWriter(io.Discard, []string{"fatal-marker"})
				_, _ = output.Write([]byte("fatal-marker"))
				return context.Background(), &piProcessWait{done: make(chan struct{}), output: output.state}
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx, wait := testCase.prepare()
			err := waitPiRuntimeReady(ctx, nil, "http://127.0.0.1:1/v1/models", "Wanted", &os.Process{Pid: 1}, wait, time.Second)
			if sharedRuntimeErrorCode(err) != testCase.wantCode {
				t.Fatalf("readiness refusal code=%q want=%q err=%v", sharedRuntimeErrorCode(err), testCase.wantCode, err)
			}
		})
	}
}

func responseClient(status int, body io.ReadCloser) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: status, Body: body, Header: make(http.Header), Request: request}, nil
	})}
}

func startReadinessChild(t *testing.T) (*exec.Cmd, *piProcessWait) {
	t.Helper()
	child := exec.Command("/bin/sleep", "30")
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	wait := waitForPiProcessAndClose(child, io.NopCloser(strings.NewReader("")))
	t.Cleanup(func() {
		if child.ProcessState == nil {
			_ = terminateProcessGroup(child.Process.Pid, wait, 50*time.Millisecond)
		}
	})
	return child, wait
}
