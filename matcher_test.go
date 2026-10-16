package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestResolveForecastNamesAndReasons(t *testing.T) {
	kickoff := time.Date(2026, 10, 14, 19, 0, 0, 0, time.UTC)
	fixtures := []publishedMatch{{ID: 42, Date: kickoff, Home: "Racing Club de Lens", Away: "Sporting CP", HomeID: 1, AwayID: 2, HasScore: true, HomeGoals: 0, AwayGoals: 2}}
	forecast := Forecast{Date: kickoff.Add(-time.Hour).Unix(), Match: Match{Home: "Ланс", Away: "Спортинг", HomeGoals: 1, AwayGoals: 1}}
	got := resolveForecast(forecast, fixtures)
	if got.Reason != "" || got.Fixture.ID != 42 || got.Actual.HomeGoals != 0 || got.Actual.AwayGoals != 2 {
		t.Fatalf("expected scored fixture, got %+v", got)
	}
	forecast.Date = kickoff.Add(time.Minute).Unix()
	if got := resolveForecast(forecast, fixtures); got.Reason != "" || got.Actual.AwayGoals != 2 {
		t.Fatalf("forecast after kickoff should resolve: %+v", got)
	}
	forecast.Match.Home = "Неизвестная команда"
	if got := resolveForecast(forecast, fixtures); got.Reason != "не удалось сопоставить команды с сохранёнными матчами" {
		t.Fatalf("unknown team reason = %q", got.Reason)
	}
	forecast.Match.Home = "Ланс"
	forecast.Date = kickoff.Add(-time.Hour).Unix()
	fixtures[0].HasScore = false
	fixtures[0].Status = "IN_PLAY"
	if got := resolveForecast(forecast, fixtures); got.Reason != "финальный счёт отсутствует (статус: IN_PLAY)" {
		t.Fatalf("unfinished match reason = %q", got.Reason)
	}
}

