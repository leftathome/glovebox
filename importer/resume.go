package importer

// ResumeAction is the outcome of the resume decision table in
// docs/specs/09-mbox-importer-design.md §3.1.1. RunOneShot uses it
// to decide whether to start a fresh import, pick up where the
// previous run left off, exit immediately, or refuse to proceed
// without an explicit operator override.
type ResumeAction int

const (
	// StartFresh discards any existing manifest/checkpoint and
	// begins a fresh import from byte offset 0.
	StartFresh ResumeAction = iota

	// Resume continues from the existing checkpoint's byte offset
	// and preserves the manifest's Message-ID dedup set.
	Resume

	// ExitComplete means the archive has already been imported to
	// completion; RunOneShot returns nil without work.
	ExitComplete

	// RequireExplicitResume means the previous run hit a terminal
	// error worth investigating; RunOneShot returns an error
	// unless the caller passes --resume=true.
	RequireExplicitResume
)

// String returns a short human-readable label for logs.
func (a ResumeAction) String() string {
	switch a {
	case StartFresh:
		return "start-fresh"
	case Resume:
		return "resume"
	case ExitComplete:
		return "exit-complete"
	case RequireExplicitResume:
		return "require-explicit-resume"
	default:
		return "unknown"
	}
}

// ResumeDecision is the output of Decide. Importer callers use the
// Action to branch and the ByteOffset / PreservedMessageIDs fields
// to seed a resumed run when Action == Resume.
type ResumeDecision struct {
	Action              ResumeAction
	ByteOffset          int64
	PreservedMessageIDs []string
}

// Decide implements the pure-function resume table from spec §3.1.1.
//
// manifestStatus is one of "", "in_progress", "complete", "interrupted",
// "failed" -- "" signals an absent manifest. checkpointExists reports
// whether a checkpoint file sits next to the source. resumeOverride is
// the CLI --resume flag: nil means "do whatever the rule says",
// *false forces fresh start, *true forces resume semantics (including
// for failed status).
//
// Note on ByteOffset / PreservedMessageIDs: this function is concerned
// only with the Action. The concrete offset and dedup set live in the
// format-specific manifest/checkpoint files and are looked up by
// RunOneShot after Decide returns Resume.
func Decide(manifestStatus string, checkpointExists bool, resumeOverride *bool) ResumeDecision {
	// Explicit --resume=false: per spec §3.1.1, "forces a fresh start
	// (deletes existing manifest and checkpoint)" -- overrides any
	// status, including complete. An operator who passes --resume=false
	// is saying "re-do this from zero."
	if resumeOverride != nil && !*resumeOverride {
		return ResumeDecision{Action: StartFresh}
	}

	// Complete always wins before any other resume semantics.
	if manifestStatus == "complete" {
		return ResumeDecision{Action: ExitComplete}
	}

	// Explicit --resume=true: force resume if we have something to
	// resume from; otherwise fall back to fresh.
	if resumeOverride != nil && *resumeOverride {
		if checkpointExists {
			return ResumeDecision{Action: Resume}
		}
		return ResumeDecision{Action: StartFresh}
	}

	// No override: apply the default table.
	switch manifestStatus {
	case "interrupted":
		if checkpointExists {
			return ResumeDecision{Action: Resume}
		}
		return ResumeDecision{Action: StartFresh}
	case "failed":
		return ResumeDecision{Action: RequireExplicitResume}
	case "in_progress", "":
		return ResumeDecision{Action: StartFresh}
	default:
		// Unknown status -- treat conservatively as "previous run died".
		return ResumeDecision{Action: StartFresh}
	}
}
