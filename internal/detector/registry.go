package detector

import "github.com/leftathome/glovebox/internal/engine"

type Detector interface {
	Detect(content []byte) ([]engine.Signal, error)
}

// SampledDetector is implemented by detectors whose answer is a property
// of the document as a whole, so a prefix+suffix sample represents it
// faithfully. Everything else receives the full content.
//
// This is opt-in on purpose. Sampling used to be applied to every
// detector, which meant an injection padded past the first 64 KB and
// before the last 64 KB was invisible to the detectors that look for
// payloads. A detector must now say that positioning cannot hide anything
// from it before it gives up seeing the whole document.
type SampledDetector interface {
	Detector
	// AllowSampling reports that this detector may run on a
	// representative sample rather than the entire content.
	AllowSampling() bool
}

type Registry struct {
	detectors map[string]Detector
}

func NewRegistry() *Registry {
	return &Registry{detectors: make(map[string]Detector)}
}

func (r *Registry) Register(name string, d Detector) {
	r.detectors[name] = d
}

func (r *Registry) Get(name string) (Detector, bool) {
	d, ok := r.detectors[name]
	return d, ok
}
