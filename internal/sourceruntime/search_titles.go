package sourceruntime

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// 搜索站点未必忽略中日韩标题的排版空格。优先提交紧凑写法，保留原文
// 作为后备；候选仍使用原始标题和季数校验，不能因搜索变体放宽作品匹配。
func searchTitles(source map[string]any, titles []string) []string {
	matching := object(source["matching"])
	limit := integer(matching["search_title_limit"], len(titles)*3)
	if limit < 1 || limit > len(titles)*3 {
		limit = len(titles) * 3
	}
	var result []string
	seen := map[string]bool{}
	for _, title := range titles {
		title = strings.TrimSpace(title)
		if boolean(matching["search_remove_special"], false) {
			title = strings.Map(func(character rune) rune {
				if unicode.IsLetter(character) || unicode.IsNumber(character) || unicode.IsSpace(character) {
					return character
				}
				return -1
			}, title)
		}
		if boolean(matching["search_first_word_only"], false) {
			if words := strings.Fields(title); len(words) > 0 {
				title = words[0]
			}
		}
		for _, query := range []string{compactCJKSpaces(title), title, numberedSeasonTitle(title)} {
			if query == "" || seen[query] {
				continue
			}
			seen[query] = true
			result = append(result, query)
			if len(result) == limit {
				return result
			}
		}
	}
	return result
}

var terminalCJKSeason = regexp.MustCompile(`第\s*([0-9一二三四五六七八九十两]{1,3})\s*[季期]\s*$`)

// 只有明确的末尾季数才能生成数字别名，不猜测标题原有数字的含义。
func numberedSeasonTitle(title string) string {
	match := terminalCJKSeason.FindStringSubmatchIndex(title)
	if match == nil {
		return ""
	}
	season := ordinalNumber(title[match[2]:match[3]])
	base := strings.TrimSpace(title[:match[0]])
	if season < 2 || base == "" {
		return ""
	}
	return compactCJKSpaces(base) + strconv.Itoa(season)
}

func isNumberedSequel(candidate string, titles []string) bool {
	normalized := compactTitle(candidate)
	for _, title := range titles {
		base := compactTitle(title)
		if base == "" || !strings.HasPrefix(normalized, base) {
			continue
		}
		if season, err := strconv.Atoi(strings.TrimPrefix(normalized, base)); err == nil && season > 1 && season < 100 {
			return true
		}
	}
	return false
}

func compactCJKSpaces(title string) string {
	runes := []rune(title)
	var result strings.Builder
	for i := 0; i < len(runes); {
		if !unicode.IsSpace(runes[i]) {
			result.WriteRune(runes[i])
			i++
			continue
		}
		end := i + 1
		for end < len(runes) && unicode.IsSpace(runes[end]) {
			end++
		}
		if i == 0 || end == len(runes) || !cjkRune(runes[i-1]) || !cjkRune(runes[end]) {
			result.WriteString(string(runes[i:end]))
		}
		i = end
	}
	return result.String()
}

func cjkRune(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul)
}
