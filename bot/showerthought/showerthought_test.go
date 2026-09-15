package showerthought

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/tmc/langchaingo/llms"
	"go.uber.org/zap"
)

type testAI struct {
	responses []*llms.ContentResponse
	prompts   []string
}

func (a *testAI) GenerateContent(_ context.Context, messages []llms.MessageContent, _ ...llms.CallOption) (*llms.ContentResponse, error) {
	if len(messages) > 1 {
		for _, part := range messages[1].Parts {
			if text, ok := part.(llms.TextContent); ok {
				a.prompts = append(a.prompts, text.Text)
			}
		}
	}
	response := a.responses[0]
	a.responses = a.responses[1:]
	return response, nil
}

func content(text string) *llms.ContentResponse {
	return &llms.ContentResponse{Choices: []*llms.ContentChoice{{Content: text}}}
}

func TestValidateThought(t *testing.T) {
	if got, err := validateThought("A calendar is just a to-do list with better public relations."); err != nil || got == "" {
		t.Fatalf("validateThought() = %q, %v", got, err)
	}
	for _, input := range []string{"", "- a formatted list", "Here is a thought", "\"quoted thought\"", strings.Repeat("x", maxThoughtLength+1), strings.Repeat("word ", maxThoughtWords+1)} {
		if _, err := validateThought(input); err == nil {
			t.Errorf("validateThought(%q) accepted invalid input", input)
		}
	}
}

func TestParseSelection(t *testing.T) {
	for _, test := range []struct {
		response string
		want     int
	}{
		{`{"choice":2}`, 2},
		{"Candidate: 3", 3},
	} {
		got, err := parseSelection(content(test.response), 3)
		if err != nil || got != test.want {
			t.Errorf("parseSelection(%q) = %d, %v; want %d", test.response, got, err, test.want)
		}
	}
	if _, err := parseSelection(content(`{"choice":4}`), 3); err == nil {
		t.Error("parseSelection accepted out-of-range choice")
	}
}

func TestGenerateRetriesAndUsesDirectAddressPlaceholder(t *testing.T) {
	model := &testAI{responses: []*llms.ContentResponse{
		content("bad\nformat"), content("{{MEMBER}}, a fresh first candidate appears."),
		content("{{MEMBER}}, a second candidate turns routine into surprise."), content("{{MEMBER}}, a third candidate notices a useful contradiction."),
		content(`{"choice":2}`),
	}}
	st := New(zap.NewNop(), Config{}, nil, model)
	thought, err := st.generateShowerThoughtForTarget(context.Background(), "U123")
	if err != nil {
		t.Fatal(err)
	}
	if thought != "{{MEMBER}}, a second candidate turns routine into surprise." {
		t.Fatalf("selected thought = %q", thought)
	}
	foundPlaceholder := false
	for _, prompt := range model.prompts {
		if strings.Contains(prompt, "{{MEMBER}}") {
			foundPlaceholder = true
		}
	}
	if !foundPlaceholder {
		t.Error("direct-address prompt omitted member placeholder")
	}
}

func TestValidateCandidateRequiresCorrectAddressingAndRejectsDuplicates(t *testing.T) {
	if err := validateCandidate("A generic thought without a target.", true, nil, nil); err == nil {
		t.Error("targeted candidate without placeholder was accepted")
	}
	if err := validateCandidate("{{MEMBER}}, calendars are public to-do lists.", false, nil, nil); err == nil {
		t.Error("untargeted candidate with placeholder was accepted")
	}
	if err := validateCandidate("{{MEMBER}}, calendars are public to-do lists.", true, []string{"<@U123>, calendars are public to-do lists."}, nil); err == nil {
		t.Error("duplicate targeted candidate was accepted")
	}
}

func TestHistoryIsBoundedAndPersists(t *testing.T) {
	dir := t.TempDir()
	st := New(zap.NewNop(), Config{DataDir: dir}, nil, nil)
	for i := 0; i < maxRecentThoughts+3; i++ {
		st.recordThought("A distinct thought number " + string(rune('a'+i)))
	}
	if got := len(st.recentThoughts()); got != maxRecentThoughts {
		t.Fatalf("history length = %d, want %d", got, maxRecentThoughts)
	}
	loaded := New(zap.NewNop(), Config{DataDir: dir}, nil, nil)
	if got := len(loaded.recentThoughts()); got != maxRecentThoughts {
		t.Fatalf("loaded history length = %d, want %d", got, maxRecentThoughts)
	}
	if _, err := os.Stat(dir + "/showerthoughts.json"); err != nil {
		t.Fatalf("history file missing: %v", err)
	}
}

func TestSetConfigEnablesDisablesAndReschedules(t *testing.T) {
	st := New(zap.NewNop(), Config{Enabled: false}, nil, nil)
	st.SetConfig(Config{
		Enabled:            true,
		NotifyChannel:      "C123",
		BusinessHoursStart: 8,
		BusinessHoursEnd:   16,
	})
	got := st.configSnapshot()
	if !got.Enabled || got.NotifyChannel != "C123" || got.BusinessHoursStart != 8 || got.BusinessHoursEnd != 16 {
		t.Fatalf("reloaded config = %#v", got)
	}
	select {
	case <-st.wakeCh:
	default:
		t.Fatal("config reload did not wake scheduler")
	}

	st.SetConfig(Config{Enabled: false, BusinessHoursStart: 8, BusinessHoursEnd: 16})
	if st.configSnapshot().Enabled {
		t.Fatal("disabled config remained enabled")
	}
}

func TestStopIsIdempotent(t *testing.T) {
	st := New(zap.NewNop(), Config{}, nil, nil)
	if err := st.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := st.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestNormalizeBusinessHours(t *testing.T) {
	if start, end := normalizeBusinessHours(25, 2); start != 9 || end != 17 {
		t.Fatalf("invalid hours normalized to %d-%d", start, end)
	}
	if start, end := normalizeBusinessHours(8, 24); start != 8 || end != 24 {
		t.Fatalf("valid hours changed to %d-%d", start, end)
	}
}
