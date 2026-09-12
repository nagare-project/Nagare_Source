package btcrawler

import "testing"

func TestParseDateTimeAssumesOffsetForNaiveTimestamps(t *testing.T) {
	cases := []struct{ value, offset, want string }{
		{"2026-09-10T10:45:00", "+08:00", "2026-09-10T02:45:00Z"}, // Mikan：北京时间裸串
		{"2026-09-10T10:45:00", "", "2026-09-10T10:45:00Z"},       // 缺省当 UTC
		{"Wed, 09 Sep 2026 19:10:20 -0700", "+08:00", "2026-09-10T02:10:20Z"},
		{"2026-09-10T10:45:00+08:00", "", "2026-09-10T02:45:00Z"},
	}
	for _, tc := range cases {
		got, err := parseDateTime(tc.value, "", tc.offset)
		if err != nil || got != tc.want {
			t.Errorf("%q offset=%q: got %q err=%v, want %q", tc.value, tc.offset, got, err, tc.want)
		}
	}
	if _, err := parseDateTime("2026-09-10T10:45:00", "", "bogus"); err == nil {
		t.Error("bogus assume_offset must fail")
	}
}
