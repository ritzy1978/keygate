package model

import "testing"

// A plan sold per seat holds the seats bought and its per-seat devices
// times them; any other plan holds its own limits whatever the
// quantity. A licence without its plan loaded holds no seat limit and
// three devices, as before plans had limits.
func TestLimits(t *testing.T) {
	for _, tc := range []struct {
		name               string
		plan               *Plan
		quantity           int
		seats, activations int
	}{
		{"per licence ignores quantity", &Plan{MaxSeats: 5, MaxActivations: 3}, 7, 5, 3},
		{"per seat", &Plan{PerSeat: true, MaxSeats: 1, MaxActivations: 2}, 7, 7, 14},
		{"per seat, no devices", &Plan{PerSeat: true, MaxSeats: 1}, 4, 4, 0},
		{"per seat, quantity below one counts one", &Plan{PerSeat: true, MaxActivations: 3}, 0, 1, 3},
	} {
		if s, a := tc.plan.Limits(tc.quantity); s != tc.seats || a != tc.activations {
			t.Errorf("%s: limits = %d, %d; want %d, %d", tc.name, s, a, tc.seats, tc.activations)
		}
	}
	lic := &License{Quantity: 9}
	if s, a := lic.Limits(); s != 0 || a != 3 {
		t.Errorf("licence without a plan: %d, %d; want 0, 3", s, a)
	}
	lic.Plan = &Plan{PerSeat: true, MaxActivations: 2}
	FillLimits(lic, nil)
	if lic.SeatLimit == nil || *lic.SeatLimit != 9 || *lic.ActivationLimit != 18 {
		t.Errorf("filled limits = %v, %v; want 9 and 18", lic.SeatLimit, lic.ActivationLimit)
	}
}
