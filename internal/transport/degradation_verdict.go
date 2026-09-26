package transport

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var (
	degradationGeneration         = regexp.MustCompile("(?i)(?:^|[^a-z0-9])(?:苹果|iphone|apple)[[:space:]　_-]*([0-9]+)")
	degradationEnglishUncertainty = regexp.MustCompile("(?i)(?:^|[^a-z])(?:may|might|maybe|perhaps|probably|possibly|guess|unsure|uncertain|rumor|rumour|not)(?:$|[^a-z])")
)

// This deliberately implements a fixed generation-17 routing heuristic. It
// neither measures intelligence nor verifies the latest released phone. Only
// definite, unambiguous model names produce an account-routing classification.
func classifyDegradationAnswer(answer string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(answer))
	ambiguous := func() (string, error) {
		return "error", fmt.Errorf("answer is ambiguous or does not name one definite iPhone generation")
	}
	if strings.ContainsAny(normalized, "?？") || degradationEnglishUncertainty.MatchString(normalized) {
		return ambiguous()
	}
	for _, marker := range []string{
		"不确定", "无法确定", "不能确定", "不清楚", "不知道", "不保证",
		"可能", "也许", "或许", "大概", "应该", "我猜", "猜测是", "推测",
		"传闻", "传言", "未确认", "没确认", "不是", "并非", "尚未", "未发布", "没发布",
		"i don't know", "i do not know", "cannot confirm", "can't confirm", "could be", "i think",
	} {
		if strings.Contains(normalized, marker) {
			return ambiguous()
		}
	}
	matches := degradationGeneration.FindAllStringSubmatch(normalized, -1)
	if len(matches) == 0 {
		return ambiguous()
	}
	generation := 0
	for _, match := range matches {
		value, err := strconv.Atoi(match[1])
		if err != nil || value <= 0 || value > 99 || strconv.Itoa(value) != match[1] {
			return ambiguous()
		}
		if generation != 0 && generation != value {
			return ambiguous()
		}
		generation = value
	}
	if generation == 17 {
		return "ok", nil
	}
	return "degraded", nil
}
