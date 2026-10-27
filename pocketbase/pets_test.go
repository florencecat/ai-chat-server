package pocketbase

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestScheduleTimes(t *testing.T) {
	cases := map[string][]string{
		`["16:00"]`:                           {"16:00"},
		`[{"time":"22:00"},{"time":"12:00"}]`: {"12:00", "22:00"},
		`["2026-07-06T08:30:00+03:00"]`:       {"08:30"},
		`[{"hour":"9:05"}, "9:05"]`:           {"09:05"},
		`null`:                                nil,
	}
	for in, want := range cases {
		if got := scheduleTimes(json.RawMessage(in)); !reflect.DeepEqual(got, want) {
			t.Errorf("scheduleTimes(%s) = %v, want %v", in, got, want)
		}
	}
}
