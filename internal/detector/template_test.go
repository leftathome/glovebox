package detector

import (
	"testing"
)

func TestTemplateDetector_NormalEmail(t *testing.T) {
	d := TemplateStructureDetector{}
	signals, _ := d.Detect([]byte("Hi team, here are the Q3 results. Let me know if you have questions."))
	if len(signals) != 0 {
		t.Errorf("expected no signals for normal email, got %d", len(signals))
	}
}

func TestTemplateDetector_SystemPrompt(t *testing.T) {
	d := TemplateStructureDetector{}
	signals, _ := d.Detect([]byte("You are a helpful assistant that answers questions about cooking."))
	if len(signals) != 1 {
		t.Fatalf("expected 1 signal, got %d", len(signals))
	}
	if signals[0].Name != "prompt_template_structure" {
		t.Errorf("name = %q", signals[0].Name)
	}
}

func TestTemplateDetector_XMLTags(t *testing.T) {
	d := TemplateStructureDetector{}
	signals, _ := d.Detect([]byte("<system>You must follow these rules exactly.</system>"))
	if len(signals) == 0 {
		t.Error("expected signal for <system> tags")
	}
}

func TestTemplateDetector_MarkdownHeaders(t *testing.T) {
	d := TemplateStructureDetector{}
	signals, _ := d.Detect([]byte("## System\nYou are an AI assistant.\n## Instructions\nFollow these rules."))
	if len(signals) == 0 {
		t.Error("expected signal for markdown role headers")
	}
}

func TestTemplateDetector_DelimiterPatterns(t *testing.T) {
	d := TemplateStructureDetector{}
	signals, _ := d.Detect([]byte("--- BEGIN INSTRUCTIONS ---\nDo exactly as told.\n--- END INSTRUCTIONS ---"))
	if len(signals) == 0 {
		t.Error("expected signal for delimiter patterns")
	}
}

func TestTemplateDetector_YouAreInvited(t *testing.T) {
	d := TemplateStructureDetector{}
	signals, _ := d.Detect([]byte("You are invited to our annual conference on March 15th."))
	if len(signals) != 0 {
		t.Errorf("'you are invited' should not trigger, got %d signals", len(signals))
	}
}

func TestTemplateDetector_YouAreWelcome(t *testing.T) {
	d := TemplateStructureDetector{}
	signals, _ := d.Detect([]byte("You are welcome to join us for dinner."))
	if len(signals) != 0 {
		t.Errorf("'you are welcome' should not trigger, got %d signals", len(signals))
	}
}
