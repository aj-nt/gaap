package platform

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

// CommitAlgoHMACSHA256 is the only commitment algorithm implemented.
const CommitAlgoHMACSHA256 = "hmac-sha256"

// Committer turns content into a keyed commitment so the ledger can reference
// it without storing it and without becoming a re-identification oracle.
//
// An unkeyed hash of low-entropy content is a pseudonymous identifier: anyone
// holding the ledger can hash a candidate list of names, arguments, or subjects
// and confirm which record belongs to which. Keying the hash with a random
// per-subject key removes the confirmation oracle, because without the key the
// commitment is indistinguishable from random.
//
// The commitment is NOT the chain hash. The chain hash (Entry.Hash) is unkeyed
// on purpose, so a third party can verify tamper-evidence forever with no
// secrets. The commitment is keyed on purpose, so an erasure can destroy it.
// Conflating the two would make erasure and verifiability compete; keeping them
// separate makes a crypto-shred leave a chain that still verifies.
type Committer struct {
	// Ring supplies per-subject keys. Where the ref comes from is the caller's
	// policy: a per-subject ref makes records about one subject linkable and
	// lets a single destruction unlink all of them; a per-record ref makes
	// records unlinkable but turns one erasure into N destructions.
	Ring KeyRing
	// Algo names the commitment algorithm. Empty means hmac-sha256.
	Algo string
}

// NewCommitter returns a Committer over ring.
func NewCommitter(ring KeyRing) (*Committer, error) {
	if ring == nil {
		return nil, errors.New("committer requires a key ring")
	}
	return &Committer{Ring: ring}, nil
}

func (c *Committer) algo() string {
	if c.Algo == "" {
		return CommitAlgoHMACSHA256
	}
	return c.Algo
}

// Algo reports the commitment algorithm name, defaulted. It is what an entry
// stamps in CommitAlgo so old commitments stay verifiable if the default
// ever changes.
func (c *Committer) AlgoName() string { return c.algo() }

// Commit returns the keyed commitment of value under ref's key. It returns
// ErrKeyDestroyed if the subject was erased, rather than minting a fresh key,
// because a fresh key would silently unlink the new record from the erased
// history it should not be able to reach anyway.
func (c *Committer) Commit(ref, value string) (string, error) {
	switch c.algo() {
	case CommitAlgoHMACSHA256:
	default:
		return "", fmt.Errorf("unsupported commitment algorithm %q (only %q implemented)", c.algo(), CommitAlgoHMACSHA256)
	}
	key, err := c.Ring.Key(ref)
	if err != nil {
		return "", err
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte(value))
	return hex.EncodeToString(m.Sum(nil)), nil
}

// Match reports whether value is the content that commitment refers to. It
// fails closed: if the subject's key is gone, Match returns false and
// ErrKeyDestroyed. It never reports a match it cannot prove.
func (c *Committer) Match(ref, value, commitment string) (bool, error) {
	got, err := c.Commit(ref, value)
	if err != nil {
		return false, err
	}
	return hmac.Equal([]byte(got), []byte(commitment)), nil
}
