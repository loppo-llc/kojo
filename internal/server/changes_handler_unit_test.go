package server

import (
	"errors"
	"testing"
)

func TestParseSinceParam(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"", 0, false},
		{"0", 0, false},
		{"123", 123, false},
		{"-1", 0, true},
		{"abc", 0, true},
		{"9999999999999", 9999999999999, false},
	}
	for _, tc := range cases {
		got, err := parseSinceParam(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("%q: err=%v wantErr=%v", tc.in, err, tc.wantErr)
			continue
		}
		if !tc.wantErr && got != tc.want {
			t.Errorf("%q: got %d want %d", tc.in, got, tc.want)
		}
		if tc.wantErr && err != nil && !errors.Is(err, errInvalidSince) {
			t.Errorf("%q: err = %v, want errInvalidSince", tc.in, err)
		}
	}
}

func TestParseLimitParam(t *testing.T) {
	cases := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{"", 0, false},
		{"1", 1, false},
		{"500", 500, false},
		{"5000", 5000, false},
		{"5001", 0, true},
		{"0", 0, true},
		{"-3", 0, true},
		{"abc", 0, true},
	}
	for _, tc := range cases {
		got, err := parseLimitParam(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("%q: err=%v wantErr=%v", tc.in, err, tc.wantErr)
			continue
		}
		if !tc.wantErr && got != tc.want {
			t.Errorf("%q: got %d want %d", tc.in, got, tc.want)
		}
	}
}
