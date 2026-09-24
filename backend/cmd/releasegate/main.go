// Command releasegate applies the fork release contract inside the release
// workflow. It reads a JSON input file and writes a JSON verdict, so the same
// decision can be replayed locally against fixtures with no network or tag.
package main

import (
	"os"

	"github.com/Wei-Shaw/sub2api/internal/releasecontract"
)

func main() {
	os.Exit(releasecontract.Run(os.Args[1:], os.Stdout))
}
