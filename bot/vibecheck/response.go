package vibecheck

import (
	"fmt"
	"math/rand"
	"time"
)

var goodEmojis = []string{
	":ok:",
}

var badEmojis = []string{
	":no_entry:",
}

var goodText = []string{
	"V I B E C H E C K - P A S S E D",
}

var badText = []string{
	"V I B E C H E C K - F A I L E D",
}

func randomString(values []string) string {
	rand.New(rand.NewSource(time.Now().UnixNano()))
	return values[rand.Intn(len(values))]
}

func randomBool(weight float64) bool {
	if weight < 0.0 || weight > 1.0 {
		weight = 0.8
	}
	rand.New(rand.NewSource(time.Now().UnixNano()))
	return rand.Float64() < weight
}

func randomResponse(passed bool, c FileConfig) string {
	emojis := withDefault(c.GoodReactions, goodEmojis)
	texts := withDefault(c.GoodText, goodEmojis)
	if !passed {
		emojis = withDefault(c.BadReactions, badEmojis)
		texts = withDefault(c.BadText, badText)
	}
	e := randomString(emojis)
	t := randomString(texts)
	return fmt.Sprintf("%s %s %s %s %s %s %s", e, e, e, t, e, e, e)
}

func withDefault(values, defaultValues []string) []string {
	if len(values) == 0 {
		return defaultValues
	}
	return values
}
