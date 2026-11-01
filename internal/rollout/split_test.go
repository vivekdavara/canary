package rollout

import "testing"

// Every replicas count the API allows (1 to 1000) against every weight (1 to 100): 100,000 cases, checked against
// DESIGN.md's rules written out directly.
func TestSplitHoldsForEveryAllowedInput(t *testing.T) {
	for r := int32(1); r <= 1000; r++ {
		for w := int32(1); w <= 100; w++ {
			c, s := Split(r, w)
			// ceil(r*w/100), by its definition: the least c with 100*c >= r*w.
			if 100*c < r*w || 100*(c-1) >= r*w {
				t.Fatalf("Split(%d, %d) canary = %d; not ceil(%d * %d / 100)", r, w, c, r, w)
			}
			if c < 1 {
				t.Fatalf("Split(%d, %d) canary = %d; every step runs a canary pod", r, w, c)
			}
			switch {
			case w == 100:
				if s != 0 {
					t.Fatalf("Split(%d, 100) stable = %d; want 0", r, s)
				}
			case r-c >= 1:
				if s != r-c {
					t.Fatalf("Split(%d, %d) = %d, %d; want the stable to have the rest, %d", r, w, c, s, r-c)
				}
			default:
				// Below 100% and the canary took them all: the last stable pod stays.
				if s != 1 || c != r {
					t.Fatalf("Split(%d, %d) = %d, %d; want %d, 1", r, w, c, s, r)
				}
			}
		}
	}
}

func TestSplitExamples(t *testing.T) {
	for _, tc := range []struct{ r, w, c, s, weight int32 }{
		{10, 10, 1, 9, 10},
		{10, 50, 5, 5, 50},
		{10, 100, 10, 0, 100},
		{3, 10, 1, 2, 33}, // DESIGN.md's example: a 10% step of 3 pods is 33%
		{4, 25, 1, 3, 25},
		{4, 50, 2, 2, 50},
		{1, 10, 1, 1, 50}, // one pod: the canary is extra, so the rollout runs 2
		{1, 99, 1, 1, 50}, // still below 100%: the stable pod stays
		{2, 51, 2, 1, 66}, // ceil(1.02) = 2 takes both; the last stable pod stays
		{1000, 1, 10, 990, 1},
		{999, 1, 10, 989, 1}, // ceil(9.99)
	} {
		c, s := Split(tc.r, tc.w)
		if c != tc.c || s != tc.s {
			t.Errorf("Split(%d, %d) = %d, %d; want %d, %d", tc.r, tc.w, c, s, tc.c, tc.s)
		}
		if got := Weight(c, s); got != tc.weight {
			t.Errorf("Weight(%d, %d) = %d; want %d", c, s, got, tc.weight)
		}
	}
}

func TestWeight(t *testing.T) {
	for _, tc := range []struct{ c, s, want int32 }{
		{0, 0, 0},
		{0, 5, 0},
		{5, 0, 100},
		{1, 2, 33}, // rounded down
		{2, 1, 66},
		{1, 999, 0},
		{1000, 1000, 50},
	} {
		if got := Weight(tc.c, tc.s); got != tc.want {
			t.Errorf("Weight(%d, %d) = %d; want %d", tc.c, tc.s, got, tc.want)
		}
	}
}
