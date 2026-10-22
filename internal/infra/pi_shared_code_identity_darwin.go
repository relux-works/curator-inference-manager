//go:build darwin

package infra

import (
	"encoding/binary"
	"errors"
	"net"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// audit_token_t is eight uint32 words; pid and pidversion are words 5 and 7.
// https://github.com/apple-oss-distributions/xnu/blob/main/osfmk/mach/message.h
// https://github.com/apple-oss-distributions/xnu/blob/main/bsd/kern/kern_prot.c
// x/sys provides LOCAL_PEERTOKEN and the syscalls, but no audit_token_t wrapper.
type sharedAuditToken [8]uint32
type sharedCodeHash [20]byte

// CS_OPS_STATUS/CS_OPS_CDHASH: XNU bsd/sys/codesign.h (includes kern/cs_blobs.h).
// CS_VALID is the dynamically valid bit, not merely the presence of a cached hash.
// https://github.com/apple-oss-distributions/xnu/blob/xnu-12377.1.9/bsd/sys/codesign.h
// https://github.com/apple-oss-distributions/xnu/blob/xnu-12377.1.9/osfmk/kern/cs_blobs.h
const (
	sharedCSOpsStatus        = 0
	sharedCSOpsCDHash        = 5
	sharedCSValid     uint32 = 0x1
)

func sharedCodeOperation(pid int, operation uintptr, buffer unsafe.Pointer, size uintptr, token *sharedAuditToken) error {
	var errno unix.Errno
	if token != nil {
		_, _, errno = unix.Syscall6(unix.SYS_CSOPS_AUDITTOKEN, uintptr(pid), operation, uintptr(buffer), size, uintptr(unsafe.Pointer(token)), 0)
	} else {
		_, _, errno = unix.Syscall6(unix.SYS_CSOPS, uintptr(pid), operation, uintptr(buffer), size, 0, 0)
	}
	runtime.KeepAlive(token)
	runtime.KeepAlive(buffer)
	if errno != 0 {
		return errno
	}
	return nil
}

func sharedKernelCodeStatus(pid int, token *sharedAuditToken) (uint32, error) {
	var flags uint32
	err := sharedCodeOperation(pid, sharedCSOpsStatus, unsafe.Pointer(&flags), 4, token)
	return flags, err
}

func sharedKernelRawCodeHash(pid int, token *sharedAuditToken) (sharedCodeHash, error) {
	var hash sharedCodeHash
	err := sharedCodeOperation(pid, sharedCSOpsCDHash, unsafe.Pointer(&hash[0]), uintptr(len(hash)), token)
	return hash, err
}

func sharedKernelCodeHash(pid int, token *sharedAuditToken) (sharedCodeHash, error) {
	return sharedValidatedCodeHash(pid, token, sharedKernelCodeStatus, sharedKernelRawCodeHash)
}

// Status brackets the hash query to detect invalidation during that query.
// Every peer operation receives the very same token, including its pidversion.
func sharedValidatedCodeHash(pid int, token *sharedAuditToken, status func(int, *sharedAuditToken) (uint32, error), query func(int, *sharedAuditToken) (sharedCodeHash, error)) (sharedCodeHash, error) {
	if pid <= 0 || (token != nil && int(token[5]) != pid) {
		return sharedCodeHash{}, sharedRuntimeError("broker_identity_unavailable", errors.New("code identity pid differs from audit token"))
	}
	var hash sharedCodeHash
	for i := 0; i < 2; i++ {
		flags, err := status(pid, token)
		if err != nil {
			return sharedCodeHash{}, sharedRuntimeError("broker_identity_unavailable", err)
		}
		if flags&sharedCSValid == 0 {
			return sharedCodeHash{}, sharedRuntimeError("broker_identity_unavailable", errors.New("code signature validity unavailable or invalid"))
		}
		if i == 0 {
			hash, err = query(pid, token)
			if err != nil {
				return sharedCodeHash{}, sharedRuntimeError("broker_identity_unavailable", err)
			}
			if hash == (sharedCodeHash{}) {
				return hash, sharedRuntimeError("broker_identity_unavailable", errors.New("kernel code identity is absent"))
			}
		}
	}
	return hash, nil
}

func sharedOwnCodeHash() (sharedCodeHash, error) {
	return sharedKernelCodeHash(os.Getpid(), nil)
}

func validateSharedCodeHashes(self, peer sharedCodeHash) error {
	if self == (sharedCodeHash{}) || peer == (sharedCodeHash{}) {
		return sharedRuntimeError("broker_identity_unavailable", errors.New("kernel code identity is absent"))
	}
	if self != peer {
		return sharedRuntimeError("broker_executable_identity_mismatch", errors.New("kernel code identities differ"))
	}
	return nil
}

func sharedSocketCodeIdentity(connection *net.UnixConn, pid int) error {
	token, err := sharedSocketAuditToken(connection)
	if err != nil {
		return err
	}
	return sharedTokenCodeIdentity(pid, token, sharedOwnCodeHash, sharedKernelCodeHash)
}

func sharedSocketAuditToken(connection *net.UnixConn) (sharedAuditToken, error) {
	raw, err := connection.SyscallConn()
	if err != nil {
		return sharedAuditToken{}, sharedRuntimeError("broker_identity_unavailable", err)
	}
	var token sharedAuditToken
	length := uint32(unsafe.Sizeof(token))
	var socketErr error
	if err := raw.Control(func(fd uintptr) {
		_, _, errno := unix.Syscall6(unix.SYS_GETSOCKOPT, fd, unix.SOL_LOCAL, unix.LOCAL_PEERTOKEN, uintptr(unsafe.Pointer(&token)), uintptr(unsafe.Pointer(&length)), 0)
		if errno != 0 {
			socketErr = errno
		}
	}); err != nil {
		return sharedAuditToken{}, sharedRuntimeError("broker_identity_unavailable", err)
	}
	if socketErr != nil || length != uint32(unsafe.Sizeof(token)) {
		return sharedAuditToken{}, sharedRuntimeError("broker_identity_unavailable", errors.New("peer audit token unavailable or truncated"))
	}
	return token, nil
}

func sharedRecheckPeerToken(connection *net.UnixConn, authenticated sharedAuditToken, read func(*net.UnixConn) (sharedAuditToken, error)) error {
	current, err := read(connection)
	if err != nil {
		return sharedRuntimeError("broker_identity_unavailable", err)
	}
	if current[5] != authenticated[5] || current[7] != authenticated[7] {
		return sharedRuntimeError("broker_identity_mismatch", errors.New("authenticated peer incarnation changed"))
	}
	return nil
}

func sharedTokenCodeIdentity(pid int, token sharedAuditToken, own func() (sharedCodeHash, error), peer func(int, *sharedAuditToken) (sharedCodeHash, error)) error {
	if pid <= 0 || int(token[5]) != pid {
		return sharedRuntimeError("broker_identity_mismatch", errors.New("audit token pid differs from LOCAL_PEERPID"))
	}
	selfHash, err := own()
	if err != nil {
		return sharedRuntimeError("broker_identity_unavailable", err)
	}
	peerHash, err := peer(pid, &token)
	if err != nil {
		return sharedRuntimeError("broker_identity_unavailable", err)
	}
	return validateSharedCodeHashes(selfHash, peerHash)
}

// Non-socket operator inspection binds csops to a kernel pid version too.
// PROC_PIDUNIQIDENTIFIERINFO (17), proc_uniqidentifierinfo (56 bytes):
// https://github.com/apple-oss-distributions/xnu/blob/main/bsd/sys/proc_info_private.h
// csops_internal checks token pid + pidversion, not caller-supplied credentials:
// https://github.com/apple-oss-distributions/xnu/blob/main/bsd/kern/kern_proc.c
func sharedObservedCodeIdentity(observation sharedProcessObservation) error {
	var info [56]byte
	count, _, errno := unix.Syscall6(unix.SYS_PROC_INFO, 2, uintptr(observation.PID), 17, 0, uintptr(unsafe.Pointer(&info[0])), uintptr(len(info)))
	if errno != 0 {
		return sharedRuntimeError("broker_identity_unavailable", errno)
	}
	if count != uintptr(len(info)) {
		return sharedRuntimeError("broker_identity_unavailable", errors.New("process pid version unavailable"))
	}
	token := sharedAuditToken{}
	token[5] = uint32(observation.PID)
	token[7] = binary.NativeEndian.Uint32(info[32:36])
	// Recheck the recorded incarnation after acquiring its pid version.
	current, err := inspectSharedProcessKernel(observation.PID)
	if err != nil || current.StartTime != observation.StartTime {
		return sharedRuntimeError("broker_identity_unavailable", errors.New("process incarnation changed during code inspection"))
	}
	return sharedTokenCodeIdentity(observation.PID, token, sharedOwnCodeHash, sharedKernelCodeHash)
}

func sharedPeerIdentityRefusal(err error) error {
	var refusal *SharedRuntimeError
	if errors.As(err, &refusal) {
		return err
	}
	return sharedRuntimeError("broker_peer_untrusted", err)
}

func sharedProcessIdentityRefusal(err error) error {
	var refusal *SharedRuntimeError
	if errors.As(err, &refusal) {
		return err
	}
	return sharedRuntimeError("broker_identity_unavailable", err)
}
