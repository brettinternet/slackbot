package vibecheck

import (
	"fmt"

	"slackbot.arpa/tools/random"
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

func randomResponse(passed bool, c FileConfig) string {
	emojis := withDefault(c.GoodReactions, goodEmojis)
	texts := withDefault(c.GoodText, goodText)
	if !passed {
		emojis = withDefault(c.BadReactions, badEmojis)
		texts = withDefault(c.BadText, badText)
	}
	e := random.String(emojis)
	if e[0] != ':' {
		e = ":" + e
	}
	if e[len(e)-1] != ':' {
		e = e + ":"
	}
	t := random.String(texts)
	return fmt.Sprintf("%s %s %s %s %s %s %s", e, e, e, t, e, e, e)
}

func withDefault(values, defaultValues []string) []string {
	if len(values) == 0 {
		return defaultValues
	}
	return values
}
