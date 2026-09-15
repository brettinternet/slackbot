package showerthought

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	goslack "github.com/slack-go/slack"
	"github.com/tmc/langchaingo/llms"
	"go.uber.org/zap"
	"slackbot.arpa/tools/random"
)

const (
	generationAttempts = 3
	candidateCount     = 3
	selectionAttempts  = 2
	maxRecentThoughts  = 20
	stableTemperature  = 0.9
	maxThoughtLength   = 280
	maxThoughtWords    = 30
)

// aiService is intentionally limited to the operation this feature needs. This
// keeps generation straightforward to test without constructing an OpenAI client.
type aiService interface {
	GenerateContent(context.Context, []llms.MessageContent, ...llms.CallOption) (*llms.ContentResponse, error)
}

type slackService interface {
	Client() *goslack.Client
	BotUserID() string
}

type FileConfig struct {
	Enabled            *bool `json:"enabled" yaml:"enabled"`
	BusinessHoursStart *int  `json:"business_hours_start" yaml:"business_hours_start"`
	BusinessHoursEnd   *int  `json:"business_hours_end" yaml:"business_hours_end"`
}

type Config struct {
	Enabled            bool
	NotifyChannel      string
	DataDir            string
	BusinessHoursStart int // hour in 24h local time (inclusive), default 9
	BusinessHoursEnd   int // hour in 24h local time (exclusive), default 17
}

type ShowerThought struct {
	log      *zap.Logger
	config   Config
	configMu sync.RWMutex
	slack    slackService
	ai       aiService
	stopCh   chan struct{}
	wakeCh   chan struct{}
	start    sync.Once
	stop     sync.Once

	historyMu sync.Mutex
	history   []string
}

func New(log *zap.Logger, c Config, s slackService, a aiService) *ShowerThought {
	c.BusinessHoursStart, c.BusinessHoursEnd = normalizeBusinessHours(c.BusinessHoursStart, c.BusinessHoursEnd)
	st := &ShowerThought{
		log:    log,
		config: c,
		slack:  s,
		ai:     a,
		stopCh: make(chan struct{}),
		wakeCh: make(chan struct{}, 1),
	}
	st.loadHistory()
	return st
}

func (st *ShowerThought) Start(ctx context.Context) error {
	st.start.Do(func() { go st.run(ctx) })
	return nil
}

// SetConfig atomically applies reloadable scheduler settings and wakes the worker
// so enabling, disabling, channel changes, and schedule changes take effect now.
func (st *ShowerThought) SetConfig(c Config) {
	c.BusinessHoursStart, c.BusinessHoursEnd = normalizeBusinessHours(c.BusinessHoursStart, c.BusinessHoursEnd)
	st.configMu.Lock()
	// History storage is opened at construction and therefore remains restart-required.
	c.DataDir = st.config.DataDir
	st.config = c
	st.configMu.Unlock()
	select {
	case st.wakeCh <- struct{}{}:
	default:
	}
}

func (st *ShowerThought) configSnapshot() Config {
	st.configMu.RLock()
	defer st.configMu.RUnlock()
	return st.config
}

func (st *ShowerThought) Stop(_ context.Context) error {
	st.stop.Do(func() { close(st.stopCh) })
	return nil
}

func (st *ShowerThought) run(ctx context.Context) {
	for {
		config := st.configSnapshot()
		if !config.Enabled || config.NotifyChannel == "" {
			select {
			case <-st.stopCh:
				return
			case <-ctx.Done():
				return
			case <-st.wakeCh:
				continue
			}
		}

		next := nextPostTime(config, time.Now())
		st.log.Info("Next shower thought scheduled", zap.Time("at", next))
		timer := time.NewTimer(time.Until(next))
		select {
		case <-st.stopCh:
			timer.Stop()
			return
		case <-ctx.Done():
			timer.Stop()
			return
		case <-st.wakeCh:
			timer.Stop()
			continue
		case <-timer.C:
			st.postShowerThoughtWithConfig(ctx, config)
		}
	}
}