func TestCommandResponseStaysInForumTopic(t *testing.T) {
	var payload map[string]any
	client := &http.Client{Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{}}`)), Header: make(http.Header)}, nil
	})}
	bot := &Bot{token: "test", client: client}
	bot.handle(Message{Chat: Chat{ID: -100123, Type: "supergroup"}, MessageID: 8, MessageThreadID: 456, From: User{ID: 1}, Text: "/help"})
	if payload["chat_id"] != float64(-100123) || payload["message_thread_id"] != float64(456) {
		t.Fatalf("help response was not routed to the command topic: %#v", payload)
	}
}

func TestSampleTeamNamesResolve(t *testing.T) {
	pairs := [][2]string{
		{"Ланс", "Racing Club de Lens"}, {"Славия Прага", "SK Slavia Praha"},
		{"Интер", "FC Internazionale Milano"}, {"Брюгге", "Club Brugge KV"},
		{"Галатасарай", "Galatasaray SK"}, {"Атлетико Мадрид", "Club Atlético de Madrid"},
		{"Манчестер Юнайтед", "Manchester United FC"}, {"Арсенал", "Arsenal FC"},
		{"Лилль", "Lille OSC"}, {"Бавария", "FC Bayern München"},
		{"Вильярреал", "Villarreal CF"}, {"Ливерпуль", "Liverpool FC"},
		{"Фейеноорд", "Feyenoord Rotterdam"}, {"Комо", "Como 1907"},
		{"Реал Мадрид", "Real Madrid CF"}, {"Манчестер Сити", "Manchester City FC"},
		{"ПСЖ", "Paris Saint-Germain FC"}, {"Будё-Глимт", "FK Bodø/Glimt"},
		{"Боруссия Дортмунд", "Borussia Dortmund"}, {"Астон Вилла", "Aston Villa FC"},
		{"Фенербахче", "Fenerbahçe SK"}, {"Бетис", "Real Betis Balompié"},
		{"Шахтёр", "FK Shakhtar Donetsk"}, {"АЕК", "PAE AEK"},
		{"Слован", "ŠK Slovan Bratislava"},
	}
	for _, pair := range pairs {
		if teamKey(pair[0]) != teamKey(pair[1]) {
			t.Errorf("%q and %q resolve differently: %q vs %q", pair[0], pair[1], teamKey(pair[0]), teamKey(pair[1]))
		}
	}
}

func TestLatestForecastWinsRegardlessOfKickoff(t *testing.T) {
	kickoff := time.Date(2026, 10, 14, 19, 0, 0, 0, time.UTC)
	fixtures := []publishedMatch{{ID: 42, Date: kickoff, Home: "Ланс", Away: "Спортинг", HasScore: true, HomeGoals: 0, AwayGoals: 2}}
	forecasts := []Forecast{
		{Key: "first", ChatID: 7, UserID: 8, Date: kickoff.Add(-2 * time.Hour).Unix(), Match: Match{Home: "Ланс", Away: "Спортинг", HomeGoals: 1, AwayGoals: 1}},
		{Key: "last", ChatID: 7, UserID: 8, Date: kickoff.Add(-time.Hour).Unix(), Match: Match{Home: "Ланс", Away: "Спортинг", HomeGoals: 0, AwayGoals: 2}},
		{Key: "latest", ChatID: 7, UserID: 8, Date: kickoff.Add(time.Hour).Unix(), Match: Match{Home: "Ланс", Away: "Спортинг", HomeGoals: 4, AwayGoals: 0}},
	}
	selected := selectForecasts(forecasts, fixtures)
	if len(selected) != 1 || selected[0].Key != "latest" {
		t.Fatalf("selected forecasts = %+v", selected)
	}
}

func TestTemplateCanBeFilledAndParsed(t *testing.T) {
	fixtures := []publishedMatch{{ID: 42, Home: "Racing Club de Lens", Away: "Sporting CP"}}
	text := formatForecastTemplate(fixtures)
	if !strings.Contains(text, "Пример:") || !strings.Contains(text, "1. Ланс — Спортинг:") || strings.Contains(text, "[#42]") {
		t.Fatalf("template has unexpected format: %q", text)
	}
	if got := parseMatches(text); len(got) != 0 {
		t.Fatalf("blank template was parsed as forecasts: %+v", got)
	}
	filled := strings.Replace(text, "\n1. Ланс — Спортинг:", "\n1. Ланс — Спортинг: 1:1", 1)
	if got := parseMatches(filled); len(got) != 1 || got[0].Home != "Ланс" || got[0].Away != "Спортинг" || got[0].Number != 1 || got[0].FixtureID != 0 {
		t.Fatalf("filled template was not parsed: %+v", got)
	}
}

func TestFixtureIDResolvesRenamedTeamsAndLegacyForecast(t *testing.T) {
	kickoff := time.Date(2026, 10, 14, 19, 0, 0, 0, time.UTC)
	fixture := publishedMatch{ID: 42, Date: kickoff, Home: "Racing Club de Lens", Away: "Спортинг", HasScore: true, HomeGoals: 0, AwayGoals: 2}
	forecast := Forecast{Date: kickoff.Add(-time.Hour).Unix(), Match: Match{Home: "Другое название", Away: "Спортинг", FixtureID: 42}}
	if got := resolveForecast(forecast, []publishedMatch{fixture}); got.Reason != "" || got.Actual.AwayGoals != 2 {
		t.Fatalf("ID forecast not resolved: %+v", got)
	}
	forecast.Match.Home = "Ланс"
	forecast.Match.FixtureID = 0 // Forecasts saved before IDs were added still use team aliases.
	if got := resolveForecast(forecast, []publishedMatch{fixture}); got.Reason != "" || got.Actual.AwayGoals != 2 {
		t.Fatalf("legacy forecast not resolved: %+v", got)
	}
	forecast.Date = kickoff.Add(time.Hour).Unix()
	if got := resolveForecast(forecast, []publishedMatch{fixture}); got.Reason != "" || got.Actual.AwayGoals != 2 {
		t.Fatalf("forecast after kickoff should resolve: %+v", got)
	}
	forecast.Match.FixtureID = 42
	fixture.ID = 0
	if got := resolveForecast(forecast, []publishedMatch{fixture}); got.Reason != "" || got.Actual.AwayGoals != 2 {
		t.Fatalf("legacy snapshot without IDs should resolve by names: %+v", got)
	}
	fixture.ID = 99
	if got := resolveForecast(forecast, []publishedMatch{fixture}); got.Reason != "матч с указанным ID отсутствует в сохранённых результатах" {
		t.Fatalf("wrong fixture ID should not use another fixture: %+v", got)
	}
}

func TestTourMatchdayFetchesAndPersistsFixtureTemplate(t *testing.T) {
	sent := make(chan string, 1)
	apiCalls := 0
	client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "api.football-data.org" {
			apiCalls++
			if r.URL.Query().Get("season") != strconv.Itoa(footballSeasonYear(time.Now().UTC())) || r.URL.Query().Get("matchday") != "2" {
				t.Errorf("unexpected matchday query: %s", r.URL.RawQuery)
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"matches":[{"id":42,"utcDate":"2026-10-14T19:00:00Z","matchday":2,"status":"TIMED","homeTeam":{"id":1,"name":"Racing Club de Lens"},"awayTeam":{"id":2,"name":"Sporting CP"},"score":{"fullTime":{"home":null,"away":null}}}]}`)), Header: make(http.Header)}, nil
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		sent <- string(body)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{}}`)), Header: make(http.Header)}, nil
	})}
	bot := &Bot{token: "test", footballDataToken: "test", storePath: filepath.Join(t.TempDir(), "bot.db"), client: client, data: Store{Totals: map[string]Totals{}}}
	bot.handleTour(Message{Chat: Chat{ID: 7}}, "2")
	select {
	case response := <-sent:
		if !strings.Contains(response, "1. Ланс — Спортинг:") || strings.Contains(response, "[#42]") {
			t.Fatalf("list response has wrong template: %s", response)
		}
	case <-time.After(time.Second):
		t.Fatal("list did not send a fixture template")
	}
	if apiCalls != 1 || len(bot.data.ResultSets) != 1 || bot.data.ResultSets[0].Matchday != 2 || !bot.data.ResultSets[0].Matches[0].Date.Equal(time.Date(2026, 10, 14, 19, 0, 0, 0, time.UTC)) {
		t.Fatalf("matchday results were not persisted: calls=%d sets=%+v", apiCalls, bot.data.ResultSets)
	}
	bot.handleTour(Message{Chat: Chat{ID: 7}}, "2")
	select {
	case <-sent:
	case <-time.After(time.Second):
		t.Fatal("cached matchday list was not published")
	}
	if apiCalls != 1 {
		t.Fatalf("repeating /tour for a cached tour made another API call: %d", apiCalls)
	}
	loaded, err := loadStore(bot.storePath)
	if err != nil {
		t.Fatal(err)
	}
	restarted := &Bot{storePath: bot.storePath, data: loaded}
	restarted.handle(Message{MessageID: 5, Date: time.Now().Unix(), Chat: Chat{ID: 7}, From: User{ID: 8}, Text: "1. Другое имя — Ещё одно: 1:1"})
	if len(restarted.data.Forecasts) != 1 || restarted.data.Forecasts[0].Match.FixtureID != 42 || restarted.data.Forecasts[0].Match.Number != 1 {
		t.Fatalf("forecast number was not bound to persisted fixture ID: %+v", restarted.data.Forecasts)
	}
	restarted.handle(Message{MessageID: 6, Date: time.Now().Unix(), Chat: Chat{ID: 9}, From: User{ID: 8}, Text: "1. Другое имя — Ещё одно: 1:1"})
	if restarted.data.Forecasts[1].Match.FixtureID != 0 {
		t.Fatal("another chat reused the fixture list")
	}
}

func TestParseResultOptionsUsesMatchdayAndUsername(t *testing.T) {
	tests := []struct {
		args     string
		matchday int
		stage    string
		username string
		dataOnly bool
		refresh  bool
		wantErr  bool
	}{
		{args: ""},
		{args: "4", matchday: 4},
		{args: "4 usov_vlad", matchday: 4, username: "usov_vlad"},
		{args: "4 --refresh", matchday: 4, refresh: true},
		{args: "4 usov_vlad --refresh", matchday: 4, username: "usov_vlad", refresh: true},
		{args: "1/8", matchday: 10, stage: "LAST_16"},
		{args: "1/8 usov_vlad --refresh", matchday: 10, stage: "LAST_16", username: "usov_vlad", refresh: true},
		{args: "1/1", matchday: 13, stage: "FINAL"},
		{args: "--refresh 4", matchday: 4, refresh: true},
		{args: "usov_vlad", wantErr: true},
		{args: "--refresh", wantErr: true},
		{args: "data usov_vlad", username: "usov_vlad", dataOnly: true},
		{args: "0", wantErr: true},
		{args: "2026-10-13 2026-10-16", wantErr: true},
	}
	for _, tt := range tests {
		got, err := parseResultOptions(tt.args)
		if (err != nil) != tt.wantErr {
			t.Errorf("parseResultOptions(%q) error = %v, wantErr %v", tt.args, err, tt.wantErr)
			continue
		}
		if err == nil && (got.matchday != tt.matchday || got.stage != tt.stage || got.username != tt.username || got.dataOnly != tt.dataOnly || got.refresh != tt.refresh) {
			t.Errorf("parseResultOptions(%q) = %+v", tt.args, got)
		}
	}
}

func TestParticipantLabelIncludesTelegramUsername(t *testing.T) {
	if got := participantLabel("Usov V", "@usov_vlad"); got != "Usov V (usov_vlad)" {
		t.Fatalf("participant label = %q", got)
	}
	if got := participantLabel("@usov_vlad", "usov_vlad"); got != "usov_vlad" {
		t.Fatalf("username-only fallback should not repeat username: %q", got)
	}
}

func TestKnockoutStagesUseStableRoundKeysAndAPIAliases(t *testing.T) {
	tests := []struct {
		command, apiStage, stageAlias, display string
		round                                  int
	}{
		{"1/16", "PLAYOFFS", "PLAY_OFF_ROUND", "1/16 финала", 9},
		{"1/8", "LAST_16", "ROUND_OF_16", "1/8 финала", 10},
		{"1/4", "QUARTER_FINALS", "QUARTER_FINALS", "1/4 финала", 11},
		{"1/2", "SEMI_FINALS", "SEMI_FINALS", "1/2 финала", 12},
		{"1/1", "FINAL", "FINAL", "финал", 13},
	}
	for _, tt := range tests {
		round, stage, err := parseRoundSpecifier(tt.command)
		if err != nil || round != tt.round || stage != tt.apiStage || roundIndexForStage(tt.stageAlias) != tt.round || displayRound(round) != tt.display {
			t.Errorf("stage %s: round=%d stage=%q display=%q err=%v", tt.command, round, stage, displayRound(round), err)
		}
	}
}

func TestFetchSeasonStageResultsNormalizesStage(t *testing.T) {
	client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		if got := r.URL.Query().Get("stage"); got != "LAST_16" || r.URL.Query().Get("season") != "2026" || r.URL.Query().Get("matchday") != "" {
			t.Fatalf("unexpected stage query: %s", r.URL.RawQuery)
		}
		body := `{"matches":[{"id":101,"utcDate":"2027-03-10T20:00:00Z","matchday":0,"stage":"LAST_16","status":"FINISHED","homeTeam":{"id":1,"name":"Arsenal FC"},"awayTeam":{"id":2,"name":"Lille OSC"},"score":{"fullTime":{"home":2,"away":1}}},{"id":102,"utcDate":"2027-03-17T20:00:00Z","matchday":0,"stage":"LAST_16","status":"FINISHED","homeTeam":{"id":2,"name":"Lille OSC"},"awayTeam":{"id":1,"name":"Arsenal FC"},"score":{"fullTime":{"home":1,"away":0}}}]}`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	matches, _, err := fetchSeasonStageResults(context.Background(), client, "token", 2026, "LAST_16")
	if err != nil || len(matches) != 2 {
		t.Fatalf("fetch stage: matches=%+v err=%v", matches, err)
	}
	for _, match := range matches {
		if match.Matchday != 10 || match.Stage != "LAST_16" || !match.HasScore {
			t.Fatalf("knockout fixture not normalized: %+v", match)
		}
	}
}

func TestResultScoresAndCachesKnockoutStageAsOneRound(t *testing.T) {
	kickoffOne := time.Date(2027, 3, 10, 20, 0, 0, 0, time.UTC)
	kickoffTwo := time.Date(2027, 3, 17, 20, 0, 0, 0, time.UTC)
	apiCalls := 0
	client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet {
			apiCalls++
			if r.URL.Query().Get("stage") != "LAST_16" {
				t.Fatalf("knockout request did not use API stage filter: %s", r.URL.RawQuery)
			}
			body := `{"matches":[{"id":101,"utcDate":"2027-03-10T20:00:00Z","stage":"LAST_16","status":"FINISHED","homeTeam":{"id":1,"name":"Arsenal FC"},"awayTeam":{"id":2,"name":"Lille OSC"},"score":{"fullTime":{"home":2,"away":1}}},{"id":102,"utcDate":"2027-03-17T20:00:00Z","stage":"LAST_16","status":"FINISHED","homeTeam":{"id":2,"name":"Lille OSC"},"awayTeam":{"id":1,"name":"Arsenal FC"},"score":{"fullTime":{"home":1,"away":0}}}]}`
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{}}`)), Header: make(http.Header)}, nil
	})}
	fixtures := []publishedMatch{
		{ID: 101, Date: kickoffOne, Season: 2026, Matchday: 10, Stage: "LAST_16", Home: "Арсенал", Away: "Лилль"},
		{ID: 102, Date: kickoffTwo, Season: 2026, Matchday: 10, Stage: "LAST_16", Home: "Лилль", Away: "Арсенал"},
	}
	forecasts := []Forecast{
		{ChatID: 7, UserID: 8, Name: "Иван Иванов", Date: kickoffOne.Add(-time.Hour).Unix(), Match: Match{FixtureID: 101, Home: "Арсенал", Away: "Лилль", HomeGoals: 2, AwayGoals: 1}},
		{ChatID: 7, UserID: 8, Name: "Иван Иванов", Date: kickoffTwo.Add(-time.Hour).Unix(), Match: Match{FixtureID: 102, Home: "Лилль", Away: "Арсенал", HomeGoals: 1, AwayGoals: 0}},
	}
	bot := &Bot{token: "bot-token", footballDataToken: "fd-token", storePath: filepath.Join(t.TempDir(), "data", "bot.db"), client: client, apiInterval: 0,
		data: Store{Totals: map[string]Totals{"7:8": {Name: "Иван Иванов"}}, Forecasts: forecasts, Lists: []FixtureList{{ChatID: 7, Matches: fixtures}}},
	}
	bot.fetchAndScoreRounds(7, forecasts, resultOptions{matchday: 10, stage: "LAST_16"})
	if apiCalls != 1 || len(bot.data.RoundScores) != 1 || bot.data.RoundScores[0].Matchday != 10 || bot.data.RoundScores[0].Points != 10 || bot.data.RoundScores[0].Exact != 2 {
		t.Fatalf("knockout fixtures were not scored together: calls=%d roundScores=%+v totals=%+v", apiCalls, bot.data.RoundScores, bot.data.Totals)
	}
	if len(bot.data.ResultSets) != 1 || bot.data.ResultSets[0].Matchday != 10 || len(bot.data.ResultSets[0].Matches) != 2 {
		t.Fatalf("knockout stage results were not cached under the common round key: %+v", bot.data.ResultSets)
	}
}

