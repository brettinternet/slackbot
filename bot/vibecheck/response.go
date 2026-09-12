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

func randomResponse(passed bool, c Config) string {
	emojis := nonEmptyOrDefault(c.GoodReactions, goodEmojis)
	texts := nonEmptyOrDefault(c.GoodText, goodText)
	if !passed {
		emojis = nonEmptyOrDefault(c.BadReactions, badEmojis)
		texts = nonEmptyOrDefault(c.BadText, badText)
	}
	e := random.String(emojis)
	if e[0] != ':' {
		e = ":" + e
	}
	if e[len(e)-1] != ':' {
		e += ":"
	}
	t := random.String(texts)
	return fmt.Sprintf("%s %s %s %s %s %s %s", e, e, e, t, e, e, e)
}

func nonEmptyOrDefault(values, defaultValues []string) []string {
	nonEmpty := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" {
			nonEmpty = append(nonEmpty, value)
		}
	}
	if len(nonEmpty) == 0 {
		return defaultValues
	}
	return nonEmpty
}