// nextPostTime returns a random time within the configured business hours (Mon-Fri, local time)
// within the next 7 days, at least 1 hour from now.
func nextPostTime(config Config, now time.Time) time.Time {
	start := config.BusinessHoursStart
	end := config.BusinessHoursEnd

	var candidates []time.Time
	for d := range 7 {
		day := now.AddDate(0, 0, d)
		if day.Weekday() == time.Saturday || day.Weekday() == time.Sunday {
			continue
		}
		for h := start; h < end; h++ {
			minute := random.Int(0, 59)
			t := time.Date(day.Year(), day.Month(), day.Day(), h, minute, 0, 0, day.Location())
			if t.After(now.Add(time.Hour)) {
				candidates = append(candidates, t)
			}
		}
	}

	if len(candidates) > 0 {
		return candidates[random.Int(0, len(candidates)-1)]
	}

	for d := range 7 {
		t := now.AddDate(0, 0, d+1)
		if t.Weekday() != time.Saturday && t.Weekday() != time.Sunday {
			return time.Date(t.Year(), t.Month(), t.Day(), start, 0, 0, 0, t.Location())
		}
	}
	return now.AddDate(0, 0, 1)
}

func normalizeBusinessHours(start, end int) (int, int) {
	if start < 0 || start > 23 || end < 1 || end > 24 || start >= end {
		return 9, 17
	}
	return start, end
}

const systemPrompt = `You write original, workplace-safe shower thoughts for a Slack channel.
A strong thought has a concrete observation, a fresh angle or unexpected connection, and a concise payoff that makes someone pause, smile, or say "huh". Prefer specificity and clever reframing over generic wisdom. It must stand on its own and be suitable for coworkers.
Never use these anti-patterns: motivational or inspirational advice, tired "what if" hypotheticals, generic observations about Mondays/coffee/sleep, recycled internet jokes, insults, sexual content, politics, diagnoses, assumptions about someone's identity or private life, or filler such as "I was just thinking". Do not explain the joke.
Output only one thought in 1–2 sentences, without a title, preamble, numbering, markdown, or quotation marks.`

func (st *ShowerThought) postShowerThoughtWithConfig(ctx context.Context, config Config) {
	if !config.Enabled || config.NotifyChannel == "" {
		return
	}
	target := ""
	// The mention is supplied to the model as a placeholder so direct address is
	// part of the thought rather than an unrelated prefix added after generation.
	if random.Bool(0.40) {
		target = st.randomChannelMember(ctx, config.NotifyChannel)
	}

	var thought string
	var err error
	if target == "" {
		thought, err = st.generateShowerThought(ctx)
	} else {
		thought, err = st.generateShowerThoughtForTarget(ctx, target)
	}
	if err != nil {
		st.log.Error("Failed to generate shower thought", zap.Error(err))
		return
	}
	if target != "" {
		thought = strings.ReplaceAll(thought, "{{MEMBER}}", fmt.Sprintf("<@%s>", target))
	}
	if thought, err = validateThought(thought); err != nil {
		st.log.Error("Generated shower thought failed final validation", zap.Error(err))
		return
	}
	latest := st.configSnapshot()
	if !latest.Enabled || latest.NotifyChannel != config.NotifyChannel {
		return
	}

	_, _, err = st.slack.Client().PostMessageContext(
		ctx,
		config.NotifyChannel,
		goslack.MsgOptionText(thought, false),
		goslack.MsgOptionAsUser(true),
	)
	if err != nil {
		st.log.Error("Failed to post shower thought",
			zap.String("channel", config.NotifyChannel),
			zap.Error(err),
		)
		return
	}
	st.recordThought(thought)
	st.log.Info("Posted shower thought", zap.String("channel", config.NotifyChannel))
}

// randomChannelMember returns a random non-bot member of the notify channel, or empty string on failure.
func (st *ShowerThought) randomChannelMember(ctx context.Context, channel string) string {
	members, _, err := st.slack.Client().GetUsersInConversationContext(ctx,
		&goslack.GetUsersInConversationParameters{ChannelID: channel})
	if err != nil {
		st.log.Warn("Failed to fetch channel members for shower thought targeting", zap.Error(err))
		return ""
	}

	botID := st.slack.BotUserID()
	var eligible []string
	for _, id := range members {
		if id != botID {
			eligible = append(eligible, id)
		}
	}
	if len(eligible) == 0 {
		return ""
	}
	return eligible[random.Int(0, len(eligible)-1)]
}

func (st *ShowerThought) generateShowerThought(ctx context.Context) (string, error) {
	return st.generateShowerThoughtForTarget(ctx, "")
}