func TestParticipantNamesReplaceUsernamesInSavedHistory(t *testing.T) {
	stamp := time.Date(2026, 10, 14, 18, 0, 0, 0, time.UTC).Unix()
	bot := &Bot{storePath: filepath.Join(t.TempDir(), "bot.db"), data: Store{
		Totals:      map[string]Totals{"7:8": {Name: "@usov_vlad", Points: 5, Exact: 1}},
		Forecasts:   []Forecast{{ChatID: 7, UserID: 8, Name: "@usov_vlad", Date: stamp, Match: Match{Home: "Ланс", Away: "Спортинг", HomeGoals: 1, AwayGoals: 1}}},
		RoundScores: []RoundScore{{ChatID: 7, UserID: 8, Name: "@usov_vlad", Season: 2026, Matchday: 2, Points: 5, Exact: 1, Details: []ForecastDetail{{UserID: 8, Name: "@usov_vlad", Prediction: Match{Home: "Ланс", Away: "Спортинг"}}}}},
	}}
	bot.handle(Message{MessageID: 2, Date: stamp + 60, Chat: Chat{ID: 7}, From: User{ID: 8, FirstName: "Vlad", LastName: "Usov", Username: "usov_vlad"}, Text: "1. Ланс — Спортинг: 0:0"})
	if len(bot.data.Forecasts) != 2 || bot.data.Forecasts[1].Name != "Vlad Usov" || bot.data.Forecasts[1].Username != "usov_vlad" {
		t.Fatalf("new forecast should keep the full name and username separately: %+v", bot.data.Forecasts)
	}
	if bot.data.Forecasts[0].Name != "Vlad Usov" || bot.data.RoundScores[0].Name != "Vlad Usov" || bot.data.RoundScores[0].Details[0].Name != "Vlad Usov" || bot.data.Totals["7:8"].Name != "Vlad Usov" {
		t.Fatalf("new Telegram profile should upgrade historical display labels: forecasts=%+v rounds=%+v totals=%+v", bot.data.Forecasts, bot.data.RoundScores, bot.data.Totals)
	}
	if !(resultOptions{username: "usov_vlad"}).includesUser("Vlad Usov", "usov_vlad") {
		t.Fatal("username filter should still match after display names switched to first and last name")
	}
}

