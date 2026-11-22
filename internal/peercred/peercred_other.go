//go:build !linux && !darwin

package peercred

import (
	"errors"
	"net"
)

func uidOf(*net.UnixConn) (uint32, error) {
	return 0, errors.New("peercred: no peer-credential mechanism on this platform; refusing to guess an identity")
}
