package main

import (
	"fmt"
	"time"
)

// diskStore keeps each fixture once. Lists and season snapshots retain only
// ordered references, so numbering and results survive a restart.
type diskStore struct {
	Fixtures     map[string]publishedMatch `json:"fixtures"`
	Forecasts    []Forecast                `json:"forecasts,omitempty"`
	Totals       map[string]Totals         `json:"totals,omitempty"`
	ResultSets   []diskResultSet           `json:"uefa_results,omitempty"`
	Lists        []diskList                `json:"fixture_lists,omitempty"`
	Seasons      []diskSeason              `json:"season_results,omitempty"`
	RoundScores  []RoundScore              `json:"round_scores,omitempty"`
	UpdateOffset int                       `json:"last_update_id,omitempty"`
}

type diskResultSet struct {
	ChatID    int64    `json:"chat_id"`
	Season    int      `json:"season,omitempty"`
	Matchday  int      `json:"matchday,omitempty"`
	From      string   `json:"from"`
	To        string   `json:"to"`
	FetchedAt string   `json:"fetched_at"`
	Matches   []string `json:"matches"`
}
type diskList struct {
	ChatID  int64    `json:"chat_id"`
	Matches []string `json:"matches"`
}
type diskSeason struct {
	ChatID          int64    `json:"chat_id"`
	Season          int      `json:"season"`
	FetchedAt       string   `json:"fetched_at"`
	CurrentMatchday int      `json:"current_matchday,omitempty"`
	Matches         []string `json:"matches"`
}

func compactStore(s Store) diskStore {
	d := diskStore{Fixtures: map[string]publishedMatch{}, Forecasts: s.Forecasts, Totals: s.Totals, RoundScores: s.RoundScores, UpdateOffset: s.UpdateOffset}
	fixtureFetchedAt := map[string]time.Time{}
	add := func(matches []publishedMatch, fetchedAt time.Time) []string {
		refs := make([]string, 0, len(matches))
		for _, match := range matches {
			key := fixtureKey(match)
			old, ok := d.Fixtures[key]
			oldFetchedAt := fixtureFetchedAt[key]
			if !ok || fetchedAt.After(oldFetchedAt) || fetchedAt.Equal(oldFetchedAt) && (!old.HasScore && match.HasScore || old.Date.IsZero() && !match.Date.IsZero()) {
				d.Fixtures[key] = match
				fixtureFetchedAt[key] = fetchedAt
			}
			refs = append(refs, key)
		}
		return refs
	}
	for _, list := range s.Lists {
		d.Lists = append(d.Lists, diskList{list.ChatID, add(list.Matches, time.Time{})})
	}
	for _, season := range s.Seasons {
		d.Seasons = append(d.Seasons, diskSeason{season.ChatID, season.Season, season.FetchedAt.Format(timeLayout), season.CurrentMatchday, add(season.Matches, season.FetchedAt)})
	}
	for _, set := range s.ResultSets {
		d.ResultSets = append(d.ResultSets, diskResultSet{ChatID: set.ChatID, Season: set.Season, Matchday: set.Matchday, From: set.From.Format(timeLayout), To: set.To.Format(timeLayout), FetchedAt: set.FetchedAt.Format(timeLayout), Matches: add(set.Matches, set.FetchedAt)})
	}
	return d
}

const timeLayout = "2006-01-02T15:04:05.999999999Z07:00"

func parseDiskTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, value)
}

func expandStore(d diskStore) (Store, error) {
	s := Store{Forecasts: d.Forecasts, Totals: d.Totals, RoundScores: d.RoundScores, UpdateOffset: d.UpdateOffset}
	if s.Totals == nil {
		s.Totals = map[string]Totals{}
	}
	get := func(refs []string) ([]publishedMatch, error) {
		matches := make([]publishedMatch, 0, len(refs))
		for _, ref := range refs {
			match, ok := d.Fixtures[ref]
			if !ok {
				return nil, fmt.Errorf("fixture %q missing from catalog", ref)
			}
			matches = append(matches, match)
		}
		return matches, nil
	}
	for _, list := range d.Lists {
		matches, err := get(list.Matches)
		if err != nil {
			return s, err
		}
		s.Lists = append(s.Lists, FixtureList{ChatID: list.ChatID, Matches: matches})
	}
	for _, season := range d.Seasons {
		matches, err := get(season.Matches)
		if err != nil {
			return s, err
		}
		fetched, err := parseDiskTime(season.FetchedAt)
		if err != nil {
			return s, err
		}
		s.Seasons = append(s.Seasons, SeasonSnapshot{ChatID: season.ChatID, Season: season.Season, FetchedAt: fetched, CurrentMatchday: season.CurrentMatchday, Matches: matches})
	}
	for _, set := range d.ResultSets {
		matches, err := get(set.Matches)
		if err != nil {
			return s, err
		}
		from, err := parseDiskTime(set.From)
		if err != nil {
			return s, err
		}
		to, err := parseDiskTime(set.To)
		if err != nil {
			return s, err
		}
		fetched, err := parseDiskTime(set.FetchedAt)
		if err != nil {
			return s, err
		}
		s.ResultSets = append(s.ResultSets, UEFAResultSet{ChatID: set.ChatID, Season: set.Season, Matchday: set.Matchday, From: from, To: to, FetchedAt: fetched, Matches: matches})
	}
	return s, nil
}