func TestResultMatchdayFiltersFootballDataAndCachesRound(t *testing.T) {
	kickoff := time.Date(2026, 10, 14, 19, 0, 0, 0, time.UTC)
	apiResponse := `{"competition":{"currentSeason":{"currentMatchday":3}},"matches":[{"id":42,"utcDate":"2026-10-14T19:00:00Z","matchday":2,"status":"FINISHED","homeTeam":{"id":1,"name":"Racing Club de Lens"},"awayTeam":{"id":2,"name":"Sporting CP"},"score":{"fullTime":{"home":0,"away":2}}}]}`
	apiCalls := 0
	var telegramMessages []string
	client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet {
			apiCalls++
			if r.URL.Query().Get("season") != "2026" || r.URL.Query().Get("matchday") != "2" {
				t.Errorf("matchday request query = %s", r.URL.RawQuery)
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(apiResponse)), Header: make(http.Header)}, nil
		}
		var payload struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		telegramMessages = append(telegramMessages, payload.Text)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{}}`)), Header: make(http.Header)}, nil
	})}
	forecasts := []Forecast{
		{Key: "round-2", ChatID: 7, UserID: 8, Name: "Vlad Usov", Username: "usov_vlad", Date: kickoff.Add(-time.Hour).Unix(), Match: Match{Home: "Ланс", Away: "Спортинг", HomeGoals: 0, AwayGoals: 2, FixtureID: 42}},
		{Key: "round-3", ChatID: 7, UserID: 8, Name: "Vlad Usov", Username: "usov_vlad", Date: kickoff.Add(time.Hour).Unix(), Match: Match{Home: "Арсенал", Away: "Лилль", HomeGoals: 1, AwayGoals: 0, FixtureID: 43}},
		{Key: "round-2-other", ChatID: 7, UserID: 9, Name: "Anna Player", Username: "anna", Date: kickoff.Add(-time.Hour).Unix(), Match: Match{Home: "Ланс", Away: "Спортинг", HomeGoals: 1, AwayGoals: 0, FixtureID: 42}},
	}
	path := filepath.Join(t.TempDir(), "bot.db")
	bot := &Bot{token: "bot-token", footballDataToken: "fd-token", storePath: path, client: client, data: Store{Totals: map[string]Totals{}, Forecasts: forecasts}}
	bot.fetchAndScoreRounds(7, forecasts, resultOptions{matchday: 2, username: "usov_vlad"})
	if apiCalls != 1 {
		t.Fatalf("matchday execution made %d API calls, want 1", apiCalls)
	}
	if len(bot.data.RoundScores) != 2 {
		t.Fatalf("username-specific request should save scores for every player: %+v", bot.data.RoundScores)
	}
	if got := bot.data.Totals["7:8"]; got.Points != 5 || got.Exact != 1 {
		t.Fatalf("matchday total = %+v", got)
	}
	if got := bot.data.Totals["7:9"]; got.Points != 0 || got.Exact != 0 {
		t.Fatalf("other player's points were not saved: %+v", got)
	}
	if len(bot.data.ResultSets) != 1 || bot.data.ResultSets[0].Season != 2026 || bot.data.ResultSets[0].Matchday != 2 {
		t.Fatalf("matchday result cache not saved: %+v", bot.data.ResultSets)
	}
	if len(telegramMessages) != 1 || !strings.Contains(telegramMessages[0], "тур 2") || strings.Contains(telegramMessages[0], "Арсенал") || strings.Contains(telegramMessages[0], "Anna Player") {
		t.Fatalf("username report should contain only the selected player's breakdown: %v", telegramMessages)
	}
	if !strings.Contains(telegramMessages[0], "Vlad Usov (usov_vlad)") || !strings.Contains(telegramMessages[0], "Ланс—Спортинг 0:2→0:2 +5") || strings.Contains(telegramMessages[0], "@usov_vlad") {
		t.Fatalf("result output should show the player's name and Telegram username: %v", telegramMessages[0])
	}
	bot.fetchAndScoreRounds(7, forecasts, resultOptions{matchday: 2})
	bot.fetchAndScoreRounds(7, forecasts, resultOptions{matchday: 2, username: "usov_vlad"})
	if apiCalls != 1 {
		t.Fatalf("cached finished matchday or username lookup triggered an API call; calls=%d", apiCalls)
	}
	loaded, err := loadStore(path)
	if err != nil || len(loaded.ResultSets) != 1 || loaded.ResultSets[0].Matchday != 2 {
		t.Fatalf("matchday cache did not survive reload: state=%+v err=%v", loaded, err)
	}
	restarted := &Bot{token: "bot-token", footballDataToken: "fd-token", storePath: path, client: client, data: loaded}
	restarted.fetchAndScoreRounds(7, forecasts, resultOptions{matchday: 2, username: "usov_vlad"})
	if apiCalls != 1 {
		t.Fatalf("username-specific result should use persisted round data after restart; calls=%d", apiCalls)
	}
}

func TestForcedMatchdayRefreshUpdatesCachedScoresAndTotals(t *testing.T) {
	kickoff := time.Date(2026, 10, 14, 19, 0, 0, 0, time.UTC)
	updatedResponse := `{"competition":{"currentSeason":{"currentMatchday":3}},"matches":[{"id":42,"utcDate":"2026-10-14T19:00:00Z","matchday":2,"status":"FINISHED","homeTeam":{"id":1,"name":"Racing Club de Lens"},"awayTeam":{"id":2,"name":"Sporting CP"},"score":{"fullTime":{"home":0,"away":1}}}]}`
	apiCalls := 0
	client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet {
			apiCalls++
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(updatedResponse)), Header: make(http.Header)}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{}}`)), Header: make(http.Header)}, nil
	})}
	forecast := Forecast{Key: "forecast", ChatID: 7, UserID: 8, Name: "Vlad Usov", Username: "usov_vlad", Date: kickoff.Add(-time.Hour).Unix(), Match: Match{Home: "Ланс", Away: "Спортинг", HomeGoals: 0, AwayGoals: 2, FixtureID: 42}}
	oldFixture := publishedMatch{ID: 42, Date: kickoff, Season: 2026, Matchday: 2, Home: "Ланс", Away: "Спортинг", Status: "FINISHED", HasScore: true, HomeGoals: 0, AwayGoals: 2}
	path := filepath.Join(t.TempDir(), "bot.db")
	store, initial, err := OpenSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	initial = Store{
		Totals:      map[string]Totals{"7:8": {Name: "Vlad Usov", Points: 5, Exact: 1}},
		Forecasts:   []Forecast{forecast},
		ResultSets:  []UEFAResultSet{{ChatID: 7, Season: 2026, Matchday: 2, FetchedAt: kickoff, Matches: []publishedMatch{oldFixture}}},
		RoundScores: []RoundScore{{ChatID: 7, UserID: 8, Name: "Vlad Usov", Season: 2026, Matchday: 2, Points: 5, Exact: 1}},
	}
	if err := store.Save(initial); err != nil {
		t.Fatal(err)
	}
	bot := &Bot{
		token: "bot-token", footballDataToken: "fd-token", storePath: path, storage: store, client: client, data: initial,
	}
	bot.fetchAndScoreRounds(7, []Forecast{forecast}, resultOptions{matchday: 2, refresh: true})
	if apiCalls != 1 {
		t.Fatalf("forced update made %d API calls, want 1", apiCalls)
	}
	if got := bot.data.Totals["7:8"]; got.Points != 1 || got.Exact != 0 {
		t.Fatalf("corrected total = %+v, want 1 point and 0 exact scores", got)
	}
	if len(bot.data.RoundScores) != 1 || bot.data.RoundScores[0].Points != 1 || bot.data.RoundScores[0].Exact != 0 {
		t.Fatalf("round score was not recomputed: %+v", bot.data.RoundScores)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.ResultSets) != 1 || loaded.ResultSets[0].Matches[0].AwayGoals != 1 {
		t.Fatalf("updated API result was not persisted over the cached result: %+v", loaded.ResultSets)
	}
	updatedResponse = `{"competition":{"currentSeason":{"currentMatchday":3}},"matches":[{"id":42,"utcDate":"2026-10-14T19:00:00Z","matchday":2,"status":"POSTPONED","homeTeam":{"id":1,"name":"Racing Club de Lens"},"awayTeam":{"id":2,"name":"Sporting CP"},"score":{"fullTime":{"home":null,"away":null}}}]}`
	bot.fetchAndScoreRounds(7, []Forecast{forecast}, resultOptions{matchday: 2, refresh: true})
	if len(bot.data.RoundScores) != 0 || bot.data.Totals["7:8"].Points != 0 {
		t.Fatalf("newly pending round must remove old points: scores=%+v totals=%+v", bot.data.RoundScores, bot.data.Totals["7:8"])
	}
	loaded, err = store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.RoundScores) != 0 || loaded.Totals["7:8"].Points != 0 {
		t.Fatalf("removed round score was restored from SQLite: scores=%+v totals=%+v", loaded.RoundScores, loaded.Totals["7:8"])
	}
}

