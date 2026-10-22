//go:build darwin

package infra

import (
	"bufio"
	"context"
	"debug/macho"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Missing either hash refuses; a nonzero matching pair is a positive control.
func TestSharedCodeHashesFailClosed(t *testing.T) {
	for _, test := range []struct {
		name       string
		self, peer sharedCodeHash
		code       string
	}{
		{"same", sharedCodeHash{1}, sharedCodeHash{1}, ""},
		{"different", sharedCodeHash{1}, sharedCodeHash{2}, "broker_executable_identity_mismatch"},
		{"unsigned peer", sharedCodeHash{1}, sharedCodeHash{}, "broker_identity_unavailable"},
		{"unsigned self", sharedCodeHash{}, sharedCodeHash{1}, "broker_identity_unavailable"},
		{"both unsigned", sharedCodeHash{}, sharedCodeHash{}, "broker_identity_unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if code := sharedRuntimeErrorCode(validateSharedCodeHashes(test.self, test.peer)); code != test.code {
				t.Fatalf("code=%q want=%q", code, test.code)
			}
		})
	}
}

// Token/PID disagreement refuses before querying code; pidversion reaches csops unchanged.
func TestSharedCodeIdentityAuditTokenBinding(t *testing.T) {
	token := sharedAuditToken{5: 123, 7: 456}
	calls := 0
	own := func() (sharedCodeHash, error) { return sharedCodeHash{1}, nil }
	peer := func(pid int, got *sharedAuditToken) (sharedCodeHash, error) {
		calls++
		if pid != 123 || *got != token {
			t.Fatal("audit token was changed")
		}
		return sharedCodeHash{1}, nil
	}
	if err := sharedTokenCodeIdentity(123, token, own, peer); err != nil {
		t.Fatal(err)
	}
	if code := sharedRuntimeErrorCode(sharedTokenCodeIdentity(124, token, own, peer)); code != "broker_identity_mismatch" || calls != 1 {
		t.Fatalf("code=%q calls=%d", code, calls)
	}
	failure := func() (sharedCodeHash, error) { return sharedCodeHash{1}, os.ErrPermission }
	if code := sharedRuntimeErrorCode(sharedTokenCodeIdentity(123, token, failure, peer)); code != "broker_identity_unavailable" || calls != 1 {
		t.Fatalf("self failure code=%q calls=%d", code, calls)
	}
	peerFailure := func(int, *sharedAuditToken) (sharedCodeHash, error) { return sharedCodeHash{1}, os.ErrPermission }
	if code := sharedRuntimeErrorCode(sharedTokenCodeIdentity(123, token, own, peerFailure)); code != "broker_identity_unavailable" {
		t.Fatalf("peer failure code=%q", code)
	}
}

