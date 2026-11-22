package peercred

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// uidOf reads LOCAL_PEERCRED (XUCRED), Darwin's equivalent of SO_PEERCRED: the kernel
// hands over the peer's ucred, so the value is attested rather than claimed.
//
// golang.org/x/sys rather than the syscall package because Go's darwin syscall exports
// neither Xucred nor GetsockoptXucred. The Linux side uses the same library so the two
// implementations read alike.
func uidOf(c *net.UnixConn) (uint32, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var xu *unix.Xucred
	var sockErr error
	if err := raw.Control(func(fd uintptr) {
		xu, sockErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if sockErr != nil {
		return 0, fmt.Errorf("peercred: LOCAL_PEERCRED: %w", sockErr)
	}
	return xu.Uid, nil
}