func TestNumberedForecastUsesFixtureIDWhenResultOrderAndNamesChange(t *testing.T) {
	fixture := publishedMatch{ID: 42, Date: time.Date(2026, 10, 14, 19, 0, 0, 0, time.UTC), Season: 2026, Matchday: 2, Home: "API home", Away: "API away", HasScore: true, HomeGoals: 1, AwayGoals: 1}
	forecast := Forecast{ChatID: 7, UserID: 8, Date: time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC).Unix(), Match: Match{Number: 2, FixtureID: 42, Home: "Сабах", Away: "Славия Прага", HomeGoals: 1, AwayGoals: 1}}
	got := resolveForecast(forecast, []publishedMatch{fixture})
	if got.Reason != "" || got.Fixture.Matchday != 2 || got.Actual.HomeGoals != 1 {
		t.Fatalf("forecast should resolve by its stored fixture ID: %+v", got)
	}
}

func TestResultScoresLatestForecastAndDoesNotScoreTwice(t *testing.T) {
	kickoff := time.Date(2026, 10, 14, 19, 0, 0, 0, time.UTC)
	fixtures := []publishedMatch{
		{ID: 42, Date: kickoff, Season: 2026, Matchday: 1, Home: "Racing Club de Lens", Away: "Sporting CP", HasScore: true, HomeGoals: 0, AwayGoals: 2},
		{ID: 43, Date: kickoff.Add(time.Hour), Season: 2026, Matchday: 1, Home: "Arsenal FC", Away: "Lille OSC", HasScore: true, HomeGoals: 3, AwayGoals: 0},
		{ID: 44, Date: kickoff.AddDate(0, 0, 7), Season: 2026, Matchday: 2, Home: "Viking", Away: "Bayern", HasScore: true, HomeGoals: 3, AwayGoals: 3},
	}
	bot := &Bot{data: Store{Totals: map[string]Totals{}, Forecasts: []Forecast{
		{Key: "a", ChatID: 7, UserID: 8, Name: "@test", Date: kickoff.Add(-2 * time.Hour).Unix(), Match: Match{Home: "Ланс", Away: "Спортинг", HomeGoals: 1, AwayGoals: 1}},
		{Key: "b", ChatID: 7, UserID: 8, Name: "@test", Date: kickoff.Add(-time.Hour).Unix(), Match: Match{Home: "Ланс", Away: "Спортинг", HomeGoals: 0, AwayGoals: 2}},
		{Key: "c", ChatID: 7, UserID: 8, Name: "@test", Date: kickoff.Add(time.Hour).Unix(), Match: Match{Home: "Ланс", Away: "Спортинг", HomeGoals: 0, AwayGoals: 2, Number: 1}},
		{Key: "d", ChatID: 7, UserID: 8, Name: "@test", Date: kickoff.Add(-2 * time.Hour).Unix(), Match: Match{Home: "Арсенал", Away: "Лилль", HomeGoals: 3, AwayGoals: 0}},
		{Key: "e", ChatID: 7, UserID: 8, Name: "@test", Date: kickoff.AddDate(0, 0, 6).Unix(), Match: Match{Home: "Викинг", Away: "Бавария", HomeGoals: 3, AwayGoals: 3}},
	}}}
	bot.mu.Lock()
	bot.scoreRoundsLocked(7, bot.data.Forecasts, fixtures)
	bot.mu.Unlock()
	if got := bot.data.Totals["7:8"]; got.Points != 15 || got.Exact != 3 {
		t.Fatalf("first scoring totals = %+v, want 15 points and 3 exact scores", got)
	}
	if len(bot.data.RoundScores) != 2 {
		t.Fatalf("two distinct matchdays should create two player round scores: %+v", bot.data.RoundScores)
	}
	bot.mu.Lock()
	bot.scoreRoundsLocked(7, bot.data.Forecasts, fixtures)
	bot.mu.Unlock()
	if got := bot.data.Totals["7:8"]; got.Points != 15 || got.Exact != 3 || len(bot.data.RoundScores) != 2 {
		t.Fatalf("re-running the same round must not add points: totals=%+v rounds=%+v", got, bot.data.RoundScores)
	}
}

func TestPendingForecastDefersWholeUserRoundUntilAllScoresAreKnown(t *testing.T) {
	kickoff := time.Date(2026, 10, 14, 19, 0, 0, 0, time.UTC)
	forecasts := []Forecast{
		{Key: "first", ChatID: 7, UserID: 8, Name: "@player", Date: kickoff.Add(-time.Hour).Unix(), Match: Match{Home: "Ланс", Away: "Спортинг", HomeGoals: 0, AwayGoals: 2}},
		{Key: "second", ChatID: 7, UserID: 8, Name: "@player", Date: kickoff.Add(-time.Hour).Unix(), Match: Match{Home: "Арсенал", Away: "Лилль", HomeGoals: 2, AwayGoals: 0}},
	}
	fixtures := []publishedMatch{
		{ID: 41, Date: kickoff, Season: 2026, Matchday: 1, Home: "Racing Club de Lens", Away: "Sporting CP", Status: "FINISHED", HasScore: true, HomeGoals: 0, AwayGoals: 2},
		{ID: 42, Date: kickoff.Add(time.Hour), Season: 2026, Matchday: 1, Home: "Arsenal FC", Away: "Lille OSC", Status: "IN_PLAY"},
	}
	bot := &Bot{data: Store{Totals: map[string]Totals{}, Forecasts: forecasts}}
	bot.mu.Lock()
	_, pending := bot.scoreRoundsLocked(7, forecasts, fixtures)
	bot.mu.Unlock()
	if len(pending) != 2 || bot.data.Totals["7:8"].Points != 0 || len(bot.data.RoundScores) != 0 {
		t.Fatalf("incomplete round should remain unscored: pending=%+v totals=%+v roundScores=%+v", pending, bot.data.Totals, bot.data.RoundScores)
	}
	fixtures[1].Status, fixtures[1].HasScore, fixtures[1].HomeGoals, fixtures[1].AwayGoals = "FINISHED", true, 3, 0
	bot.mu.Lock()
	bot.scoreRoundsLocked(7, forecasts, fixtures)
	bot.mu.Unlock()
	if got := bot.data.Totals["7:8"]; got.Points != 6 || got.Exact != 1 || len(bot.data.RoundScores) != 1 {
		t.Fatalf("completed round should score both forecasts once: totals=%+v rounds=%+v", got, bot.data.RoundScores)
	}
}