// Real csops agrees with the file's Go-linked CodeDirectory; stale pidversion refuses.
func TestSharedCodeIdentityKernelAndFile(t *testing.T) {
	flags, statusErr := sharedKernelCodeStatus(os.Getpid(), nil)
	if statusErr != nil || flags&sharedCSValid == 0 {
		t.Fatalf("normal test binary must have CS_VALID: flags=%#x err=%v", flags, statusErr)
	}
	hash, err := sharedOwnCodeHash()
	if err != nil {
		t.Fatalf("signed test executable required: %v", err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	fileHash, err := sharedFileCodeHash(executable, runtime.GOARCH)
	if err != nil || hash != fileHash {
		t.Fatalf("kernel=%x file=%x err=%v", hash, fileHash, err)
	}
	observation, err := inspectSharedProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := sharedObservedCodeIdentity(observation); err != nil {
		t.Fatal(err)
	}
	observation.StartTime.Seconds++
	if code := sharedRuntimeErrorCode(sharedObservedCodeIdentity(observation)); code != "broker_identity_unavailable" {
		t.Fatalf("incarnation mismatch code=%q", code)
	}
}

// The kernel accepts the current pidversion and rejects a stale token with ESRCH.
func TestSharedCodeIdentityKernelRejectsStaleAuditToken(t *testing.T) {
	var info [56]byte
	count, _, errno := unix.Syscall6(unix.SYS_PROC_INFO, 2, uintptr(os.Getpid()), 17, 0, uintptr(unsafe.Pointer(&info[0])), uintptr(len(info)))
	if errno != 0 || count != uintptr(len(info)) {
		t.Fatalf("pidversion query: count=%d errno=%v", count, errno)
	}
	token := sharedAuditToken{5: uint32(os.Getpid()), 7: binary.NativeEndian.Uint32(info[32:36])}
	hash, err := sharedKernelCodeHash(os.Getpid(), &token)
	if err != nil || hash == (sharedCodeHash{}) {
		t.Fatalf("current token: hash=%x err=%v", hash, err)
	}
	token[7]++
	if _, err := sharedKernelCodeHash(os.Getpid(), &token); !errors.Is(err, unix.ESRCH) {
		t.Fatalf("stale token: want ESRCH, got %v", err)
	}
	token[5]++
	if _, err := sharedKernelCodeHash(os.Getpid(), &token); err == nil {
		t.Fatal("mismatched token PID accepted")
	}
}

func buildSharedCodeFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	source := filepath.Join(dir, "main.go")
	binary := filepath.Join(dir, "helper")
	// A second executable supports a real socket handshake and deliberately has different code.
	const program = `package main
import("bufio";"encoding/json";"fmt";"net";"os")
func main(){fmt.Println("ready");bufio.NewReader(os.Stdin).ReadString('\n');c,e:=net.Dial("unix",os.Getenv("CODE_SOCKET"));if e!=nil{panic(e)};defer c.Close();var h map[string]any;json.Unmarshal([]byte(os.Getenv("CODE_HELLO")),&h);h["client_pid"]=os.Getpid();json.NewEncoder(c).Encode(h);bufio.NewReader(c).ReadString('\n')}
`
	if err := os.WriteFile(source, []byte(program), 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("go", "build", "-o", binary, source)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %v\n%s", err, output)
	}
	return binary
}

// A real built Mach-O agrees with codesign's independent CDHash display.
func TestSharedCodeDirectoryMatchesCodesign(t *testing.T) {
	if _, err := exec.LookPath("codesign"); err != nil {
		t.Skip("codesign unavailable; independent CDHash comparison requires Darwin signing tools")
	}
	executable := buildSharedCodeFixture(t)
	if runtime.GOARCH == "amd64" {
		if out, err := exec.Command("codesign", "--force", "--sign", "-", executable).CombinedOutput(); err != nil {
			t.Fatalf("ad-hoc sign fixture: %v %s", err, out)
		}
	}
	hash, err := sharedFileCodeHash(executable, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command("codesign", "-dvvv", executable).CombinedOutput()
	if err != nil {
		t.Fatalf("codesign: %v %s", err, output)
	}
	match := regexp.MustCompile(`(?m)^CDHash=([a-fA-F0-9]{40})\s*$`).FindSubmatch(output)
	if len(match) != 2 || !strings.EqualFold(hex.EncodeToString(hash[:]), string(match[1])) {
		t.Fatalf("hash=%x codesign=%s", hash, output)
	}
	foreign := "amd64"
	if runtime.GOARCH == foreign {
		foreign = "arm64"
	}
	if _, err := sharedFileCodeHash(executable, foreign); err == nil {
		t.Fatal("foreign architecture accepted")
	}
	data, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	image, err := macho.Open(executable)
	if err != nil {
		t.Fatal(err)
	}
	defer image.Close()
	fat := make([]byte, 4096+len(data))
	put := binary.BigEndian.PutUint32
	put(fat[:4], 0xcafebabe)
	put(fat[4:8], 1)
	put(fat[8:12], uint32(image.Cpu))
	put(fat[12:16], image.SubCpu)
	put(fat[16:20], 4096)
	put(fat[20:24], uint32(len(data)))
	put(fat[24:28], 12)
	copy(fat[4096:], data)
	fatPath := filepath.Join(t.TempDir(), "universal")
	if err := os.WriteFile(fatPath, fat, 0600); err != nil {
		t.Fatal(err)
	}
	if actual, err := sharedFileCodeHash(fatPath, runtime.GOARCH); err != nil || actual != hash {
		t.Fatalf("fat hash=%x want=%x err=%v", actual, hash, err)
	}
	if _, err := sharedFileCodeHash(fatPath, foreign); err == nil {
		t.Fatal("foreign fat architecture accepted")
	}
	if err := os.WriteFile(fatPath, fat[:len(fat)-1], 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := sharedFileCodeHash(fatPath, runtime.GOARCH); err == nil {
		t.Fatal("truncated fat slice accepted")
	}
	for _, size := range []int{0, 4, 32, len(data) - 1} {
		truncated := filepath.Join(t.TempDir(), "truncated")
		if err := os.WriteFile(truncated, data[:size], 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := sharedFileCodeHash(truncated, runtime.GOARCH); err == nil {
			t.Fatalf("truncated length %d accepted", size)
		}
	}
}

// The child blocks before connect so the parent can replace its executable path.
func TestSharedCodeIdentityChild(t *testing.T) {
	if os.Getenv("CODE_CHILD") != "1" {
		return
	}
	fmt.Println("ready")
	if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
		os.Exit(2)
	}
	connection, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: os.Getenv("CODE_SOCKET"), Net: "unix"})
	if err != nil {
		os.Exit(3)
	}
	defer connection.Close()
	var hello sharedWireMessage
	if err := json.Unmarshal([]byte(os.Getenv("CODE_HELLO")), &hello); err != nil {
		os.Exit(4)
	}
	hello.ClientPID = os.Getpid()
	if err := writeSharedWireMessage(connection, hello); err != nil {
		os.Exit(5)
	}
	_, _ = bufio.NewReader(connection).ReadString('\n')
	os.Exit(0)
}

// Real broker admission follows running code through copy/rename replacement,
// rejects a different signed image at the original path, and cross-checks hello PID.
func TestSharedCodeIdentitySocketAdmissionAndReplacement(t *testing.T) {
	own, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	other := buildSharedCodeFixture(t)
	if runtime.GOARCH == "amd64" {
		if out, err := exec.Command("codesign", "--force", "--sign", "-", other).CombinedOutput(); err != nil {
			t.Fatalf("sign: %v %s", err, out)
		}
	}
	for _, test := range []struct {
		name                         string
		different, replace, badHello bool
		code                         string
	}{
		{name: "same binary"},
		{name: "same code copy replaced by different code", replace: true},
		{name: "different signed code at expected path", different: true, code: "broker_executable_identity_mismatch"},
		{name: "different running code replaced by expected code", different: true, replace: true, code: "broker_executable_identity_mismatch"},
		{name: "hello pid mismatch", badHello: true, code: "broker_identity_mismatch"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newSharedBrokerAdmissionFixture(t)
			fixture.server.admission = sharedBrokerAdmissionSystem
			dir := t.TempDir()
			socketDir, err := os.MkdirTemp(os.TempDir(), "code-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
			socket := filepath.Join(socketDir, "peer.sock")
			listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			_ = listener.SetDeadline(time.Now().Add(15 * time.Second))
			path := filepath.Join(dir, "peer")
			source := own
			if test.different {
				source = other
			}
			copyFile := func(from, to string) {
				t.Helper()
				data, err := os.ReadFile(from)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(to, data, 0700); err != nil {
					t.Fatal(err)
				}
			}
			copyFile(source, path)
			hello, err := json.Marshal(fixture.hello)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, path, "-test.run=^TestSharedCodeIdentityChild$")
			command.Env = append(os.Environ(), "CODE_CHILD=1", "CODE_SOCKET="+socket, "CODE_HELLO="+string(hello))
			input, err := command.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			output, err := command.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			command.Stderr = os.Stderr
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = input.Close(); cancel(); _ = command.Wait() }()
			if line, err := bufio.NewReader(output).ReadString('\n'); err != nil || line != "ready\n" {
				t.Fatalf("child readiness=%q err=%v", line, err)
			}
			if test.replace {
				replacement := own
				if !test.different {
					replacement = other
				}
				copyFile(replacement, path+".next")
				if err := os.Rename(path+".next", path); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := io.WriteString(input, "connect\n"); err != nil {
				t.Fatal(err)
			}
			connection, err := listener.AcceptUnix()
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
			message, err := readSharedWireMessage(bufio.NewReader(connection))
			if err != nil {
				t.Fatal(err)
			}
			if test.badHello {
				message.ClientPID++
			}
			_, _, err = fixture.server.attestClient(connection, message)
			if code := sharedRuntimeErrorCode(err); code != test.code {
				t.Fatalf("code=%q want=%q err=%v", code, test.code, err)
			}
			_ = writeSharedWireMessage(connection, sharedWireMessage{Type: "done"})
		})
	}
}

