package pdf

import (
	"log"
	"os"
	"os/exec"

	"github.com/leftathome/glovebox/connector/enrich"
)

// lookPath is the package-level seam used by init() so tests can inject
// a fake LookPath without mutating $PATH. Production callers see
// exec.LookPath; tests instead drive registerIfAvailable() directly with
// their own LookPath function (the recommended path) and never touch
// this variable.
var lookPath = exec.LookPath

// initLogger is the package-level log destination for the init-time
// warning. Default: os.Stderr via log.New. Tests drive
// registerIfAvailable() with a captured logger and so do not need to
// stomp this either.
var initLogger = log.New(os.Stderr, "", 0)

func init() {
	registerIfAvailable(enrich.Default, lookPath, initLogger)
}

// registerIfAvailable is the testable core of init(): given a registry,
// a LookPath function, and a logger, it either registers an Enricher
// instance and returns true, or logs the documented WHAT/CHECK/FIX
// warning and returns false. Exposed unexported but package-internal
// for use from tests in this package.
//
// Splitting init() this way lets us assert both branches without ever
// having to mutate $PATH or the package-level lookPath variable from
// concurrent test runs.
func registerIfAvailable(reg *enrich.Registry, look func(string) (string, error), logger *log.Logger) bool {
	if _, err := look(pdftotextBinary); err != nil {
		logger.Printf(
			"enrich/pdf: pdftotext not found in PATH; the PDF enricher is disabled for this connector.\n"+
				"  CHECK: docker inspect <image> for /usr/bin/pdftotext\n"+
				"  FIX:   rebase this connector on glovebox-enricher-runtime, or accept that PDFs will not be text-extracted.\n"+
				"  (LookPath error: %v)",
			err,
		)
		return false
	}
	reg.Register(&Enricher{})
	return true
}