func TestSkippedMatchesAndWholeRoundsAreSavedAsZero(t *testing.T) {
	kickoff := time.Date(2026, 10, 14, 19, 0, 0, 0, time.UTC)
	fixtures := []publishedMatch{
		{ID: 1, Date: kickoff, Season: 2026, Matchday: 1, Home: "Ланс", Away: "Спортинг", HasScore: true, HomeGoals: 0, AwayGoals: 2},
		{ID: 2, Date: kickoff.Add(time.Hour), Season: 2026, Matchday: 1, Home: "Арсенал", Away: "Лилль", HasScore: true, HomeGoals: 3, AwayGoals: 0},
	}
	roster := []Forecast{
		{ChatID: 7, UserID: 8, Name: "@submitted", Date: kickoff.Add(-time.Hour).Unix(), Match: Match{Home: "Ланс", Away: "Спортинг", HomeGoals: 0, AwayGoals: 2}},
		{ChatID: 7, UserID: 9, Name: "@skipped", Date: kickoff.AddDate(0, 0, 7).Unix(), Match: Match{Home: "Рома", Away: "Порту", HomeGoals: 1, AwayGoals: 1}},
	}
	bot := &Bot{data: Store{Totals: map[string]Totals{}}}
	bot.mu.Lock()
	bot.scoreRoundsLocked(7, roster[:1], fixtures)
	bot.scoreNoForecastPlayersLocked(7, roster, fixtures, resultOptions{matchday: 1})
	bot.mu.Unlock()
	if len(bot.data.RoundScores) != 2 {
		t.Fatalf("expected both active and skipped participants to be scored: %+v", bot.data.RoundScores)
	}
	var skipped *RoundScore
	var submitted *RoundScore
	for i := range bot.data.RoundScores {
		if bot.data.RoundScores[i].UserID == 9 {
			skipped = &bot.data.RoundScores[i]
		} else if bot.data.RoundScores[i].UserID == 8 {
			submitted = &bot.data.RoundScores[i]
		}
	}
	if submitted == nil || submitted.Missing != 1 {
		t.Fatalf("missing individual prediction should count as zero: %+v", submitted)
	}
	if skipped == nil || skipped.Points != 0 || skipped.Missing != 2 || bot.data.Totals["7:9"].Name != "@skipped" {
		t.Fatalf("whole-round absence should persist zero and roster entry: score=%+v totals=%+v", skipped, bot.data.Totals)
	}
	bot.mu.Lock()
	bot.scoreRoundsLocked(7, roster[:1], fixtures)
	bot.scoreNoForecastPlayersLocked(7, roster, fixtures, resultOptions{matchday: 1})
	bot.mu.Unlock()
	if len(bot.data.RoundScores) != 2 || bot.data.Totals["7:9"].Points != 0 {
		t.Fatalf("reprocessing a round must not duplicate zero scores: %+v %+v", bot.data.RoundScores, bot.data.Totals)
	}
}

func TestStatsUsesCompactRoundMatrix(t *testing.T) {
	var sent string
	var parseMode string
	client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		var payload struct {
			Text      string `json:"text"`
			ParseMode string `json:"parse_mode"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, err
		}
		sent = payload.Text
		parseMode = payload.ParseMode
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{}}`)), Header: make(http.Header)}, nil
	})}
	bot := &Bot{client: client, token: "test", data: Store{
		Totals: map[string]Totals{"7:8": {Name: "Anna One", Username: "one", Points: 17, Exact: 3}, "7:9": {Name: "Boris Two", Username: "two", Points: 0}},
		RoundScores: []RoundScore{
			{ChatID: 7, UserID: 8, Name: "Anna One", Username: "one", Season: 2026, Matchday: 1, Points: 12, Exact: 2},
			{ChatID: 7, UserID: 8, Name: "Anna One", Username: "one", Season: 2026, Matchday: 10, Points: 5, Exact: 1},
			{ChatID: 7, UserID: 9, Name: "Boris Two", Username: "two", Season: 2026, Matchday: 1, Missing: 18},
		},
		Seasons: []SeasonSnapshot{{ChatID: 7, Season: 2026, CurrentMatchday: 3, FetchedAt: time.Now()}},
	}}
	bot.handleStats(Message{Chat: Chat{ID: 7}})
	if parseMode != "HTML" || !strings.Contains(sent, "<pre>") || !strings.Contains(sent, "Игрок") || !strings.Contains(sent, "Очки/точно") || !strings.Contains(sent, "1/8") || !strings.Contains(sent, "+12") || !strings.Contains(sent, "+5") || !strings.Contains(sent, "Anna One (one)") || !strings.Contains(sent, "Boris Two (two)") || strings.Index(sent, "Anna One (one)") > strings.Index(sent, "Boris Two (two)") || strings.Contains(sent, "пропусков: 0") || strings.Contains(sent, "@one") || strings.Contains(sent, "Текущий тур Лиги чемпионов") {
		t.Fatalf("stats response should be one compact player-by-round table: %q", sent)
	}
}

func TestHelpAndDebugCommands(t *testing.T) {
	var sent []string
	client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		sent = append(sent, string(body))
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{}}`)), Header: make(http.Header)}, nil
	})}
	logs := &recentLog{}
	_, _ = logs.Write([]byte("recent test log with secret-token\n"))
	bot := &Bot{token: "secret-token", client: client, startedAt: time.Now().Add(-2 * time.Minute), recentLog: logs}
	bot.handle(Message{Chat: Chat{ID: 7}, From: User{ID: 8}, Text: "/help"})
	bot.handle(Message{Chat: Chat{ID: 7}, From: User{ID: 8}, Text: "/debug"})
	if len(sent) != 2 || !strings.Contains(sent[0], "/result") || !strings.Contains(sent[0], "/tour") || strings.Contains(sent[0], "/result username") {
		t.Fatalf("help did not list the main commands: %v", sent)
	}
	if !strings.Contains(sent[1], "Бот работает: 2 мин.") || !strings.Contains(sent[1], "recent test log") || strings.Contains(sent[1], "secret-token") {
		t.Fatalf("debug output should show uptime and redacted recent logs: %v", sent)
	}
}

func TestMessageLogsReplyContextAndHandlingDecision(t *testing.T) {
	var output bytes.Buffer
	oldWriter, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(&output)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(oldWriter)
		log.SetFlags(oldFlags)
	}()

	bot := &Bot{token: "secret-token"}
	bot.handle(Message{
		MessageID: 45,
		Chat:      Chat{ID: 7},
		From:      User{ID: 8},
		Text:      "!",
		ReplyTo:   &Message{MessageID: 44, From: User{ID: 9}, Text: "это обычный комментарий"},
	})
	got := output.String()
	for _, want := range []string{`message_id=45`, `reply_to=44`, `reply_text="это обычный комментарий"`, `action=ignored reason=no_forecasts`} {
		if !strings.Contains(got, want) {
			t.Errorf("debug log missing %q: %s", want, got)
		}
	}
}

func TestSingleCharacterReplyImportsForecastFromReplyAnchor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot.db")
	store, data, err := OpenSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	bot := &Bot{storePath: path, storage: store, data: data}
	anchor := &Message{MessageID: 44, Date: 100, Chat: Chat{ID: 7}, From: User{ID: 9, Username: "original"}, Text: "1. Ланс — Спортинг: 1:1"}
	trigger := Message{MessageID: 45, Date: 200, Chat: Chat{ID: 7}, From: User{ID: 8, Username: "replying"}, Text: "yy", ReplyTo: anchor}
	if !bot.handle(trigger) {
		t.Fatal("reply anchor forecast did not persist")
	}
	trigger.MessageID = 46
	if !bot.handle(trigger) {
		t.Fatal("repeated reply anchor forecast did not persist")
	}
	got, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Forecasts) != 1 {
		t.Fatalf("reply anchor was not deduplicated: %+v", got.Forecasts)
	}
	forecast := got.Forecasts[0]
	if forecast.Key != "7:44:9:0" || forecast.UserID != 9 || forecast.Name != "@original" || forecast.Date != 100 || forecast.Match.HomeGoals != 1 || forecast.Match.AwayGoals != 1 {
		t.Fatalf("forecast did not use anchor author, date, and content: %+v", forecast)
	}
}

func TestSendLogsOutputAndRedactsToken(t *testing.T) {
	var output bytes.Buffer
	oldWriter, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(&output)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(oldWriter)
		log.SetFlags(oldFlags)
	}()

	client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{}}`)), Header: make(http.Header)}, nil
	})}
	bot := &Bot{token: "secret-token", client: client}
	if err := bot.send(7, "Прогноз принят secret-token"); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	if !strings.Contains(got, "telegram output attempt") || !strings.Contains(got, "Прогноз принят") || !strings.Contains(got, "telegram output sent") || strings.Contains(got, "secret-token") {
		t.Fatalf("output log missing or leaked token: %s", got)
	}
}