// Error-free zero hashes and errors carrying a matching hash must both refuse.
func TestSharedCodeIdentityMissingKernelHash(t *testing.T) {
	token := sharedAuditToken{5: 1, 7: 2}
	valid := func() (sharedCodeHash, error) { return sharedCodeHash{1}, nil }
	for _, err := range []error{nil, os.ErrPermission} {
		own := func() (sharedCodeHash, error) { return sharedCodeHash{}, err }
		peer := func(int, *sharedAuditToken) (sharedCodeHash, error) { return sharedCodeHash{}, err }
		if code := sharedRuntimeErrorCode(sharedTokenCodeIdentity(1, token, own, peer)); code != "broker_identity_unavailable" {
			t.Fatalf("self code=%q", code)
		}
		if code := sharedRuntimeErrorCode(sharedTokenCodeIdentity(1, token, valid, peer)); code != "broker_identity_unavailable" {
			t.Fatalf("peer code=%q", code)
		}
	}
	if !errors.Is(sharedPeerIdentityRefusal(os.ErrPermission), os.ErrPermission) {
		t.Fatal("lost cause")
	}
}

// A real socketpair exercises LOCAL_PEERTOKEN without a filesystem bind;
// the token agrees with LOCAL_PEERPID and a deliberately wrong PID refuses.
func TestSharedCodeIdentitySocketPair(t *testing.T) {
	descriptors, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	var connections []*net.UnixConn
	for _, descriptor := range descriptors {
		file := os.NewFile(uintptr(descriptor), "code-identity-socket")
		connection, err := net.FileConn(file)
		_ = file.Close()
		if err != nil {
			t.Fatal(err)
		}
		connections = append(connections, connection.(*net.UnixConn))
		defer connection.Close()
	}
	uid, pid, err := sharedUnixPeerIdentity(connections[0])
	if err != nil || uid != uint32(os.Geteuid()) || pid != os.Getpid() {
		t.Fatalf("socket identity uid=%d pid=%d err=%v", uid, pid, err)
	}
	if code := sharedRuntimeErrorCode(sharedSocketCodeIdentity(connections[0], pid+1)); code != "broker_identity_mismatch" {
		t.Fatalf("token/socket mismatch code=%q", code)
	}
	typed := validateSharedCodeHashes(sharedCodeHash{1}, sharedCodeHash{2})
	if sharedPeerIdentityRefusal(typed) != typed {
		t.Fatal("typed peer refusal was relabeled")
	}
}

