package integration

import (
	"fmt"
	"os"
)

var (
	dockerfilePath = fmt.Sprintf("%s/../", os.Getenv("PWD"))
	storageVolume  = fmt.Sprintf("%s/files", os.Getenv("PWD"))
	// coverageDir is bind-mounted into each container at /covdata. The
	// integration image is built with the COVER=-cover build arg (released
	// images are not), so the instrumented binary writes covdata files here
	// on graceful shutdown. Tests merge these in CI.
	coverageDir = fmt.Sprintf("%s/covdata", os.Getenv("PWD"))
	// coverBuildArg enables coverage instrumentation in the test image only;
	// passed through to the Dockerfile's COVER build arg.
	coverBuildArg = "-cover"
)
