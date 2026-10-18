//go:build darwin

package infra

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The child remains alive until its parent closes stdin, without binding sockets.
func TestSharedProcessInvocationHelper(t *testing.T) {
	if os.Getenv("CURATOR_PROCESS_INVOCATION_HELPER") != "1" {
		return
	}
	fmt.Fprintln(os.Stdout, "ready")
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

// Kernel executable identity agrees with the launched file for relative,
// absolute and PATH lookups, even when argv[0] names a different executable.
func TestSharedProcessExecutableInvocation(t *testing.T) {
	executable, identity, err := ownResolvedExecutableIdentity()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	link := filepath.Join(dir, "process-child")
	if err := os.Symlink(executable, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, invocation := range []string{"./process-child", link, "process-child"} {
		t.Run(invocation, func(t *testing.T) {
			command := exec.Command(invocation, "-test.run=^TestSharedProcessInvocationHelper$")
			command.Dir = dir
			command.Args[0] = "/bin/sleep"
			command.Env = append(os.Environ(), "CURATOR_PROCESS_INVOCATION_HELPER=1")
			input, err := command.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			output, err := command.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = input.Close(); _ = command.Wait() }()
			scanner := bufio.NewScanner(output)
			if !scanner.Scan() || scanner.Text() != "ready" {
				t.Fatalf("child did not become ready: %v", scanner.Err())
			}
			observation, err := inspectSharedProcess(command.Process.Pid)
			if err != nil {
				t.Fatal(err)
			}
			kernelPath, err := readDarwinProcessExecutablePath(command.Process.Pid)
			if err != nil || !filepath.IsAbs(kernelPath) {
				t.Fatalf("kernel path=%q err=%v", kernelPath, err)
			}
			actual, err := processExecIdentity(observation)
			if err != nil || actual != identity {
				t.Fatalf("executable identity=%#v want=%#v path=%q err=%v", actual, identity, observation.ExecPath, err)
			}
			other, err := fileIdentity("/bin/sleep")
			if err != nil {
				t.Fatal(err)
			}
			if actual == other {
				t.Fatal("argv[0] replaced the kernel executable identity")
			}
		})
	}
}
