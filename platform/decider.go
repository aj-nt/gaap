package platform

// Decider lives in the protocol module: it is the contract a remote client is written
// against, and a client should not have to import the machinery that answers it.
//
// This assertion stays here. *Governor satisfies the interface, and a change to Decide's
// signature must break the kernel and the client together instead of letting them drift
// apart.
var _ Decider = (*Governor)(nil)
