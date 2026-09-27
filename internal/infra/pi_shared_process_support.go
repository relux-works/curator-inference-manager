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
	"syscall"
	"time"
)

type piProcessWait struct {
	done chan struct{}
	err  error
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
			return sharedRuntimeError("pi_deadline_exceeded", ctx.Err())
		case <-childWait.done:
			return sharedRuntimeError("runtime_exited_early", fmt.Errorf("runtime exited before readiness: %v", childWait.err))
		case <-deadline.C:
			return sharedRuntimeError("runtime_readiness_timeout", errors.New("runtime readiness timed out"))
		case <-ticker.C:
			if err := child.Signal(syscall.Signal(0)); err != nil {
				return sharedRuntimeError("runtime_exited_early", err)
			}
			request, requestErr := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
			if requestErr != nil {
				return sharedRuntimeError("runtime_readiness_invalid", requestErr)
			}
			response, requestErr := client.Do(request)
			if requestErr != nil {
				if ctx.Err() != nil {
					return sharedRuntimeError("pi_deadline_exceeded", ctx.Err())
				}
				continue
			}
			body, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
			_ = response.Body.Close()
			if readErr != nil {
				return sharedRuntimeError("runtime_readiness_invalid", fmt.Errorf("invalid readiness response: status=%d read=%v", response.StatusCode, readErr))
			}
			if response.StatusCode == http.StatusServiceUnavailable {
				continue
			}
			if response.StatusCode != http.StatusOK {
				return sharedRuntimeError("runtime_readiness_invalid", fmt.Errorf("invalid readiness response: status=%d", response.StatusCode))
			}
			var payload struct {
				Object string `json:"object"`
				Data   *[]struct {
					ID string `json:"id"`
				} `json:"data"`
			}
			if err := json.Unmarshal(body, &payload); err != nil || payload.Object != "list" || payload.Data == nil {
				return sharedRuntimeError("runtime_readiness_invalid", errors.New("readiness response is not an OpenAI model list"))
			}
			for _, item := range *payload.Data {
				if item.ID == model {
					return nil
				}
			}
			return sharedRuntimeError("runtime_model_unavailable", fmt.Errorf("exact model %q is absent", model))
		}
	}
}

func terminateProcessGroup(pid int, wait *piProcessWait, timeout time.Duration) error {
	return terminateProcessGroupWithSignal(pid, wait, syscall.SIGTERM, timeout)
}

func terminateProcessGroupWithSignal(pid int, wait *piProcessWait, first syscall.Signal, timeout time.Duration) error {
	if err := syscall.Kill(-pid, syscall.Signal(0)); errors.Is(err, syscall.ESRCH) {
		return waitForPiProcessDrain(wait, time.Second)
	}
	_ = syscall.Kill(-pid, first)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(-pid, syscall.Signal(0)); errors.Is(err, syscall.ESRCH) {
			return waitForPiProcessDrain(wait, time.Second)
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	if err := waitProcessGroupGone(pid, time.Second); err != nil {
		return err
	}
	if err := waitForPiProcessDrain(wait, time.Second); err != nil {
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
	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Kill(-pid, syscall.Signal(0))
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		if time.Now().After(deadline) {
			return sharedRuntimeError("runtime_shutdown_timeout", errors.New("runtime process group still exists"))
		}
		time.Sleep(20 * time.Millisecond)
	}
}