// A real child keeps its running identity after rename in both substitution
// directions, even when socket binds are unavailable to the test process.
func TestSharedCodeIdentityProcessReplacement(t *testing.T) {
	own, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	other := buildSharedCodeFixture(t)
	if runtime.GOARCH == "amd64" {
		if out, err := exec.Command("codesign", "--force", "--sign", "-", other).CombinedOutput(); err != nil {
			t.Fatalf("sign: %v %s", err, out)
		}
	}
	for _, different := range []bool{false, true} {
		t.Run(fmt.Sprintf("different_running_code=%t", different), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "peer")
			copyFile := func(from, to string) {
				t.Helper()
				data, err := os.ReadFile(from)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(to, data, 0700); err != nil {
					t.Fatal(err)
				}
			}
			source, replacement := own, other
			if different {
				source, replacement = other, own
			}
			copyFile(source, path)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, path, "-test.run=^TestSharedCodeIdentityChild$")
			command.Env = append(os.Environ(), "CODE_CHILD=1")
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
			defer func() { cancel(); _ = input.Close(); _ = command.Wait() }()
			if line, err := bufio.NewReader(output).ReadString('\n'); err != nil || line != "ready\n" {
				t.Fatalf("readiness=%q err=%v", line, err)
			}
			observation, err := inspectSharedProcess(command.Process.Pid)
			if err != nil {
				t.Fatal(err)
			}
			before := sharedObservedCodeIdentity(observation)
			want := ""
			if different {
				want = "broker_executable_identity_mismatch"
			}
			if code := sharedRuntimeErrorCode(before); code != want {
				t.Fatalf("before replacement code=%q want=%q", code, want)
			}
			copyFile(replacement, path+".next")
			if err := os.Rename(path+".next", path); err != nil {
				t.Fatal(err)
			}
			_, err = processExecIdentity(observation)
			if code := sharedRuntimeErrorCode(err); code != want {
				t.Fatalf("after replacement code=%q want=%q err=%v", code, want, err)
			}
		})
	}
}

// Both self and peer require CS_VALID before and after a matching hash query;
// other flags alone do not suffice, and every peer call uses the identical token pointer.
func TestSharedCodeIdentitySignatureValidity(t *testing.T) {
	for _, peerMode := range []bool{false, true} {
		for _, test := range []struct {
			name          string
			flags         [2]uint32
			statusErrorAt int
			hashError     bool
			want          string
		}{
			{name: "valid", flags: [2]uint32{sharedCSValid | 2, sharedCSValid | 2}},
			{name: "invalid before", flags: [2]uint32{2, sharedCSValid}, want: "broker_identity_unavailable"},
			{name: "invalid after", flags: [2]uint32{sharedCSValid, 2}, want: "broker_identity_unavailable"},
			{name: "zero flags", want: "broker_identity_unavailable"},
			{name: "status before error", flags: [2]uint32{sharedCSValid, sharedCSValid}, statusErrorAt: 1, want: "broker_identity_unavailable"},
			{name: "status after error", flags: [2]uint32{sharedCSValid, sharedCSValid}, statusErrorAt: 2, want: "broker_identity_unavailable"},
			{name: "hash error", flags: [2]uint32{sharedCSValid, sharedCSValid}, hashError: true, want: "broker_identity_unavailable"},
		} {
			t.Run(fmt.Sprintf("peer=%t/%s", peerMode, test.name), func(t *testing.T) {
				token := sharedAuditToken{5: 123, 7: 456}
				var expected *sharedAuditToken
				if peerMode {
					expected = &token
				}
				calls := 0
				sequence := ""
				status := func(pid int, got *sharedAuditToken) (uint32, error) {
					if pid != 123 || got != expected {
						t.Fatal("status lost pid/token binding")
					}
					calls++
					sequence += "status;"
					if calls == test.statusErrorAt {
						return sharedCSValid, os.ErrPermission
					}
					return test.flags[calls-1], nil
				}
				hash := func(pid int, got *sharedAuditToken) (sharedCodeHash, error) {
					if pid != 123 || got != expected {
						t.Fatal("hash lost pid/token binding")
					}
					sequence += "hash;"
					if test.hashError {
						return sharedCodeHash{1}, os.ErrPermission
					}
					return sharedCodeHash{1}, nil
				}
				query := func() (sharedCodeHash, error) { return sharedValidatedCodeHash(123, expected, status, hash) }
				valid := func() (sharedCodeHash, error) { return sharedCodeHash{1}, nil }
				var err error
				if peerMode {
					err = sharedTokenCodeIdentity(123, token, valid, func(int, *sharedAuditToken) (sharedCodeHash, error) { return query() })
				} else {
					err = sharedTokenCodeIdentity(123, token, query, func(int, *sharedAuditToken) (sharedCodeHash, error) { return valid() })
				}
				if code := sharedRuntimeErrorCode(err); code != test.want {
					t.Fatalf("code=%q want=%q err=%v", code, test.want, err)
				}
				if test.want == "" && sequence != "status;hash;status;" {
					t.Fatalf("query order %q", sequence)
				}
			})
		}
	}
}

