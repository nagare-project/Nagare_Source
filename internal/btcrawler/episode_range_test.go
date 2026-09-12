package btcrawler

import "testing"

func TestEpisodeRangeFromBatchTitles(t *testing.T) {
	cases := map[string][2]float64{
		"[诸神字幕组][排球少年!!][Haikyuu!!][BDRip][01-25全][简繁日文字幕][1080P][HEVC MKV]": {1, 25},
		"[VCB-Studio] Haikyuu!! [1-12 Fin][Ma10p_1080p]":                     {1, 12},
		"【极影字幕社】排球少年 01~13 合集":                                               {1, 13},
	}
	for title, want := range cases {
		low, high, ok := episodeRange(title)
		if !ok || low != want[0] || high != want[1] {
			t.Errorf("%q: got %v-%v ok=%v", title, low, high, ok)
		}
	}
	for _, title := range []string{"[Example] Show - 05 [1080p]", "[Sub] Show [1920x1080] [2024-2025]", "Show 1080-720"} {
		if _, _, ok := episodeRange(title); ok {
			t.Errorf("%q must not parse as a range", title)
		}
	}
}
