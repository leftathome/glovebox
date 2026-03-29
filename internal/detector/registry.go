package detector

import "github.com/leftathome/glovebox/internal/engine"

type Detector interface {
	Detect(content []byte) ([]engine.Signal, error)
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