func sharedCodeSocketPair(t *testing.T) (*net.UnixConn, *net.UnixConn) {
	t.Helper()
	descriptors, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	var connections [2]*net.UnixConn
	for i, fd := range descriptors {
		f := os.NewFile(uintptr(fd), "identity-peer")
		c, err := net.FileConn(f)
		_ = f.Close()
		if err != nil {
			t.Fatal(err)
		}
		connections[i] = c.(*net.UnixConn)
		t.Cleanup(func() { _ = c.Close() })
		_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	}
	return connections[0], connections[1]
}

// Every request refuses changed/unreadable tokens and closes; valid status succeeds.
// This exercises the production request loop without a filesystem socket bind.
func TestSharedCodeIdentityRequestRecheck(t *testing.T) {
	for _, kind := range []string{"status", "acquire", "heartbeat", "release", "stop"} {
		for _, failure := range []string{"pid", "pidversion", "unreadable"} {
			t.Run(kind+"/"+failure, func(t *testing.T) {
				fixture := newSharedBrokerAdmissionFixture(t)
				server, client := sharedCodeSocketPair(t)
				token := sharedAuditToken{5: uint32(fixture.observation.PID), 7: 1}
				calls := 0
				fixture.server.admission.peerToken = func(*net.UnixConn) (sharedAuditToken, error) {
					calls++
					if calls <= 3 {
						return token, nil
					} // hello and both checks of a valid status control
					switch failure {
					case "pid":
						token[5]++
					case "pidversion":
						token[7]++
					case "unreadable":
						return token, os.ErrPermission
					}
					return token, nil
				}
				done := make(chan struct{})
				go func() {
					defer close(done)
					fixture.server.handleConnection(&sharedBrokerConnection{connection: server, reader: bufio.NewReader(server)})
				}()
				if err := writeSharedWireMessage(client, fixture.hello); err != nil {
					t.Fatal(err)
				}
				reader := bufio.NewReader(client)
				if response, err := readSharedWireMessage(reader); err != nil || response.Type != "hello_ok" {
					t.Fatalf("hello=%+v err=%v", response, err)
				}
				if err := writeSharedWireMessage(client, sharedWireMessage{Type: "status"}); err != nil {
					t.Fatal(err)
				}
				if response, err := readSharedWireMessage(reader); err != nil || response.Type != "status" {
					t.Fatalf("control=%+v err=%v", response, err)
				}
				if err := writeSharedWireMessage(client, sharedWireMessage{Type: kind, Force: true}); err != nil {
					t.Fatal(err)
				}
				response, err := readSharedWireMessage(reader)
				want := "broker_identity_mismatch"
				if failure == "unreadable" {
					want = "broker_identity_unavailable"
				}
				if err != nil || response.Type != "refused" || response.Code != want {
					t.Fatalf("response=%+v err=%v", response, err)
				}
				if _, err := reader.ReadByte(); !errors.Is(err, io.EOF) {
					t.Fatalf("want client EOF, got %v", err)
				}
				<-done
				if len(fixture.server.leases) != 0 || fixture.server.hadLease || len(fixture.server.events) != 0 {
					t.Fatal("refusal changed lease/stop state")
				}
			})
		}
	}
}

// An incarnation change during admission work refuses before reserving a lease,
// with no lease file or stop event; an unchanged token grants a positive control.
func TestSharedCodeIdentityEffectRecheck(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(fmt.Sprint(changed), func(t *testing.T) {
			fixture := newSharedBrokerAdmissionFixture(t)
			token := sharedAuditToken{5: uint32(fixture.observation.PID), 7: 1}
			current := token
			fixture.server.resources.beforeLeaseReservation = func(uint64) {
				if changed {
					current[7]++
				}
			}
			read := func(*net.UnixConn) (sharedAuditToken, error) { return current, nil }
			check := func() error { return sharedRecheckPeerToken(nil, token, read) }
			lease, err := fixture.server.acquireLease(sharedWireMessage{Type: "acquire", ClientKey: "control"}, fixture.hello, fixture.observation, check)
			if !changed {
				if err != nil || lease == nil || len(fixture.server.leases) != 1 {
					t.Fatalf("valid lease=%v err=%v", lease, err)
				}
				return
			}
			if code := sharedRuntimeErrorCode(err); code != "broker_identity_mismatch" || lease != nil {
				t.Fatalf("lease=%v err=%v", lease, err)
			}
			entries, readErr := os.ReadDir(fixture.server.resolved.Paths.LeasesDir)
			if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
				t.Fatal(readErr)
			}
			if len(entries) != 0 || len(fixture.server.leases) != 0 || fixture.server.hadLease || len(fixture.server.events) != 0 {
				t.Fatal("changed incarnation had lease/stop effects")
			}
		})
	}
}

