package random

import "math/rand"

// Bool returns a random boolean value based on the provided weight
// Higher weight increases the chance of returning true
func Bool(weight float64) bool {
	if weight < 0.0 || weight > 1.0 {
		weight = 0.8
	}
	return rand.Float64() < weight
}

// String returns a random string from the provided slice
func String(values []string) string {
	return values[rand.Intn(len(values))]
}

// Int returns a random integer within the specified range [min, max]
func Int(min, max int) int {
	return min + rand.Intn(max-min+1)
}

// Float returns a random float64 within the specified range [min, max]
func Float(min, max float64) float64 {
	return min + rand.Float64()*(max-min)
}
