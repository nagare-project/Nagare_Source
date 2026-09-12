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