// The inherited fd survives a real re-exec of the same trusted test binary.
func TestSharedCodeIdentityExecPeerHelper(t *testing.T) {
	stage := os.Getenv("CODE_EXEC_STAGE")
	if stage == "" {
		return
	}
	fd := 3
	if stage == "before" {
		pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
		if err != nil {
			t.Fatal(err)
		}
		// Create the authenticated pair in the child so LOCAL_PEERPID names it.
		if err := unix.Sendmsg(3, []byte{1}, unix.UnixRights(pair[0]), nil, 0); err != nil {
			t.Fatal(err)
		}
		_ = unix.Close(pair[0])
		_ = unix.Close(3)
		fd = pair[1]
	} else {
		var err error
		fd, err = strconv.Atoi(os.Getenv("CODE_EXEC_PEER_FD"))
		if err != nil {
			t.Fatal(err)
		}
	}
	file := os.NewFile(uintptr(fd), "inherited-peer")
	defer file.Close()
	connection, err := net.FileConn(file)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(10 * time.Second))
	reader := bufio.NewReader(connection)
	if stage == "before" {
		var hello sharedWireMessage
		if err := json.Unmarshal([]byte(os.Getenv("CODE_EXEC_HELLO")), &hello); err != nil {
			t.Fatal(err)
		}
		hello.ClientPID = os.Getpid()
		if err := writeSharedWireMessage(connection, hello); err != nil {
			t.Fatal(err)
		}
		response, err := readSharedWireMessage(reader)
		if err != nil || response.Type != "hello_ok" {
			t.Fatalf("hello=%+v err=%v", response, err)
		}
		if err := writeSharedWireMessage(connection, sharedWireMessage{Type: "status"}); err != nil {
			t.Fatal(err)
		}
		response, err = readSharedWireMessage(reader)
		if err != nil || response.Type != "status" {
			t.Fatalf("control=%+v err=%v", response, err)
		}
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Setenv("CODE_EXEC_STAGE", "after"); err != nil {
			t.Fatal(err)
		}
		if err := os.Setenv("CODE_EXEC_PEER_FD", strconv.Itoa(fd)); err != nil {
			t.Fatal(err)
		}
		env := os.Environ()
		if err := unix.Exec(executable, []string{executable, "-test.run=^TestSharedCodeIdentityExecPeerHelper$"}, env); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeSharedWireMessage(connection, sharedWireMessage{Type: "stop", Force: true}); err != nil {
		t.Fatal(err)
	}
	response, err := readSharedWireMessage(reader)
	if err != nil || response.Type != "refused" || response.Code != "broker_identity_mismatch" {
		t.Fatalf("post-exec=%+v err=%v", response, err)
	}
	if _, err := reader.ReadByte(); !errors.Is(err, io.EOF) {
		t.Fatalf("post-exec client EOF: %v", err)
	}
}

// A real peer authenticates and obtains status, then re-execs trusted code on
// the same fd: the next stop request is refused, EOF follows, and no stop occurs.
func TestSharedCodeIdentityExecAfterAuthentication(t *testing.T) {
	fixture := newSharedBrokerAdmissionFixture(t)
	fixture.server.admission = sharedBrokerAdmissionSystem
	descriptors, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	serverFile := os.NewFile(uintptr(descriptors[0]), "broker-peer")
	childFile := os.NewFile(uintptr(descriptors[1]), "client-peer")
	defer childFile.Close()
	raw, err := net.FileConn(serverFile)
	_ = serverFile.Close()
	if err != nil {
		t.Fatal(err)
	}
	control := raw.(*net.UnixConn)
	defer control.Close()
	_ = control.SetReadDeadline(time.Now().Add(10 * time.Second))
	hello, err := json.Marshal(fixture.hello)
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "-test.run=^TestSharedCodeIdentityExecPeerHelper$")
	command.Env = append(os.Environ(), "CODE_EXEC_STAGE=before", "CODE_EXEC_HELLO="+string(hello))
	command.ExtraFiles = []*os.File{childFile}
	var output strings.Builder
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	_ = childFile.Close()
	defer func() { cancel(); _ = command.Wait() }()
	var payload [1]byte
	rights := make([]byte, unix.CmsgSpace(4))
	_, n, _, _, err := control.ReadMsgUnix(payload[:], rights)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := unix.ParseSocketControlMessage(rights[:n])
	if err != nil || len(messages) != 1 {
		t.Fatalf("rights: %v %v", messages, err)
	}
	fds, err := unix.ParseUnixRights(&messages[0])
	if err != nil || len(fds) != 1 {
		t.Fatalf("fds: %v %v", fds, err)
	}
	peerFile := os.NewFile(uintptr(fds[0]), "child-created-peer")
	peer, err := net.FileConn(peerFile)
	_ = peerFile.Close()
	if err != nil {
		t.Fatal(err)
	}
	server := peer.(*net.UnixConn)
	defer server.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fixture.server.handleConnection(&sharedBrokerConnection{connection: server, reader: bufio.NewReader(server)})
	}()
	if err := command.Wait(); err != nil {
		t.Fatalf("exec peer: %v\n%s", err, output.String())
	}
	<-done
	if len(fixture.server.events) != 0 || len(fixture.server.leases) != 0 {
		t.Fatal("exec peer changed broker state")
	}
}

