package atomicfile

// SidecarPath returns the conventional sidecar path for a given source file.
// By convention, sidecar sibling files (manifests, surveys, checkpoints) are
// placed alongside their source with the suffix appended verbatim:
//
//	SidecarPath("/data/takeout.mbox", ".survey.v1.json")
//	    => "/data/takeout.mbox.survey.v1.json"
//
// This helper exists so callers in importers/mbox (and any future importer)
// agree on the same sidecar-placement convention without each re-implementing
// string concatenation.
func SidecarPath(source, suffix string) string {
	return source + suffix
}
