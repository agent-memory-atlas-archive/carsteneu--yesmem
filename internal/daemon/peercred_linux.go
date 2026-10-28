//go:build linux

package daemon

import (
	"net"
	"syscall"
)

// peerPID returns the pid of the process on the other end of a Unix-socket
// connection, as the kernel reports it (SO_PEERCRED). Unlike the _caller_pid a
// client sends, it is a fact about the connection and cannot be claimed by the
// caller. Returns -1 when the connection is not a Unix socket or the credential
// cannot be read.
func peerPID(conn net.Conn) int {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return -1
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return -1
	}
	pid := -1
	_ = raw.Control(func(fd uintptr) {
		cred, err := syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
		if err == nil && cred != nil {
			pid = int(cred.Pid)
		}
	})
	return pid
}
