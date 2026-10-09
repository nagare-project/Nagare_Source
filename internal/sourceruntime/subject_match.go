package sourceruntime

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/nagare-project/Nagare_Source/internal/btindex"
)

// 在线来源的条目匹配与季数识别：搜索结果里挑出请求的那部作品（含续作季数、年份校验）。
func chooseSubject(rows []map[string]string, subject Subject) (map[string]string, float64, []string) {
	type titleAlias struct {
		value  string
		season int
	}
	aliases := make([]titleAlias, 0, len(subject.Titles))
	wantedSeason := 0
	if subject.Season != nil && *subject.Season > 0 {
		wantedSeason = *subject.Season
	}
	for _, title := range subject.Titles {
		if normalized := compactTitle(title); normalized != "" {
			season := titleSeason(title)
			aliases = append(aliases, titleAlias{value: normalized, season: season})
			if wantedSeason == 0 && season > 0 {
				wantedSeason = season
			}
		}
	}
	// 将明确的“第二季”与完全相同基础标题的“2”视为等价。
	// 仅追加精确别名，不能使用该别名进行子串匹配。
	numberedAliases := map[string]int{}
	for _, title := range subject.Titles {
		if alias := numberedSeasonTitle(title); alias != "" {
			numberedAliases[compactTitle(alias)] = titleSeason(title)
		}
	}
	var best map[string]string
	bestScore := 0.0
	basis := []string{"title_episode"}
	for _, row := range rows {
		if subject.Year != nil {
			if candidateYear, err := strconv.Atoi(firstValue(row, "year", "release_year")); err == nil && candidateYear > 0 && candidateYear != *subject.Year {
				continue
			}
		}
		candidateTitle := firstValue(row, "title", "name")
		normalized := compactTitle(candidateTitle)
		if normalized == "" {
			continue
		}
		candidateSeason := titleSeason(candidateTitle)
		// 未标季数的正篇不能因标题包含关系匹配到明确的后续季。
		if wantedSeason <= 1 && (candidateSeason > 1 || isNumberedSequel(candidateTitle, subject.Titles)) {
			continue
		}
		if season, ok := numberedAliases[normalized]; ok && season == wantedSeason {
			candidateSeason = season
			if bestScore < 1 {
				best, bestScore, basis = row, 1, []string{"title_episode"}
			}
		}
		if wantedSeason > 0 && candidateSeason > 0 && candidateSeason != wantedSeason {
			continue
		}
		key := firstValue(row, "subject_key", "subject_id")
		for _, identifier := range subject.IDs {
			if key != "" && key == identifier && bestScore < 1 {
				best, bestScore, basis = row, 1, []string{"subject_id", "title_episode"}
			}
		}
		for _, alias := range aliases {
			score := 0.0
			switch {
			case normalized == alias.value:
				score = 1
			case strings.Contains(normalized, alias.value) || strings.Contains(alias.value, normalized):
				score = 0.9
			}
			if wantedSeason > 1 && candidateSeason == 0 && (score < 1 || alias.season == 0) {
				score = 0.6
			}
			if score > bestScore {
				best, bestScore, basis = row, score, []string{"title_episode"}
			}
		}
	}
	if bestScore < 0.8 {
		return nil, 0, nil
	}
	return best, bestScore, basis
}

func matchingEpisodes(rows []map[string]string, wanted float64) []map[string]string {
	var result []map[string]string
	for _, row := range rows {
		if episode := number(row["number"]); episode > 0 && episode == wanted {
			result = append(result, row)
		}
	}
	return result
}

func hasExactTitle(rows []map[string]string, titles []string) bool {
	for _, row := range rows {
		value := compactTitle(firstValue(row, "title", "name"))
		for _, title := range titles {
			if value != "" && value == compactTitle(title) {
				return true
			}
		}
	}
	return false
}

var titleSeasonPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bseason\s*([0-9]{1,2})\b`),
	regexp.MustCompile(`(?i)\b([0-9]{1,2})(?:st|nd|rd|th)\s+season\b`),
	regexp.MustCompile(`第\s*([0-9一二三四五六七八九十两]{1,3})\s*[季期]`),
}

func compactTitle(value string) string {
	return strings.ReplaceAll(btindex.NormalizeTitle(value), " ", "")
}

func titleSeason(value string) int {
	for _, pattern := range titleSeasonPatterns {
		match := pattern.FindStringSubmatch(value)
		if len(match) == 2 {
			if season := ordinalNumber(match[1]); season > 0 {
				return season
			}
		}
	}
	return 0
}

func ordinalNumber(value string) int {
	if number, err := strconv.Atoi(value); err == nil {
		return number
	}
	digits := map[rune]int{'一': 1, '二': 2, '两': 2, '三': 3, '四': 4, '五': 5, '六': 6, '七': 7, '八': 8, '九': 9, '十': 10}
	runes := []rune(value)
	if len(runes) == 1 {
		return digits[runes[0]]
	}
	for index, character := range runes {
		if character != '十' {
			continue
		}
		tens := 1
		if index > 0 {
			tens = digits[runes[index-1]]
		}
		ones := 0
		if index+1 < len(runes) {
			ones = digits[runes[index+1]]
		}
		return tens*10 + ones
	}
	return 0
}