func TestFetchSeasonResultsIncludesMatchdayAndCurrentRound(t *testing.T) {
	client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Query().Get("season") != "2026" {
			t.Fatalf("season query = %q", r.URL.RawQuery)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"competition":{"currentSeason":{"currentMatchday":3}},"matches":[{"id":42,"utcDate":"2026-10-14T19:00:00Z","matchday":2,"stage":"LEAGUE_PHASE","status":"FINISHED","homeTeam":{"id":1,"name":"Arsenal FC"},"awayTeam":{"id":2,"name":"Lille OSC"},"score":{"fullTime":{"home":3,"away":0}}}]}`)), Header: make(http.Header)}, nil
	})}
	matches, current, err := fetchSeasonResults(context.Background(), client, "token", 2026)
	if err != nil {
		t.Fatal(err)
	}
	if current != 3 || len(matches) != 1 || matches[0].Matchday != 2 || matches[0].Season != 2026 || !matches[0].HasScore {
		t.Fatalf("season response lost round or current-matchday data: current=%d matches=%+v", current, matches)
	}
}

func TestResultFetchesRealScoresAndDoesNotScoreSameRoundTwice(t *testing.T) {
	kickoff := time.Date(2026, 10, 14, 19, 0, 0, 0, time.UTC)
	apiResponse := `{"competition":{"currentSeason":{"currentMatchday":3}},"matches":[{"id":42,"utcDate":"2026-10-14T19:00:00Z","matchday":2,"stage":"LEAGUE_PHASE","status":"FINISHED","homeTeam":{"id":1,"name":"Racing Club de Lens"},"awayTeam":{"id":2,"name":"Sporting CP"},"score":{"fullTime":{"home":0,"away":2}}}]}`
	var telegramMessages []string
	apiCalls := 0
	client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet {
			apiCalls++
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(apiResponse)), Header: make(http.Header)}, nil
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		var payload struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		telegramMessages = append(telegramMessages, payload.Text)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{}}`)), Header: make(http.Header)}, nil
	})}
	forecast := Forecast{Key: "7:55:8:0", ChatID: 7, UserID: 8, Name: "@player", Date: kickoff.Add(-time.Hour).Unix(), Match: Match{Home: "Ланс", Away: "Спортинг", HomeGoals: 0, AwayGoals: 2}}
	bot := &Bot{token: "bot-token", footballDataToken: "fd-token", storePath: filepath.Join(t.TempDir(), "data", "bot.db"), client: client, data: Store{Totals: map[string]Totals{}, Forecasts: []Forecast{forecast}}}
	bot.fetchAndScoreRounds(7, []Forecast{forecast}, resultOptions{})
	if apiCalls != 1 {
		t.Fatalf("finished cached result should avoid another API call; got %d", apiCalls)
	}
	if got := bot.data.Totals["7:8"]; got.Points != 5 || got.Exact != 1 {
		t.Fatalf("first /result totals = %+v, want 5 and 1 exact", got)
	}
	if len(bot.data.RoundScores) != 1 || bot.data.RoundScores[0].Season != 2026 || bot.data.RoundScores[0].Matchday != 2 {
		t.Fatalf("real result was not saved under its API matchday: %+v", bot.data.RoundScores)
	}
	if len(telegramMessages) != 1 || !strings.Contains(telegramMessages[0], "тур 2") || !strings.Contains(telegramMessages[0], "Ланс—Спортинг 0:2") || !strings.Contains(telegramMessages[0], "Очки:\n@player +5 (точно: 1)") || strings.Contains(telegramMessages[0], "пропусков: 0") {
		t.Fatalf("/result should publish actual scores and round points: %v", telegramMessages)
	}
	bot.fetchAndScoreRounds(7, []Forecast{forecast}, resultOptions{})
	if got := bot.data.Totals["7:8"]; got.Points != 5 || got.Exact != 1 || len(bot.data.RoundScores) != 1 {
		t.Fatalf("repeating /result added the same round twice: totals=%+v roundScores=%+v", got, bot.data.RoundScores)
	}
	loaded, err := loadStore(bot.storePath)
	if err != nil || len(loaded.Seasons) != 1 || len(loaded.RoundScores) != 1 || loaded.Totals["7:8"].Points != 5 {
		t.Fatalf("result state did not survive loading JSON after restart: store=%+v err=%v", loaded, err)
	}
}

func TestSQLiteRoundTripKeepsHistoryAndIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot.db")
	fixture := publishedMatch{ID: 42, Date: time.Date(2026, 10, 14, 19, 0, 0, 0, time.UTC), Season: 2026, Matchday: 2, Home: "Ланс", Away: "Спортинг", HasScore: true, AwayGoals: 2}
	store, state, err := OpenSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	state = Store{
		UpdateOffset: 987,
		Forecasts:    []Forecast{{Key: "f1", ChatID: 7, UserID: 8, Name: "@p", Date: 123, Match: Match{Home: "Ланс", Away: "Спортинг", HomeGoals: 0, AwayGoals: 2, FixtureID: 42}}},
		ResultSets:   []UEFAResultSet{{ChatID: 7, Season: 2026, Matchday: 2, FetchedAt: time.Now().UTC(), Matches: []publishedMatch{fixture}}},
		Lists:        []FixtureList{{ChatID: 7, Matches: []publishedMatch{fixture}}},
		Seasons:      []SeasonSnapshot{{ChatID: 7, Season: 2026, CurrentMatchday: 3, Matches: []publishedMatch{fixture}}},
		RoundScores:  []RoundScore{{ChatID: 7, UserID: 8, Name: "@p", Season: 2026, Matchday: 2, Points: 5, Exact: 1}},
		Totals:       map[string]Totals{"7:8": {Name: "@p", Points: 5, Exact: 1}},
	}
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.UpdateOffset != 987 || len(got.Forecasts) != 1 || len(got.ResultSets) != 1 || got.ResultSets[0].Season != 2026 || got.ResultSets[0].Matchday != 2 || len(got.Lists) != 1 || len(got.Seasons) != 1 || len(got.RoundScores) != 1 || got.Totals["7:8"].Points != 5 {
		t.Fatalf("SQLite round trip lost data: %+v", got)
	}
	if len(compactStore(got).Fixtures) != 1 {
		t.Fatal("same fixture was stored more than once")
	}
}

func TestSQLiteStartsEmptyAndIgnoresBotJSON(t *testing.T) {
	dir := t.TempDir()
	legacyPath := filepath.Join(dir, "bot.json")
	dbPath := filepath.Join(dir, "bot.db")
	old := Store{Forecasts: []Forecast{{Key: "old", ChatID: 7, UserID: 8, Name: "@old", Date: 10, Match: Match{Home: "A", Away: "B", HomeGoals: 1}}}, Totals: map[string]Totals{"7:8": {Name: "@old", Points: 5}}}
	oldJSON, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, oldJSON, 0600); err != nil {
		t.Fatal(err)
	}
	store, state, err := OpenSQLiteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if len(state.Forecasts) != 0 || len(state.Totals) != 0 || state.UpdateOffset != 0 {
		t.Fatalf("new SQLite store should start empty: %+v", state)
	}
	after, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(oldJSON, after) {
		t.Fatal("old bot.json was modified")
	}
}

func TestSQLiteRejectsCorruptDatabaseFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot.db")
	if err := os.WriteFile(path, []byte("partial sqlite header"), 0600); err != nil {
		t.Fatal(err)
	}
	if store, _, err := OpenSQLiteStore(path); err == nil {
		_ = store.Close()
		t.Fatal("corrupt SQLite database should fail startup")
	}
}

