package peercred

import (
	"fmt"
	"net"
)

// UID returns the uid of the process on the other end of a unix socket.
//
// Transport alone yields a uid, not an agent id. On a shared host every agent runs as
// some uid; on a containerised host each agent has its own. Either way the uid is the
// only part of the identity the peer cannot simply assert, so it is the part the broker
// builds the rest on: (uid, agent type authorised by that uid's grant).
//
// Implemented per platform because there is no portable standard-library call:
// SO_PEERCRED on Linux, LOCAL_PEERCRED/XUCRED on Darwin. Anything else refuses rather
// than guessing — a broker that cannot identify its peer must not serve it.
func UID(conn net.Conn) (uint32, error) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, fmt.Errorf("peercred: %T is not a unix connection", conn)
	}
	return uidOf(uc)
}
