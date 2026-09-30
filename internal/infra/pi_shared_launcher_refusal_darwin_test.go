//go:build darwin

package infra

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Production call site: runtime runtime-launch -> RunSharedRuntimeLauncher.
// Each case crosses descriptor validation and reaches one specific later
// refusal so the typed failure sites are exercised independently.
func TestRunSharedRuntimeLauncherRefusesEveryPostDescriptorFailureAtProductionEntry(t *testing.T) {
	tests := []struct {
		name, site, wantCode, wantReason string
	}{
		{name: "profile resolution failure", site: "runtime_launch_identity_unresolvable", wantCode: "runtime_launch_identity_unresolvable"},
		{name: "runtime key mismatch", site: "runtime_launch_identity_mismatch", wantCode: "runtime_launch_identity_mismatch"},
		{name: "nonblocking setup failure", site: "runtime_launch_unauthorized", wantCode: "runtime_launch_unauthorized", wantReason: "deadline_unavailable"},
		{name: "authorization read failure", site: "runtime_launch_unauthorized", wantCode: "runtime_launch_unauthorized", wantReason: "broker_died_before_authorizing"},
		{name: "reader protocol failure", site: "protocol_violation", wantCode: "protocol_violation", wantReason: "frame_unparseable"},
		{name: "authorization decode failure", site: "protocol_violation", wantCode: "protocol_violation", wantReason: "frame_unparseable"},
		{name: "authorization value mismatch", site: "runtime_authorization_mismatch", wantCode: "runtime_authorization_mismatch"},
		{name: "exec failure", site: "runtime_start_failed", wantCode: "runtime_start_failed"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newSharedLauncherFixture(t)
			var run *sharedLauncherRun
			switch testCase.name {
			case "profile resolution failure":
				if err := os.Remove(sharedLauncherProjectConfigPath(fixture.project)); err != nil {
					t.Fatal(err)
				}
				run = startSharedLauncherRun(t, fixture, true)
			case "runtime key mismatch":
				wrongKey := fixture.resolved.RuntimeKey[:len(fixture.resolved.RuntimeKey)-1] + "0"
				if strings.HasSuffix(fixture.resolved.RuntimeKey, "0") {
					wrongKey = fixture.resolved.RuntimeKey[:len(fixture.resolved.RuntimeKey)-1] + "1"
				}
				run = startSharedLauncherRunWithEnvAndRuntimeKey(t, fixture, wrongKey)
			case "nonblocking setup failure":
				run = startSharedLauncherRunWithEnv(t, fixture, true, sharedSetNonblockFail+"=1")
				if err := run.writeEnd.Close(); err != nil {
					t.Fatal(err)
				}
				run.writeEnd = nil
			case "authorization read failure":
				run = startSharedLauncherRun(t, fixture, true)
				if err := run.writeEnd.Close(); err != nil {
					t.Fatal(err)
				}
				run.writeEnd = nil
			case "reader protocol failure":
				run = startSharedLauncherRun(t, fixture, true)
				valid := rawSharedLauncherFrame(t, validSharedLauncherFrame(fixture, run.command.Process.Pid))
				_ = run.sendAuthorization(append(valid, []byte("\n{\"second\":true}")...))
			case "authorization decode failure":
				run = startSharedLauncherRun(t, fixture, true)
				_ = run.sendAuthorization([]byte("{broken"))
			case "authorization value mismatch":
				run = startSharedLauncherRun(t, fixture, true)
				frame := validSharedLauncherFrame(fixture, run.command.Process.Pid)
				frame.RuntimeKey += "x"
				_ = run.sendAuthorization(rawSharedLauncherFrame(t, frame))
			case "exec failure":
				setSharedLauncherExecutable(t, &fixture, filepath.Join(fixture.project, "missing-engine"))
				run = startSharedLauncherRun(t, fixture, true)
				run.authorize(t, rawSharedLauncherFrame(t, validSharedLauncherFrame(fixture, run.command.Process.Pid)))
			default:
				t.Fatalf("unhandled refusal case %q", testCase.name)
			}
			if err := run.wait(t, 3*time.Second); err == nil {
				t.Fatalf("runtime runtime-launch admitted %s", testCase.name)
			}
			if run.writeEnd != nil {
				_ = run.writeEnd.Close()
				run.writeEnd = nil
			}
			refusal := sharedRuntimeErrorFromOutput(t, run.stderr.String())
			if refusal.Code != testCase.wantCode || refusal.Reason != testCase.wantReason || run.carriedTarget() {
				t.Fatalf("runtime runtime-launch refusal=%#v want code=%q reason=%q carried_target=%t", refusal, testCase.wantCode, testCase.wantReason, run.carriedTarget())
			}
			if _, err := os.Stat(fixture.marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("refused runtime runtime-launch executed target: %v", err)
			}
		})
	}
}

func sharedLauncherProjectConfigPath(project string) string {
	return filepath.Join(project, ".agents", ".configs", projectConfigFileName)
}

func setSharedLauncherExecutable(t *testing.T, fixture *sharedLauncherFixture, executable string) {
	t.Helper()
	path := sharedLauncherProjectConfigPath(fixture.project)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Replace(string(data), fixture.target, executable, 1)
	if body == string(data) {
		t.Fatal("fixture executable was not present in project configuration")
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveSharedProfile(fixture.project, fixture.home, fixture.cache, "profile")
	if err != nil {
		t.Fatalf("resolve profile with missing executable: %v", err)
	}
	fixture.resolved = resolved
	if err := CreateSharedRuntimeTree(resolved.Paths); err != nil {
		t.Fatalf("create runtime tree for missing executable: %v", err)
	}
}