func (st *ShowerThought) generateShowerThoughtForTarget(ctx context.Context, target string) (string, error) {
	history := st.recentThoughts()
	addressInstruction := "Do not address or mention a particular person, and do not use the {{MEMBER}} placeholder."
	if target != "" {
		addressInstruction = "Address the coworker represented by the literal placeholder {{MEMBER}} exactly once in a quirky, friendly way. The direct address must feel integral to the thought, not like a generic thought with a name prepended. Never invent a personal trait or sensitive detail."
	}

	candidates := make([]string, 0, candidateCount)
	for candidate := 0; candidate < candidateCount; candidate++ {
		var thought string
		var err error
		for attempt := 0; attempt < generationAttempts; attempt++ {
			prompt := fmt.Sprintf("Create candidate %d of %d. %s\nRecent thoughts to avoid repeating:\n%s\nCandidates already created in this batch:\n%s", candidate+1, candidateCount, addressInstruction, formatHistory(history), formatHistory(candidates))
			resp, callErr := st.ai.GenerateContent(ctx, []llms.MessageContent{
				llms.TextParts(llms.ChatMessageTypeSystem, systemPrompt),
				llms.TextParts(llms.ChatMessageTypeHuman, prompt),
			}, llms.WithTemperature(stableTemperature), llms.WithMaxTokens(120), llms.WithFrequencyPenalty(0.8), llms.WithPresencePenalty(0.5))
			if callErr != nil {
				err = callErr
				continue
			}
			thought, err = parseThoughtResponse(resp)
			if err == nil {
				err = validateCandidate(thought, target != "", history, candidates)
			}
			if err == nil {
				break
			}
		}
		if err != nil {
			return "", fmt.Errorf("generate candidate %d: %w", candidate+1, err)
		}
		candidates = append(candidates, thought)
	}

	var candidateList strings.Builder
	for i, candidate := range candidates {
		fmt.Fprintf(&candidateList, "%d. %s\n", i+1, candidate)
	}
	selectionPrompt := fmt.Sprintf(`Choose the strongest candidate using originality, clarity, surprise, and shareability. Penalize anything resembling a recent thought. Reply with only a JSON object such as {"choice":2}; do not rewrite the thought.
Recent thoughts:
%s
Candidates:
%s`, formatHistory(history), candidateList.String())
	for attempt := 0; attempt < selectionAttempts; attempt++ {
		resp, err := st.ai.GenerateContent(ctx, []llms.MessageContent{
			llms.TextParts(llms.ChatMessageTypeSystem, selectorSystemPrompt),
			llms.TextParts(llms.ChatMessageTypeHuman, selectionPrompt),
		}, llms.WithTemperature(0), llms.WithMaxTokens(20))
		if err != nil {
			if attempt == selectionAttempts-1 {
				return "", fmt.Errorf("select candidate: %w", err)
			}
			continue
		}
		choice, err := parseSelection(resp, len(candidates))
		if err == nil {
			return candidates[choice-1], nil
		}
	}
	return "", errors.New("select candidate: invalid selector response")
}

