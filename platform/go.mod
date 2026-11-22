// The governance kernel is its own module so its release cadence decouples from
// the orchestrator's.
//
// One module meant every orchestrator commit forced a tag bump on the kernel's
// consumer, which is how vagent's pin drifted 51 commits behind. The kernel is
// pure stdlib and imports nothing from gaap root, so the split costs one import
// path that does not even change: `github.com/aj-nt/gaap/platform` names this
// module now rather than a directory inside the root one.
module github.com/aj-nt/gaap/platform

go 1.27.2
