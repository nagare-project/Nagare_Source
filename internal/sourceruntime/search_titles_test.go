package sourceruntime

import (
	"context"
	"net/url"
	"reflect"
	"testing"

	"github.com/nagare-project/Nagare_Source/internal/btcrawler"
)

func TestSearchTitleVariantsPreserveWordsAndLimits(t *testing.T) {
	titles := []string{"幼女战记 第二季", "幼女战记第二季", "Saga of Tanya the Evil Season 2"}
	want := []string{"幼女战记第二季", "幼女战记 第二季", "幼女战记2", "Saga of Tanya the Evil Season 2"}
	if got := searchTitles(nil, titles); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got)
	}
	source := map[string]any{"matching": map[string]any{"search_title_limit": 1}}
	if got := searchTitles(source, titles); !reflect.DeepEqual(got, want[:1]) {
		t.Fatalf("limit ignored: %q", got)
	}
	if got := compactCJKSpaces("作品 \t 第二季 English Title"); got != "作品第二季 English Title" {
		t.Fatalf("got %q", got)
	}
}

func TestNumberedSeasonAliasIsExactAndCannotSelectMovieOrFirstSeason(t *testing.T) {
	subject := Subject{Titles: []string{"幼女战记 第二季"}}
	for _, title := range []string{"幼女战记", "幼女战记3", "幼女战记2剧场版", "迷你幼女战记2", "幼女战记20"} {
		if got, _, _ := chooseSubject([]map[string]string{{"title": title}}, subject); got != nil {
			t.Fatalf("unsafe alias match: %s", title)
		}
	}
	got, score, _ := chooseSubject([]map[string]string{{"title": "幼女战记"}, {"title": "幼女战记2"}}, subject)
	if got == nil || got["title"] != "幼女战记2" || score != 1 {
		t.Fatalf("numbered sequel not matched: %v %v", got, score)
	}
	year := 2026
	subject.Year = &year
	if got, _, _ := chooseSubject([]map[string]string{{"title": "幼女战记2", "year": "2017"}}, subject); got != nil {
		t.Fatal("alias bypassed year check")
	}
}

func TestUnmarkedFirstSeasonNeverFallsForwardToSequel(t *testing.T) {
	for _, title := range []string{"幼女战记 第二季", "幼女战记 第三季", "幼女战记2"} {
		firstSeason := 1
		for _, season := range []*int{nil, &firstSeason} {
			got, _, _ := chooseSubject([]map[string]string{{"title": title}}, Subject{Titles: []string{"幼女战记", "Saga of Tanya the Evil"}, Season: season})
			if got != nil {
				t.Fatalf("first-season request selected sequel: %v", got)
			}
		}
	}
}

func TestCJKSearchFindsSequelWithoutFallingBackToFirstSeason(t *testing.T) {
	query := "https://api.example.test/search?q=" + url.QueryEscape("幼女战记第二季")
	fetcher := &responseFetcher{responses: map[string]btcrawler.Response{
		query:                                    {Body: []byte(`{"items":[{"id":"first","title":"幼女战记"},{"id":"second","title":"幼女战记第二季"}]}`)},
		"https://api.example.test/series/second": {Body: []byte(`{"slug":"second","lines":[{"name":"Main","episodes":[{"slug":"e5","label":"EP05"}]}]}`)},
		"https://cdn.example.test/second/e5.m3u8": {Body: []byte("#EXTM3U\n")},
	}}
	runner := NewSpecRunner(jsonWebSource(), RunnerOptions{Fetcher: fetcher})
	var candidates []Candidate
	err := runner.Candidates(context.Background(), ResolveRequest{Subject: Subject{Titles: []string{"幼女战记 第二季", "Saga of Tanya the Evil Season 2"}}, Episode: Episode{Number: "5"}}, func(c Candidate) error { candidates = append(candidates, c); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].Match.SubjectTitle != "幼女战记第二季" || *candidates[0].Match.EpisodeNumber != 5 {
		t.Fatalf("incorrect match: %+v", candidates)
	}
	if len(fetcher.requests) != 3 || fetcher.requests[0].URL != query {
		t.Fatalf("unexpected search fallback: %+v", fetcher.requests)
	}
}
