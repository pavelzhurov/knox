package client

import "fmt"

// Version represents the compiled version of the client binary. It can be overridden at compile time with:
// `go build -ldflags "-X github.com/pavelzhurov/knox/client.Version=1.2.3" github.com/pavelzhurov/knox/cmd/dev_client`
// In the above example, knox version would give you `1.2.3`. By default, the version is `devel`.
var Version string = "devel"

var cmdVersion = &Command{
	Run:       runVersion,
	UsageLine: "version",
	Short:     "Prints the current version of the Knox client",
	Long: `
Prints the current version of the Knox client.
`,
}

// GetVersion exposes the current client version
func GetVersion() string {
	return Version
}

func runVersion(cmd *Command, args []string) {
	fmt.Printf("Knox CLI version %s\n", Version)
}
