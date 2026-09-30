//go:build !windows

package infra

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
)

type piProcessWait struct {
	done   chan struct{}
	err    error
	output *sharedRuntimeOutputState
	pid    int
}

var sharedRuntimeReadinessSignal = func(process *os.Process, signal os.Signal) error {
	return process.Signal(signal)
}
var sharedRuntimeProcessGroupSignal = syscall.Kill
var sharedRuntimeProcessGroupWaitGone = waitProcessGroupGone
var sharedRuntimeProcessDrain = waitForPiProcessDrain
var sharedRuntimeProcessNow = time.Now
var sharedRuntimeProcessSleep = time.Sleep

type sharedRuntimeOutputState struct {
	mu          sync.Mutex
	marker      string
	fatalOutput chan string
}

type sharedRuntimeOutputWriter struct {
	mu       sync.Mutex
	dst      io.Writer
	markers  []string
	carry    string
	carryMax int
	state    *sharedRuntimeOutputState
}

func newSharedRuntimeOutputWriter(dst io.Writer, markers []string) *sharedRuntimeOutputWriter {
	maximum := 0
	for _, marker := range markers {
		if len(marker) > maximum {
			maximum = len(marker)
		}
	}
	return &sharedRuntimeOutputWriter{
		dst: dst, markers: append([]string(nil), markers...), carryMax: maximum - 1,
		state: &sharedRuntimeOutputState{fatalOutput: make(chan string, 1)},
	}
}

func (writer *sharedRuntimeOutputWriter) Write(data []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	n, err := writer.dst.Write(data)
	if n > 0 && writer.carryMax >= 0 {
		window := writer.carry + string(data[:n])
		for _, marker := range writer.markers {
			if marker != "" && strings.Contains(window, marker) {
				writer.state.recordFatalOutput(marker)
				break
			}
		}
		if len(window) > writer.carryMax {
			writer.carry = window[len(window)-writer.carryMax:]
		} else {
			writer.carry = window
		}
	}
	return n, err
}

func (state *sharedRuntimeOutputState) recordFatalOutput(marker string) {
	state.mu.Lock()
	if state.marker != "" {
		state.mu.Unlock()
		return
	}
	state.marker = marker
	state.mu.Unlock()
	select {
	case state.fatalOutput <- marker:
	default:
	}
}

func (state *sharedRuntimeOutputState) firstFatalOutput() string {
	if state == nil {
		return ""
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.marker
}

func preflightPiListener(baseURL string) error {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" {
		return sharedRuntimeError("runtime_listener_check_failed", errors.New("invalid runtime endpoint"))
	}
	listener, err := net.Listen("tcp4", parsed.Host)
	if err != nil {
		var op *net.OpError
		if errors.As(err, &op) && errors.Is(op.Err, syscall.EADDRINUSE) {
			return sharedRuntimeError("runtime_listener_occupied", err)
		}
		return sharedRuntimeError("runtime_listener_check_failed", err)
	}
	return listener.Close()
}

func waitPiRuntimeReady(ctx context.Context, client *http.Client, endpoint, model string, child *os.Process, childWait *piProcessWait, timeout time.Duration) error {
	_, err := waitPiRuntimeReadyAndReportModel(ctx, client, endpoint, model, child, childWait, timeout)
	return err
}

