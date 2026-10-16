package main

import (
	"fmt"
	"strings"
	"time"
)

type forecastResolution struct {
	Fixture publishedMatch
	Actual  Match
	Reason  string
	Key     string
}

func fixtureKey(m publishedMatch) string {
	if m.ID != 0 {
		return fmt.Sprintf("id:%d", m.ID)
	}
	return fmt.Sprintf("legacy:%s:%s:%s", m.Date.UTC().Format(time.RFC3339), teamKey(m.Home), teamKey(m.Away))
}

func fixtureIdentity(m publishedMatch) string {
	return fmt.Sprintf("%s:%s:%s", m.Date.UTC().Format(time.RFC3339), teamKey(m.Home), teamKey(m.Away))
}

func matchingTeam(name, shown string, names []string) bool {
	key := teamKey(name)
	if key == teamKey(shown) {
		return true
	}
	for _, candidate := range names {
		if candidate != "" && key == teamKey(candidate) {
			return true
		}
	}
	return false
}

func resolveForecast(f Forecast, fixtures []publishedMatch) forecastResolution {
	var matches []publishedMatch
	for _, candidate := range fixtures {
		if f.Match.FixtureID != 0 {
			if candidate.ID == f.Match.FixtureID {
				matches = append(matches, candidate)
			}
		} else if matchingTeam(f.Match.Home, candidate.Home, candidate.HomeNames) && matchingTeam(f.Match.Away, candidate.Away, candidate.AwayNames) {
			forecastSeason := footballSeasonYear(time.Unix(f.Date, 0).UTC())
			candidateSeason := candidate.Season
			if candidateSeason == 0 && !candidate.Date.IsZero() {
				candidateSeason = footballSeasonYear(candidate.Date)
			}
			if candidateSeason == forecastSeason {
				matches = append(matches, candidate)
			}
		}
	}
	// Snapshots saved before match IDs were added can still be resolved by names.
	if f.Match.FixtureID != 0 && len(matches) == 0 {
		for _, candidate := range fixtures {
			if candidate.ID == 0 && matchingTeam(f.Match.Home, candidate.Home, candidate.HomeNames) && matchingTeam(f.Match.Away, candidate.Away, candidate.AwayNames) {
				matches = append(matches, candidate)
			}
		}
	}
	if len(matches) == 0 {
		if f.Match.FixtureID != 0 {
			return forecastResolution{Reason: "матч с указанным ID отсутствует в сохранённых результатах", Key: fmt.Sprintf("unknown-id:%d", f.Match.FixtureID)}
		}
		return forecastResolution{Reason: "не удалось сопоставить команды с сохранёнными матчами", Key: "unknown:" + teamKey(f.Match.Home) + ":" + teamKey(f.Match.Away)}
	}
	// The same API fixture can occur in overlapping saved periods.
	unique := make([]publishedMatch, 0, len(matches))
	seen := map[string]int{}
	for _, candidate := range matches {
		key := fixtureIdentity(candidate)
		if index, exists := seen[key]; !exists {
			unique = append(unique, candidate)
			seen[key] = len(unique) - 1
		} else if candidate.ID != 0 && unique[index].ID == 0 {
			unique[index] = candidate
		}
	}
	if len(unique) > 1 {
		return forecastResolution{Reason: "найдено несколько подходящих матчей; уточните прогноз", Key: "ambiguous:" + teamKey(f.Match.Home) + ":" + teamKey(f.Match.Away)}
	}
	candidate := unique[0]
	result := forecastResolution{Fixture: candidate, Key: fixtureKey(candidate)}
	if !candidate.HasScore {
		status := candidate.Status
		if status == "" {
			status = "неизвестен"
		}
		result.Reason = "финальный счёт отсутствует (статус: " + status + ")"
		return result
	}
	result.Actual = Match{Home: candidate.Home, Away: candidate.Away, HomeGoals: candidate.HomeGoals, AwayGoals: candidate.AwayGoals}
	return result
}

// Keep one prediction per user and fixture: the latest one sent.
func selectForecasts(forecasts []Forecast, fixtures []publishedMatch) []Forecast {
	selected := map[string]Forecast{}
	order := []string{}
	for _, f := range forecasts {
		resolution := resolveForecast(f, fixtures)
		key := fmt.Sprintf("%d:%d:%s", f.ChatID, f.UserID, resolution.Key)
		previous, exists := selected[key]
		if !exists {
			order = append(order, key)
			selected[key] = f
			continue
		}
		if f.Date > previous.Date || f.Date == previous.Date && f.Key > previous.Key {
			selected[key] = f
		}
	}
	out := make([]Forecast, 0, len(order))
	for _, key := range order {
		out = append(out, selected[key])
	}
	return out
}

func formatForecastTemplate(fixtures []publishedMatch) string {
	var out strings.Builder
	if len(fixtures) > 0 {
		if round := roundIndexForStage(fixtures[0].Stage); round > 8 {
			fmt.Fprintf(&out, "Прогнозы на %s Лиги чемпионов\n", displayRound(round))
		}
	}
	out.WriteString("Скопируйте список, допишите свой счёт после двоеточия и отправьте в чат. Номера не меняйте.\n")
	if len(fixtures) > 0 {
		fmt.Fprintf(&out, "Пример: «1. %s — %s: 1:1»\n\n", displayTeam(fixtures[0].Home), displayTeam(fixtures[0].Away))
	}
	for i, fixture := range fixtures {
		home, away := displayTeam(fixture.Home), displayTeam(fixture.Away)
		fmt.Fprintf(&out, "%d. %s — %s:\n", i+1, home, away)
	}
	return strings.TrimSpace(out.String())
}