func TestSQLitePreservesEmbeddedNULAndQuotes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot.db")
	store, _, err := OpenSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	name := "name\x00with ' quote"
	state := Store{Totals: map[string]Totals{}, Forecasts: []Forecast{{Key: "nul", ChatID: 1, UserID: 2, Name: name, Date: 1, Match: Match{Home: "A\x00B", Away: "C", HomeGoals: 1, AwayGoals: 0}}}}
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Forecasts) != 1 || got.Forecasts[0].Name != name || got.Forecasts[0].Match.Home != "A\x00B" {
		t.Fatalf("SQLite truncated text fields: %+v", got.Forecasts)
	}
}

func TestRepeatedTelegramUpdateDoesNotDuplicateForecastInSQLite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot.db")
	store, data, err := OpenSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	bot := &Bot{storePath: path, storage: store, data: data}
	message := Message{MessageID: 55, Date: 100, Chat: Chat{ID: 7}, From: User{ID: 8, Username: "player"}, Text: "1. Ланс — Спортинг: 1:1"}
	if !bot.handle(message) || !bot.handle(message) {
		t.Fatal("forecast update did not persist")
	}
	got, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Forecasts) != 1 {
		t.Fatalf("replayed Telegram update duplicated forecast: %+v", got.Forecasts)
	}
}

func TestForwardedForecastUsesOriginalAuthorAndDate(t *testing.T) {
	var message Message
	if err := json.Unmarshal([]byte(`{"message_id":55,"date":200,"text":"1. Ланс — Спортинг: 1:1","from":{"id":99,"username":"forwarder"},"chat":{"id":7,"type":"group"},"forward_origin":{"type":"user","date":100,"sender_user":{"id":8,"username":"player"}}}`), &message); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "bot.db")
	store, data, err := OpenSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	bot := &Bot{storePath: path, storage: store, data: data}
	if !bot.handle(message) {
		t.Fatal("forwarded forecast did not persist")
	}
	got, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Forecasts) != 1 {
		t.Fatalf("forecast count = %d", len(got.Forecasts))
	}
	forecast := got.Forecasts[0]
	if forecast.UserID != 8 || forecast.Name != "@player" || forecast.Date != 100 || forecast.ChatID != 7 {
		t.Fatalf("forecast attributed incorrectly: %+v", forecast)
	}
}

func TestHiddenForwardedCaptionUsesStableAnonymousAuthor(t *testing.T) {
	var message Message
	if err := json.Unmarshal([]byte(`{"message_id":56,"date":200,"caption":"1. Ланс — Спортинг: 1:1","from":{"id":99,"username":"forwarder"},"chat":{"id":7,"type":"group"},"forward_origin":{"type":"hidden_user","date":100,"sender_user_name":"Anonymous Player"}}`), &message); err != nil {
		t.Fatal(err)
	}
	author, date, ok := forecastAuthor(message)
	if !ok || author.ID >= 0 || author.Name() != "Anonymous Player" || date != 100 {
		t.Fatalf("hidden forward author = %+v, date %d, ok %v", author, date, ok)
	}
	again, _, ok := forecastAuthor(Message{From: User{ID: 123}, ForwardOrigin: &MessageOrigin{Type: "hidden_user", SenderUserName: "anonymous player"}})
	if !ok || again.ID != author.ID {
		t.Fatalf("hidden author ID is not stable: %d != %d", again.ID, author.ID)
	}
	if got := parseMatches(message.Caption); len(got) != 1 {
		t.Fatalf("caption forecast was not parsed: %+v", got)
	}
	if _, _, ok := forecastAuthor(Message{From: User{ID: 99}, ForwardOrigin: &MessageOrigin{Type: "channel"}}); ok {
		t.Fatal("channel post should not be attributed to the person forwarding it")
	}
}

func TestSQLiteDatabaseFileHasPrivatePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private.db")
	store, _, err := OpenSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("SQLite file permissions = %o, want 600", info.Mode().Perm())
	}
}

func TestSQLiteDoesNotChangePermissionsOfExistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "existing.db")
	if err := os.WriteFile(path, nil, 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0640); err != nil {
		t.Fatal(err)
	}
	store, _, err := OpenSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0640 {
		t.Fatalf("opening existing DB changed permissions to %o", info.Mode().Perm())
	}
}

func TestResultDataUsernameDoesNotWriteOrFetch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot.db")
	fixture := publishedMatch{ID: 42, Date: time.Date(2026, 10, 14, 19, 0, 0, 0, time.UTC), Season: 2026, Matchday: 2, Home: "Ланс", Away: "Спортинг", HasScore: true, Status: "FINISHED", HomeGoals: 0, AwayGoals: 2}
	forecast := Forecast{Key: "7:55:8:0", ChatID: 7, UserID: 8, Name: "@player", Date: fixture.Date.Add(-time.Hour).Unix(), Match: Match{Home: "Ланс", Away: "Спортинг", HomeGoals: 0, AwayGoals: 2, FixtureID: 42}}
	var messages []string
	bot := &Bot{token: "bot-token", storePath: path, data: Store{Totals: map[string]Totals{}, Forecasts: []Forecast{forecast}, Seasons: []SeasonSnapshot{{ChatID: 7, Season: 2026, Matches: []publishedMatch{fixture}}}}}
	bot.client = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost {
			t.Fatalf("unexpected API request: %s", r.URL)
		}
		var body struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		messages = append(messages, body.Text)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{}}`)), Header: make(http.Header)}, nil
	})}
	if err := bot.saveLocked(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	bot.handleResult(Message{Chat: Chat{ID: 7}}, "data player")
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || len(bot.data.RoundScores) != 0 || bot.data.Totals["7:8"].Points != 0 {
		t.Fatal("preview changed persistent score state")
	}
	if len(messages) != 1 || !strings.Contains(messages[0], "Ланс—Спортинг 0:2→0:2 +5") || strings.Contains(messages[0], "Ланс—Спортинг 0:2\n") {
		t.Fatalf("unexpected preview: %q", messages)
	}
}

func TestSQLitePersistsUpdateHistoryResultsAndRoundScores(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "bot.db")
	bot := &Bot{storePath: path, data: Store{
		UpdateOffset: 987,
		Forecasts:    []Forecast{{Key: "7:42:8:0", ChatID: 7, UserID: 8, Name: "Vlad Usov", Username: "usov_vlad", Date: 1, Match: Match{Home: "Арсенал", Away: "Лилль", HomeGoals: 3, AwayGoals: 0}}},
		Seasons:      []SeasonSnapshot{{ChatID: 7, Season: 2026, CurrentMatchday: 3, Matches: []publishedMatch{{ID: 42, Matchday: 2, Season: 2026, HasScore: true, HomeGoals: 3}}}},
		RoundScores:  []RoundScore{{ChatID: 7, UserID: 8, Name: "Vlad Usov", Username: "usov_vlad", Season: 2026, Matchday: 2, Points: 5, Exact: 1, Missing: 2}},
		Totals:       map[string]Totals{"7:8": {Name: "Vlad Usov", Username: "usov_vlad", Points: 5, Exact: 1}},
	}}
	if err := bot.saveLocked(); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.UpdateOffset != 987 || len(loaded.Forecasts) != 1 || loaded.Forecasts[0].Name != "Vlad Usov" || loaded.Forecasts[0].Username != "usov_vlad" || len(loaded.Seasons) != 1 || len(loaded.RoundScores) != 1 || loaded.RoundScores[0].Missing != 2 || loaded.RoundScores[0].Name != "Vlad Usov" || loaded.Totals["7:8"].Exact != 1 {
		t.Fatalf("persistent SQLite store lost bot history or score state: %+v", loaded)
	}
}