// waitPiRuntimeReadyAndReportModel waits for an exact configured model id or,
// when a remote catalog entry has no local id, discovers the single non-empty
// id served by its forwarded /v1/models endpoint. The remote profile name is
// never used as a model-id substitute.
func waitPiRuntimeReadyAndReportModel(ctx context.Context, client *http.Client, endpoint, model string, child *os.Process, childWait *piProcessWait, timeout time.Duration) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if client == nil {
		client = &http.Client{
			Timeout:   time.Second,
			Transport: &http.Transport{Proxy: nil},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", sharedRuntimeError("pi_deadline_exceeded", ctx.Err())
		case <-childWait.done:
			return "", sharedRuntimeError("runtime_exited_early", fmt.Errorf("runtime exited before readiness: %v", childWait.err))
		case marker := <-fatalOutputChannel(childWait):
			return "", sharedRuntimeError("runtime_fatal_output", fmt.Errorf("configured fatal output marker matched (%d bytes)", len(marker)))
		case <-deadline.C:
			return "", sharedRuntimeError("runtime_readiness_timeout", errors.New("runtime readiness timed out"))
		case <-ticker.C:
			if err := sharedRuntimeReadinessSignal(child, syscall.Signal(0)); err != nil {
				return "", sharedRuntimeError("runtime_exited_early", err)
			}
			request, requestErr := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
			if requestErr != nil {
				return "", sharedRuntimeError("runtime_readiness_invalid", requestErr)
			}
			response, requestErr := client.Do(request)
			if requestErr != nil {
				if ctx.Err() != nil {
					return "", sharedRuntimeError("pi_deadline_exceeded", ctx.Err())
				}
				continue
			}
			body, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
			_ = response.Body.Close()
			if readErr != nil {
				return "", sharedRuntimeError("runtime_readiness_invalid", fmt.Errorf("invalid readiness response: status=%d read=%v", response.StatusCode, readErr))
			}
			if response.StatusCode == http.StatusServiceUnavailable {
				continue
			}
			if response.StatusCode != http.StatusOK {
				return "", sharedRuntimeError("runtime_readiness_invalid", fmt.Errorf("invalid readiness response: status=%d", response.StatusCode))
			}
			var payload struct {
				Object string `json:"object"`
				Data   *[]struct {
					ID string `json:"id"`
				} `json:"data"`
			}
			if err := json.Unmarshal(body, &payload); err != nil || payload.Object != "list" || payload.Data == nil {
				return "", sharedRuntimeError("runtime_readiness_invalid", errors.New("readiness response is not an OpenAI model list"))
			}
			if model != "" {
				for _, item := range *payload.Data {
					if item.ID == model {
						return item.ID, nil
					}
				}
				return "", sharedRuntimeError("runtime_model_unavailable", fmt.Errorf("exact model %q is absent", model))
			}
			var servedModel string
			for _, item := range *payload.Data {
				if item.ID == "" {
					continue
				}
				if servedModel != "" && servedModel != item.ID {
					return "", sharedRuntimeError("runtime_readiness_invalid", errors.New("readiness response has multiple served model ids and the catalog does not name one"))
				}
				servedModel = item.ID
			}
			if servedModel == "" {
				return "", sharedRuntimeError("runtime_model_unavailable", errors.New("readiness response contains no non-empty served model id"))
			}
			return servedModel, nil
		}
	}
}

func fatalOutputChannel(wait *piProcessWait) <-chan string {
	if wait == nil || wait.output == nil {
		return nil
	}
	return wait.output.fatalOutput
}

func terminateProcessGroup(pid int, wait *piProcessWait, timeout time.Duration) error {
	return terminateProcessGroupWithSignal(pid, wait, syscall.SIGTERM, timeout)
}

func terminateProcessGroupWithSignal(pid int, wait *piProcessWait, first syscall.Signal, timeout time.Duration) error {
	if err := sharedRuntimeProcessGroupSignal(-pid, syscall.Signal(0)); errors.Is(err, syscall.ESRCH) {
		return sharedRuntimeProcessDrain(wait, time.Second)
	}
	_ = sharedRuntimeProcessGroupSignal(-pid, first)
	deadline := sharedRuntimeProcessNow().Add(timeout)
	for sharedRuntimeProcessNow().Before(deadline) {
		if err := sharedRuntimeProcessGroupSignal(-pid, syscall.Signal(0)); errors.Is(err, syscall.ESRCH) {
			return sharedRuntimeProcessDrain(wait, time.Second)
		}
		sharedRuntimeProcessSleep(20 * time.Millisecond)
	}
	_ = sharedRuntimeProcessGroupSignal(-pid, syscall.SIGKILL)
	if err := sharedRuntimeProcessGroupWaitGone(pid, time.Second); err != nil {
		return err
	}
	if err := sharedRuntimeProcessDrain(wait, time.Second); err != nil {
		return err
	}
	return sharedRuntimeError("runtime_shutdown_timeout", errors.New("runtime group required SIGKILL"))
}

func waitForPiProcessDrain(wait *piProcessWait, timeout time.Duration) error {
	select {
	case <-wait.done:
		return nil
	case <-time.After(timeout):
		return sharedRuntimeError("runtime_shutdown_timeout", errors.New("runtime child could not be reaped and its output drained"))
	}
}

func waitProcessGroupGone(pid int, timeout time.Duration) error {
	deadline := sharedRuntimeProcessNow().Add(timeout)
	for {
		err := sharedRuntimeProcessGroupSignal(-pid, syscall.Signal(0))
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		if sharedRuntimeProcessNow().After(deadline) {
			return sharedRuntimeError("runtime_shutdown_timeout", errors.New("runtime process group still exists"))
		}
		sharedRuntimeProcessSleep(20 * time.Millisecond)
	}
}
