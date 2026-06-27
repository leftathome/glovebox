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

// pdfDisabledMsg is the WHAT/CHECK/FIX warning logged when pdftotext is
// absent (spec §5.1 template). enrich.RegisterIfAvailable appends the
// LookPath error.
const pdfDisabledMsg = "enrich/pdf: pdftotext not found in PATH; the PDF enricher is disabled for this connector.\n" +
	"  CHECK: docker inspect <image> for /usr/bin/pdftotext\n" +
	"  FIX:   rebase this connector on glovebox-enricher-runtime, or accept that PDFs will not be text-extracted."

// registerIfAvailable is the testable core of init(): the shared binary-enricher
// gate (glovebox-afq4.14). Tests drive it directly with their own registry +
// LookPath + logger and never mutate $PATH or package-global state.
func registerIfAvailable(reg *enrich.Registry, look func(string) (string, error), logger *log.Logger) bool {
	return enrich.RegisterIfAvailable(reg, pdftotextBinary,
		func(string) enrich.Enricher { return &Enricher{} }, look, logger, pdfDisabledMsg)
}