// Token changes after request dispatch still block heartbeat/release/stop effects.
// A refusal may clean up its own lease, but cannot refresh it or release another owner.
func TestSharedCodeIdentityDispatchEffectRecheck(t *testing.T) {
	for _, kind := range []string{"heartbeat", "release", "stop"} {
		for _, changed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/changed=%t", kind, changed), func(t *testing.T) {
				fixture := newSharedBrokerAdmissionFixture(t)
				server, client := sharedCodeSocketPair(t)
				wrapped := &sharedBrokerConnection{connection: server, reader: bufio.NewReader(server)}
				token := sharedAuditToken{5: uint32(fixture.observation.PID), 7: 1}
				calls := 0
				fixture.server.admission.peerToken = func(*net.UnixConn) (sharedAuditToken, error) {
					calls++
					current := token
					if changed && calls >= 3 {
						current[7]++
					}
					return current, nil
				}
				initial := time.Now().Add(-time.Minute).UTC()
				lease := &SharedLeaseRecord{LeaseID: "held", LeaseToken: "other-owner-token", LastHeartbeatAt: initial}
				fixture.server.leases[lease.LeaseID] = lease
				message := sharedWireMessage{Type: kind, Force: true}
				if kind == "heartbeat" {
					wrapped.leaseID = lease.LeaseID
					message.LeaseID = lease.LeaseID
				}
				if kind == "release" {
					message.LeaseToken = lease.LeaseToken
					owner, _ := sharedCodeSocketPair(t)
					fixture.server.connections[&sharedBrokerConnection{connection: owner, leaseID: lease.LeaseID}] = true
				}
				done := make(chan struct{})
				go func() { defer close(done); fixture.server.handleConnection(wrapped) }()
				if err := writeSharedWireMessage(client, fixture.hello); err != nil {
					t.Fatal(err)
				}
				reader := bufio.NewReader(client)
				if response, err := readSharedWireMessage(reader); err != nil || response.Type != "hello_ok" {
					t.Fatalf("hello=%+v err=%v", response, err)
				}
				if err := writeSharedWireMessage(client, message); err != nil {
					t.Fatal(err)
				}
				response, err := readSharedWireMessage(reader)
				if err != nil {
					t.Fatal(err)
				}
				if changed {
					if response.Type != "refused" || response.Code != "broker_identity_mismatch" {
						t.Fatalf("refusal=%+v", response)
					}
					if _, err := reader.ReadByte(); !errors.Is(err, io.EOF) {
						t.Fatalf("want EOF: %v", err)
					}
				} else {
					want := map[string]string{"heartbeat": "heartbeat_ok", "release": "released", "stop": "stopping"}[kind]
					if response.Type != want {
						t.Fatalf("response=%+v want=%s", response, want)
					}
					_ = client.Close()
				}
				<-done
				if changed {
					if !lease.LastHeartbeatAt.Equal(initial) {
						t.Fatal("refusal refreshed heartbeat")
					}
					if kind == "release" && fixture.server.leases[lease.LeaseID] != lease {
						t.Fatal("refusal removed another owner's lease")
					}
					if kind == "stop" && len(fixture.server.events) != 0 {
						t.Fatal("refusal queued stop")
					}
				} else if kind == "heartbeat" && !lease.LastHeartbeatAt.After(initial) {
					t.Fatal("positive control did not refresh heartbeat")
				}
			})
		}
	}
}

