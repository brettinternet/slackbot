package vibecheck

import (
	"fmt"
	"math/rand"
	"time"
)

var goodEmojis = []string{
	":letsgo:",
	":ok:",
	":gigachad:",
	":white_check_mark:",
	":letsgo:",
	":sniffa:",
	":catjam:",
	":bedge:",
}

var badEmojis = []string{
	":diesofcringe:",
	":no_entry:",
	":alert:",
	":sad:",
	":cry:",
	":sadge:",
	":vibecheck:",
}

var goodText = []string{
	"V I B E C H E C K - P A S S E D",
	"V I B E C H E C K - P A S S E D",
	"V I B E C H E C K - P A S S E D",
	"V I B E C H E C K - P A S S E D",
	"V I B E C H E C K - F L A W L E S S",
	"V I B E C H E C K - P A S S",
	"V I B E - I S - G O O D",
	"L G T M",
	":ok:V:ok:I:ok:B:ok:E:ok:",
	"vibes are :ok:",
	":eye: :biting_lip: :eye:",
	"h e l l o   v i b e   i ' m   b o t",
}

var badText = []string{
	"V I B E C H E C K - F A I L E D",
	"V I B E C H E C K - F A I L E D",
	"V I B E C H E C K - F A I L E D",
	"BAD!",
	"really really not great",
	"noooooooo",
	"V I B E C H E C K - F A I L E D :no_entry:",
	"nah, not the vibe",
	"V I B E  T A R I F F  U N P A I D",
	":middle_finger:",
}

func goodResponse() string {
	e := randomString(goodEmojis)
	t := randomString(goodText)
	return fmt.Sprintf("%s %s %s %s %s %s %s", e, e, e, t, e, e, e)
}

func badResponse() string {
	e := randomString(badEmojis)
	t := randomString(badText)
	return fmt.Sprintf("%s %s %s %s %s %s %s", e, e, e, t, e, e, e)
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

func randomResponse(passed bool) string {
	if passed {
		return goodResponse()
	}
	return badResponse()
}
