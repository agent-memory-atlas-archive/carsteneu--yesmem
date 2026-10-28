//go:build !linux

package daemon

import "net"

// peerPID has no portable form outside Linux. The socket layer then reports -1
// for every connection, which the channel endpoints read as "no peer": the named
// session stands, exactly as it does for a call from the daemon's own code — the
// guard cannot be enforced where the platform has no peer credentials
// (channelInboxSession).
func peerPID(net.Conn) int {
	return -1
}
