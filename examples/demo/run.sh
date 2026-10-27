#!/usr/bin/env bash
# Gaap in ninety seconds: a decision, the record, the chain, a tamper, an erasure.
#
# Everything below runs the real CLI against a real record. There is no demo mode
# in the binary and nothing here is special-cased, which is the point: if this
# script can show it, an operator can do it.
set -euo pipefail

cd "$(dirname "$0")/../.."

WORK="$(mktemp -d)"
STATE="$WORK/state"
POLICY="$(cd "$(dirname "$0")" && pwd)/rules.json"
trap 'rm -rf "$WORK"' EXIT

BIN="${GAAP_BIN:-}"
if [ -z "$BIN" ]; then
  BIN="$WORK/gaap"
  go build -o "$BIN" ./cmd/gaap
fi

beat() { printf '\n\033[1m%s\033[0m\n' "$*"; }
note() { printf '  %s\n' "$*"; }

beat "0/5  A policy file is the sovereign's standing orders"
note "$POLICY"
note "Reads are allowed. Destructive shell commands are denied by name. Tests are allowed."
note "Everything else falls through to deny-by-default."

beat "1/5  The governor decides, and records what it decided"
"$BIN" govern decide --state "$STATE" --policy "$POLICY" \
  --subject agent-1 --tool ReadFile --args '{"path":"/etc/hosts"}' --read-tools ReadFile
"$BIN" govern decide --state "$STATE" --policy "$POLICY" \
  --subject agent-1 --tool Execute --args '{"command":"rm -rf /tmp/gaap-demo"}' --read-tools ReadFile

beat "2/5  What the record holds: structure and commitments, no arguments, no identity"
"$BIN" ledger tail --n 5 "$STATE/ledger.jsonl"
note "The subject column is a commitment, not a name. The arguments are not here at all."

beat "3/5  The chain verifies"
"$BIN" ledger verify "$STATE/ledger.jsonl"

beat "4/5  Change one recorded outcome and the chain fails"
cp "$STATE/ledger.jsonl" "$WORK/tampered.jsonl"
perl -pi -e 's/"outcome":"allow"/"outcome":"deny"/ if $. == 1' "$WORK/tampered.jsonl"
if "$BIN" ledger verify "$WORK/tampered.jsonl" 2>&1; then
  note "UNEXPECTED: the tampered record verified"
else
  note "^ that is the property. The record is evidence because it can fail."
fi

beat "5/5  Erase the subject: content gone, key shredded, chain still verifies"
"$BIN" govern erase --state "$STATE" --subject agent-1 --reason gdpr-request
"$BIN" ledger verify "$STATE/ledger.jsonl"
"$BIN" ledger tail --n 5 "$STATE/ledger.jsonl"
note "The decisions survive as structure. The link to the subject does not."
note "State directory: $STATE"
