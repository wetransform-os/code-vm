package cli

// version is reported by `code-vm --version`. Release builds override it at
// link time with
// -ldflags "-X github.com/wetransform/code-vm/internal/cli.version=<version>".
var version = "dev"
