package platform

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The gate ticket is how an observation of the agent's OWN resources acquires
// internal provenance.
//
// Without it every observation an agent makes is external regardless of what it
// actually saw, so `fact` is unreachable for real reconnaissance and the provenance
// model's only effect is on free-text reasoning — the part with the least to do with
// whether an action is safe. The ticket is the narrow channel: the governor issues one
// on an allow for a `pure` call, the broker consumes it in the same transaction that
// records the observation, and only then can the resulting entry claim internal.
//
// It is NOT the ledger seal. A seal is a durable integrity proof over history; this is
// a short-lived capability token, single-use and expiring. Calling it signing would be
// the error v2 made, and the error is not cosmetic: a token that is mistaken for a
// proof gets trusted after it has expired.
const (
	// DefaultTicketTTL bounds a ticket. Short because a ticket is redeemed
	// immediately: a long-lived one is a credential lying around in a transcript.
	DefaultTicketTTL = 2 * time.Minute

	ticketPrefix = "gt1"
)

var (
	// ErrTicketInvalid covers a malformed token, a bad MAC, or a claim mismatch. The
	// cause is deliberately not distinguishable from outside: an attacker who can tell
	// "wrong key" from "wrong resource" learns which half to work on.
	ErrTicketInvalid = errors.New("ticket: invalid")
	// ErrTicketExpired is separate because it is not a forgery — a retry with a fresh
	// ticket is the correct response, and a caller cannot know that otherwise.
	ErrTicketExpired = errors.New("ticket: expired")
)

// TicketKey is the HMAC key for gate tickets.
//
// It cannot come from KeyRing. That interface is `Key(ref) []byte` plus `Destroy`:
// per-subject commitment keys, minted on first use and shredded on erasure. A ticket
// needs the opposite — one stable key that validates across subjects and survives a
// subject's erasure, since the ticket outlives no subject and proves nothing about one.
type TicketKey struct {
	key []byte
}

// LoadOrCreateTicketKey reads the ticket key at path, creating one if absent.
//
// Persisted beside kill.json rather than derived from a config secret: rotation is a
// deliberate operation, and a key that changes when a deploy changes an environment
// variable invalidates every in-flight ticket and looks like a forgery wave.
func LoadOrCreateTicketKey(path string) (*TicketKey, error) {
	raw, err := os.ReadFile(path)
	if err == nil {
		b, derr := hex.DecodeString(strings.TrimSpace(string(raw)))
		if derr != nil || len(b) < 32 {
			return nil, fmt.Errorf("ticket key %s is not a hex-encoded 32-byte key: %w", path, derr)
		}
		return &TicketKey{key: b}, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// 0600: a ticket key readable by another uid is a forge-anything key.
	if err := os.WriteFile(path, []byte(hex.EncodeToString(key)), 0o600); err != nil {
		return nil, err
	}
	return &TicketKey{key: key}, nil
}

// NewTicketKey builds a key from raw bytes. Tests only.
func NewTicketKey(key []byte) *TicketKey { return &TicketKey{key: key} }

// Ticket is the claim set a token carries.
type Ticket struct {
	// ID identifies this grant of authority. Single use is enforced on the ID, so a
	// token replayed with the same ID is detectable without comparing signatures.
	ID       string `json:"id"`
	EntryID  string `json:"entry_id"`
	Agent    string `json:"agent"`
	Resource string `json:"resource"`
	Expires  int64  `json:"exp"`
}

// Issue mints a ticket for one (entry, agent, resource) triple. A zero ttl uses
// DefaultTicketTTL.
func (k *TicketKey) Issue(entryID, agent, resource string, ttl time.Duration) (string, Ticket, error) {
	if entryID == "" || agent == "" || resource == "" {
		return "", Ticket{}, errors.New("ticket: entry_id, agent and resource are all required")
	}
	if ttl <= 0 {
		ttl = DefaultTicketTTL
	}
	idb := make([]byte, 16)
	if _, err := rand.Read(idb); err != nil {
		return "", Ticket{}, err
	}
	t := Ticket{
		ID:       hex.EncodeToString(idb),
		EntryID:  entryID,
		Agent:    agent,
		Resource: resource,
		Expires:  time.Now().Add(ttl).Unix(),
	}
	payload, err := json.Marshal(t)
	if err != nil {
		return "", Ticket{}, err
	}
	mac := k.mac(payload)
	return ticketPrefix + "." + base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac), t, nil
}

// Verify checks the MAC, the expiry and the claims.
//
// The claims are checked against the caller's expectation, not merely returned: a
// valid ticket for tenant A presented while asking about tenant B is a valid MAC over
// the wrong assertion, and returning claims without comparing them is how that passes.
func (k *TicketKey) Verify(token, entryID, agent, resource string) (Ticket, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != ticketPrefix {
		return Ticket{}, ErrTicketInvalid
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Ticket{}, ErrTicketInvalid
	}
	gotMAC, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Ticket{}, ErrTicketInvalid
	}
	if !hmac.Equal(gotMAC, k.mac(payload)) {
		return Ticket{}, ErrTicketInvalid
	}
	var t Ticket
	if err := json.Unmarshal(payload, &t); err != nil {
		return Ticket{}, ErrTicketInvalid
	}
	if t.EntryID != entryID || t.Agent != agent || t.Resource != resource {
		return Ticket{}, ErrTicketInvalid
	}
	if time.Now().Unix() >= t.Expires {
		return Ticket{}, ErrTicketExpired
	}
	return t, nil
}

func (k *TicketKey) mac(payload []byte) []byte {
	m := hmac.New(sha256.New, k.key)
	m.Write(payload)
	return m.Sum(nil)
}