func parseThoughtResponse(resp *llms.ContentResponse) (string, error) {
	if resp == nil || len(resp.Choices) == 0 {
		return "", errors.New("empty response from LLM")
	}
	var lastErr error
	for _, choice := range resp.Choices {
		if choice == nil {
			continue
		}
		raw := choice.Content
		var object struct {
			Thought string `json:"thought"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &object); err == nil && object.Thought != "" {
			raw = object.Thought
		}
		thought, err := validateThought(raw)
		if err == nil {
			return thought, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("empty response from LLM")
	}
	return "", lastErr
}

func validateCandidate(thought string, direct bool, history, batch []string) error {
	placeholderCount := strings.Count(thought, "{{MEMBER}}")
	if direct && placeholderCount != 1 {
		return errors.New("direct-address thought must contain {{MEMBER}} exactly once")
	}
	if !direct && placeholderCount != 0 {
		return errors.New("non-addressed thought contains {{MEMBER}}")
	}
	key := thoughtComparisonKey(thought)
	for _, previous := range append(append([]string(nil), history...), batch...) {
		if key == thoughtComparisonKey(previous) {
			return errors.New("thought duplicates a recent or current candidate")
		}
	}
	return nil
}

var slackMentionPattern = regexp.MustCompile(`<@[A-Z0-9]+>`)

func thoughtComparisonKey(thought string) string {
	thought = slackMentionPattern.ReplaceAllString(thought, "{{MEMBER}}")
	return strings.ToLower(strings.Join(strings.Fields(thought), " "))
}

func validateThought(raw string) (string, error) {
	thought := strings.TrimSpace(raw)
	thought = strings.TrimPrefix(thought, "```text")
	thought = strings.TrimPrefix(thought, "```")
	thought = strings.TrimSuffix(thought, "```")
	thought = strings.TrimSpace(thought)
	if len(thought) < 12 || len(thought) > maxThoughtLength || strings.ContainsAny(thought, "\r\n") {
		return "", fmt.Errorf("thought must be 12-%d characters on one line", maxThoughtLength)
	}
	if len(strings.Fields(thought)) > maxThoughtWords {
		return "", fmt.Errorf("thought must be at most %d words", maxThoughtWords)
	}
	if strings.HasPrefix(thought, "-") || strings.HasPrefix(thought, "*") || strings.HasPrefix(thought, "#") || strings.HasPrefix(strings.ToLower(thought), "here ") {
		return "", errors.New("thought contains a preamble or formatting")
	}
	if (strings.HasPrefix(thought, "\"") && strings.HasSuffix(thought, "\"")) ||
		(strings.HasPrefix(thought, "'") && strings.HasSuffix(thought, "'")) {
		return "", errors.New("thought is quoted")
	}
	if strings.Contains(strings.ToLower(thought), "as an ai") {
		return "", errors.New("thought contains model boilerplate")
	}
	return thought, nil
}

const selectorSystemPrompt = `You are a strict editor selecting one shower thought from candidates. Judge originality, clarity, surprise, shareability, and workplace safety. Reply only with a JSON object containing the selected candidate number, such as {"choice":2}.`

var choicePattern = regexp.MustCompile(`(?i)\b(?:choice|candidate|select(?:ed|ion)?)?\s*[:#-]?\s*([1-9][0-9]*)\b`)

func parseSelection(resp *llms.ContentResponse, count int) (int, error) {
	if resp == nil || len(resp.Choices) == 0 {
		return 0, errors.New("empty selector response")
	}
	text := strings.TrimSpace(resp.Choices[0].Content)
	var object struct {
		Choice int `json:"choice"`
	}
	if err := json.Unmarshal([]byte(text), &object); err == nil && object.Choice >= 1 && object.Choice <= count {
		return object.Choice, nil
	}
	match := choicePattern.FindStringSubmatch(text)
	if len(match) == 2 {
		choice, _ := strconv.Atoi(match[1])
		if choice >= 1 && choice <= count {
			return choice, nil
		}
	}
	return 0, fmt.Errorf("selector did not choose 1-%d", count)
}

func formatHistory(history []string) string {
	if len(history) == 0 {
		return "(none)"
	}
	return strings.Join(history, "\n- ")
}

func (st *ShowerThought) recentThoughts() []string {
	st.historyMu.Lock()
	defer st.historyMu.Unlock()
	return append([]string(nil), st.history...)
}

func (st *ShowerThought) loadHistory() {
	config := st.configSnapshot()
	if config.DataDir == "" {
		return
	}
	data, err := os.ReadFile(filepath.Join(config.DataDir, "showerthoughts.json")) // #nosec G304 -- configured data directory
	if err != nil {
		return
	}
	var history []string
	if err := json.Unmarshal(data, &history); err != nil {
		st.log.Warn("Ignoring corrupt shower thought history", zap.Error(err))
		return
	}
	for _, thought := range history {
		if valid, err := validateThought(thought); err == nil {
			st.history = append(st.history, valid)
		}
	}
	if len(st.history) > maxRecentThoughts {
		st.history = st.history[len(st.history)-maxRecentThoughts:]
	}
}

func (st *ShowerThought) recordThought(thought string) {
	config := st.configSnapshot()
	st.historyMu.Lock()
	defer st.historyMu.Unlock()
	st.history = append(st.history, thought)
	if len(st.history) > maxRecentThoughts {
		st.history = st.history[len(st.history)-maxRecentThoughts:]
	}
	if config.DataDir == "" {
		return
	}
	if err := os.MkdirAll(config.DataDir, 0750); err != nil {
		st.log.Warn("Failed to create shower thought data directory", zap.Error(err))
		return
	}
	data, err := json.MarshalIndent(st.history, "", "  ")
	if err != nil {
		st.log.Warn("Failed to encode shower thought history", zap.Error(err))
		return
	}
	path := filepath.Join(config.DataDir, "showerthoughts.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil { // #nosec G304 -- configured data directory
		st.log.Warn("Failed to write shower thought history", zap.Error(err))
		return
	}
	if err := os.Rename(tmp, path); err != nil { // #nosec G304 -- configured data directory
		st.log.Warn("Failed to replace shower thought history", zap.Error(err))
	}
}