// Closing immediately after stopping leaves exactly the accepted stop event queued.
// A failed final recheck sends a typed refusal and EOF, with no stopping or stop event.
func TestSharedCodeIdentityStopAcknowledgementOrdering(t *testing.T) {
	for _, force := range []bool{false, true} {
		for _, failure := range []string{"", "pidversion", "unreadable"} {
			t.Run(fmt.Sprintf("force=%t/failure=%s", force, failure), func(t *testing.T) {
				fixture := newSharedBrokerAdmissionFixture(t)
				server, client := sharedCodeSocketPair(t)
				token := sharedAuditToken{5: uint32(fixture.observation.PID), 7: 1}
				clientClosed := make(chan struct{})
				calls := 0
				fixture.server.admission.peerToken = func(*net.UnixConn) (sharedAuditToken, error) {
					calls++
					if calls > 3 {
						// Schedule any erroneous post-acknowledgement recheck after closure.
						select {
						case <-clientClosed:
						case <-time.After(5 * time.Second):
						}
						return sharedAuditToken{}, unix.ENOTCONN
					}
					if calls == 3 {
						switch failure {
						case "pidversion":
							current := token
							current[7]++
							return current, nil
						case "unreadable":
							return sharedAuditToken{}, unix.ENOTCONN
						}
					}
					return token, nil
				}
				done := make(chan struct{})
				go func() {
					defer close(done)
					fixture.server.handleConnection(&sharedBrokerConnection{connection: server, reader: bufio.NewReader(server)})
				}()
				if err := writeSharedWireMessage(client, fixture.hello); err != nil {
					t.Fatal(err)
				}
				reader := bufio.NewReader(client)
				if response, err := readSharedWireMessage(reader); err != nil || response.Type != "hello_ok" {
					t.Fatalf("hello=%+v err=%v", response, err)
				}
				if err := writeSharedWireMessage(client, sharedWireMessage{Type: "stop", Force: force}); err != nil {
					t.Fatal(err)
				}
				response, err := readSharedWireMessage(reader)
				if failure == "" {
					_ = client.Close()
				}
				close(clientClosed)
				if err != nil {
					t.Fatal(err)
				}
				if failure == "" {
					if response.Type != "stopping" {
						t.Fatalf("response=%+v", response)
					}
				} else {
					want := "broker_identity_mismatch"
					if failure == "unreadable" {
						want = "broker_identity_unavailable"
					}
					if response.Type != "refused" || response.Code != want {
						t.Fatalf("response=%+v want typed refusal=%s", response, want)
					}
					if _, err := reader.ReadByte(); !errors.Is(err, io.EOF) {
						t.Fatalf("want EOF, got %v", err)
					}
				}
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("broker did not finish stop request")
				}
				if failure == "" {
					select {
					case event := <-fixture.server.events:
						if event.kind != "stop" || event.force != force {
							t.Fatalf("event=%+v", event)
						}
					default:
						t.Fatal("acknowledged stop was not queued before client closure")
					}
				}
				if len(fixture.server.events) != 0 || len(fixture.server.leases) != 0 {
					t.Fatal("unexpected stop or lease effects")
				}
			})
		}
	}
}

// Even a peer changing during random-token generation cannot reserve a lease.
func TestSharedCodeIdentityFinalLeaseEffectRecheck(t *testing.T) {
	fixture := newSharedBrokerAdmissionFixture(t)
	token := sharedAuditToken{5: uint32(fixture.observation.PID), 7: 1}
	current := token
	original := sharedBrokerRandomReader
	t.Cleanup(func() { sharedBrokerRandomReader = original })
	sharedBrokerRandomReader = sharedCodeChangingReader{change: func() { current[7]++ }}
	check := func() error {
		return sharedRecheckPeerToken(nil, token, func(*net.UnixConn) (sharedAuditToken, error) { return current, nil })
	}
	lease, err := fixture.server.acquireLease(sharedWireMessage{Type: "acquire"}, fixture.hello, fixture.observation, check)
	if sharedRuntimeErrorCode(err) != "broker_identity_mismatch" || lease != nil || len(fixture.server.leases) != 0 || fixture.server.hadLease {
		t.Fatalf("lease=%v err=%v", lease, err)
	}
}

type sharedCodeChangingReader struct{ change func() }

func (r sharedCodeChangingReader) Read(data []byte) (int, error) {
	for i := range data {
		data[i] = 1
	}
	r.change()
	return len(data), nil
}

// A peer changing during provider I/O cannot commit the resulting pressure latch.
// The generation reserved while the peer was still valid is allowed to remain.
func TestSharedCodeIdentityProviderEffectRecheck(t *testing.T) {
	for _, commit := range []bool{false, true} {
		for _, changed := range []bool{false, true} {
			t.Run(fmt.Sprintf("admission=%t/changed=%t", commit, changed), func(t *testing.T) {
				fixture := newSharedBrokerAdmissionFixture(t)
				configureSharedBrokerResourceFixture(&fixture, observedSharedRuntimeResources(2000, "idle"))
				token := sharedAuditToken{5: uint32(fixture.observation.PID), 7: 1}
				current := token
				observe := fixture.server.resources.observe
				fixture.server.resources.observe = func(ctx context.Context, client *http.Client, endpoint, model string, policy PiRuntimeResourcePressure) (sharedRuntimeProviderResourceObservation, error) {
					result, err := observe(ctx, client, endpoint, model, policy)
					if changed {
						current[7]++
					}
					return result, err
				}
				check := func() error {
					return sharedRecheckPeerToken(nil, token, func(*net.UnixConn) (sharedAuditToken, error) { return current, nil })
				}
				_, _, _, err := fixture.server.observeResourceStatusChecked(commit, check)
				if changed {
					if sharedRuntimeErrorCode(err) != "broker_identity_mismatch" || fixture.server.pressureLatched || fixture.server.resourcePressureGeneration != 0 || len(fixture.server.events) != 0 {
						t.Fatalf("changed peer committed pressure: err=%v", err)
					}
				} else if err != nil || (commit && !fixture.server.pressureLatched) {
					t.Fatalf("valid pressure control: err=%v", err)
				}
			})
		}
	}
}
