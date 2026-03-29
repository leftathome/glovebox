package detector

func NewDefaultRegistry() *Registry {
	r := NewRegistry()
	r.Register("encoding_anomaly", EncodingAnomalyDetector{})
	r.Register("template_structure", TemplateStructureDetector{})
	r.Register("language_detection", NewLanguageDetectionDetector())
	return r
}
