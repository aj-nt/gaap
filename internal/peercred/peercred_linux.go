package peercred

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// uidOf reads SO_PEERCRED. The kernel fills the credential from the peer's credentials
// at connect time, so the value is attested rather than claimed.
func uidOf(c *net.UnixConn) (uint32, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *unix.Ucred
	var sockErr error
	if err := raw.Control(func(fd uintptr) {
		cred, sockErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if sockErr != nil {
		return 0, fmt.Errorf("peercred: SO_PEERCRED: %w", sockErr)
	}
	return cred.Uid, nil
}
