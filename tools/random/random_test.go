package random

import (
	"math"
	"slices"
	"testing"
)

func TestBool(t *testing.T) {
	tests := []struct {
		name     string
		weight   float64
		expected float64
	}{
		{"weight 0.0", 0.0, 0.0},
		{"weight 0.5", 0.5, 0.5},
		{"weight 1.0", 1.0, 1.0},
		{"weight -0.1", -0.1, 0.8},
		{"weight 1.1", 1.1, 0.8},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			trueCount := 0
			iterations := 10000

			for range iterations {
				if Bool(tt.weight) {
					trueCount++
				}
			}

			ratio := float64(trueCount) / float64(iterations)
			tolerance := 0.05

			if math.Abs(ratio-tt.expected) > tolerance {
				t.Errorf("Bool(%f) ratio = %f, expected ~%f (tolerance: %f)", tt.weight, ratio, tt.expected, tolerance)
			}
		})
	}
}

func TestString(t *testing.T) {
	values := []string{"apple", "banana", "cherry"}

	for range 100 {
		result := String(values)
		found := slices.Contains(values, result)
		if !found {
			t.Errorf("String(%v) returned %s, which is not in the input slice", values, result)
		}
	}
}

func TestStringEmpty(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("String with empty slice should panic")
		}
	}()

	var empty []string
	String(empty)
}

func TestInt(t *testing.T) {
	tests := []struct {
		name string
		min  int
		max  int
	}{
		{"positive range", 1, 10},
		{"negative range", -10, -1},
		{"mixed range", -5, 5},
		{"single value", 5, 5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for range 100 {
				result := Int(tt.min, tt.max)
				if result < tt.min || result > tt.max {
					t.Errorf("Int(%d, %d) = %d, want value between %d and %d", tt.min, tt.max, result, tt.min, tt.max)
				}
			}
		})
	}
}

func TestFloat(t *testing.T) {
	tests := []struct {
		name string
		min  float64
		max  float64
	}{
		{"positive range", 1.0, 10.0},
		{"negative range", -10.0, -1.0},
		{"mixed range", -5.0, 5.0},
		{"small range", 0.1, 0.2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for range 100 {
				result := Float(tt.min, tt.max)
				if result < tt.min || result > tt.max {
					t.Errorf("Float(%f, %f) = %f, want value between %f and %f", tt.min, tt.max, result, tt.min, tt.max)
				}
			}
		})
	}
}

func TestFloatSameValue(t *testing.T) {
	min, max := 5.0, 5.0
	result := Float(min, max)
	if result != min {
		t.Errorf("Float(%f, %f) = %f, want %f", min, max, result, min)
	}
}

func BenchmarkBool(b *testing.B) {
	for b.Loop() {
		Bool(0.5)
	}
}

func BenchmarkString(b *testing.B) {
	values := []string{"a", "b", "c", "d", "e"}

	for b.Loop() {
		String(values)
	}
}

func BenchmarkInt(b *testing.B) {
	for b.Loop() {
		Int(1, 100)
	}
}

func BenchmarkFloat(b *testing.B) {
	for b.Loop() {
		Float(0.0, 1.0)
	}
}
