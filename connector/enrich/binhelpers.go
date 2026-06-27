package enrich

import "log"

// RegisterIfAvailable is the shared init-time gate for binary-dependent
// enrichers (pdf/ocr/office). Given a registry, the external binary the
// enricher needs, a constructor, a LookPath function, a logger, and the
// enricher's WHAT/CHECK/FIX "disabled" message, it either:
//
//   - resolves the binary, registers make(resolvedPath), and returns true; or
//   - logs disabledMsg (with the LookPath error appended) and returns false.
//
// Standardizing the three binary enrichers on this one helper (glovebox-afq4.14)
// keeps the init seam identical and parallel-safe: tests call RegisterIfAvailable
// directly with their own registry + LookPath + logger and never mutate $PATH or
// package-global state. make receives the resolved absolute path for enrichers
// that need it (office captures pandoc's path); enrichers that don't can ignore
// the argument.
func RegisterIfAvailable(
	reg *Registry,
	binary string,
	make func(resolvedPath string) Enricher,
	look func(string) (string, error),
	logger *log.Logger,
	disabledMsg string,
) bool {
	path, err := look(binary)
	if err != nil {
		logger.Printf("%s\n  (LookPath error: %v)", disabledMsg, err)
		return false
	}
	reg.Register(make(path))
	return true
}
