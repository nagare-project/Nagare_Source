package btcrawler

import "testing"

// 常见发布组的集号写法都要解得出来；解不出的条目会被 require_episode 丢掉，用户看到的就是「没资源」。
func TestParseEpisodeCoversCommonReleaseStyles(t *testing.T) {
	cases := map[string]string{
		"[Nix-Raws] 幼女战记Ⅱ / Youjo Senki S02E10 [CR WEB-DL 1080p AVC AAC]":          "10",
		"[LoliHouse] 幼女战记II / Youjo Senki II - 05 [WebRip 1080p HEVC-10bit AAC]":   "5",
		"[YYQ字幕组][幼女战记 第二季 / Youjo Senki S2][10][1080P][简日双语][MP4]":                "10",
		"[ANi] Youjo Senki / 幼女戰記 2 - 10 [1080P][Baha][WEB-DL][AAC AVC][CHT][MP4]": "10",
		"【喵萌奶茶屋】★07月新番★[幼女战记 第二季][05][1080p][简日双语]":                                "5",
		"葬送的芙莉莲 第03话 1080p":                                                        "3",
		"Frieren EP07 [1080p]":                                                     "7",
	}
	for title, want := range cases {
		got, err := parseEpisode(title)
		if err != nil || got != want {
			t.Errorf("%q: got %q err=%v, want %q", title, got, err, want)
		}
	}
	if _, err := parseEpisode("幼女战记 第二季 全集合集 [1080P]"); err == nil {
		t.Error("合集标题不该解出集号（1080 不是集号）")
	}
}

// 修订版（v2/v3）是字幕组补发时的常见写法：[01v2] 解不出集号，整条就被 require_episode 丢掉。
func TestParseEpisodeAcceptsVersionSuffix(t *testing.T) {
	cases := []struct {
		title string
		want  string
	}{
		{"[绿茶字幕组] 描绘直至生命尽头 / Kore Kaite Shine [01v2][WebRip][1080p][简日内嵌]", "1"},
		{"[Group] Show [12V3][1080p]", "12"},
		{"[Group] Show - 05v3 [1080p]", "5"},
		{"[Group] Show - 05v2", "5"},
		{"[Group] Show [ 07v2 ]", "7"},
	}
	for _, tc := range cases {
		got, err := parseEpisode(tc.title)
		if err != nil || got != tc.want {
			t.Errorf("%q: got %q err=%v, want %q", tc.title, got, err, tc.want)
		}
	}
}

// 分辨率、区间、版本号以外的字母后缀都不能被当成单集号：区间交给 episodeRange 处理。
func TestParseEpisodeRejectsNonEpisodeBrackets(t *testing.T) {
	for _, title := range []string{
		"[Group] Show [1080p]",
		"[Group] Show [01-12][1080p]",
		"[Group] Show [01-12v2][1080p]",
		"[Group] Show [2v2b]",
		"[Group] Show [720P][HEVC]",
	} {
		if got, err := parseEpisode(title); err == nil {
			t.Errorf("%q must not parse as a single episode, got %q", title, got)
		}
	}
}
