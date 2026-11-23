// The wire is its own module so a client can speak to a broker without depending
// on the kernel that answers it.
//
// `platform` holds the policy engine, the ledger and the enforcer — the machinery
// that decides. A remote client needs none of that: it needs the shape of a
// Decision and the options that qualify one. While both lived in one module, a
// client's pin meant "a kernel release" and moved every time the kernel did,
// which made a stable wire look unstable. Here the version means what it says.
module github.com/aj-nt/gaap/protocol

go 1.27.2
