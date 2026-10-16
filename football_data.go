package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const footballDataAPI = "https://api.football-data.org/v4/competitions/CL/matches"

type footballDataResponse struct {
	Matches     []footballDataMatch `json:"matches"`
	Competition struct {
		CurrentSeason struct {
			CurrentMatchday int `json:"currentMatchday"`
		} `json:"currentSeason"`
	} `json:"competition"`
}

type publishedMatch struct {
	ID        int       `json:"id,omitempty"`
	Date      time.Time `json:"date"`
	Season    int       `json:"season,omitempty"`
	Matchday  int       `json:"matchday,omitempty"`
	Stage     string    `json:"stage,omitempty"`
	Home      string    `json:"home"`
	Away      string    `json:"away"`
	HomeID    int       `json:"home_id,omitempty"`
	AwayID    int       `json:"away_id,omitempty"`
	HomeNames []string  `json:"home_names,omitempty"`
	AwayNames []string  `json:"away_names,omitempty"`
	Status    string    `json:"status"`
	HasScore  bool      `json:"has_score"`
	HomeGoals int       `json:"home_goals,omitempty"`
	AwayGoals int       `json:"away_goals,omitempty"`
}

type footballDataMatch struct {
	ID       int       `json:"id"`
	UTCDate  time.Time `json:"utcDate"`
	Matchday int       `json:"matchday"`
	Stage    string    `json:"stage"`
	Status   string    `json:"status"`
	HomeTeam apiTeam   `json:"homeTeam"`
	AwayTeam apiTeam   `json:"awayTeam"`
	Score    struct {
		FullTime struct {
			Home *int `json:"home"`
			Away *int `json:"away"`
		} `json:"fullTime"`
	} `json:"score"`
}

type apiTeam struct {
	ID        int    `json:"id"`
	Name      string `json:"name"`
	ShortName string `json:"shortName"`
	TLA       string `json:"tla"`
}

var teamAliases = map[string]string{
	"lens": "lens", "rc lens": "lens", "lans": "lens", "ланс": "lens",
	"sporting": "sporting", "sporting cp": "sporting", "sporting lisbon": "sporting", "sporting clube de portugal": "sporting", "спортинг": "sporting",
	"sabah": "sabah", "sabah fk": "sabah", "сабах": "sabah",
	"slavia praha": "slavia", "slavia prague": "slavia", "slavia": "slavia", "славия прага": "slavia", "славия": "slavia",
	"arsenal": "arsenal", "арсенал": "arsenal",
	"lille": "lille", "lil": "lille", "лилль": "lille",
	"atletico madrid": "atletico", "atletico de madrid": "atletico", "атлетико мадрид": "atletico", "атлетико": "atletico",
	"manchester united": "manchester united", "man united": "manchester united", "манчестер юнайтед": "manchester united",
	"inter": "inter", "internazionale": "inter", "интер": "inter",
	"club brugge": "brugge", "brugge": "brugge", "брюгге": "brugge",
	"galatasaray": "galatasaray", "галатасарай": "galatasaray",
	"barcelona": "barcelona", "fc barcelona": "barcelona", "барселона": "barcelona",
	"rb leipzig": "leipzig", "leipzig": "leipzig", "рб лейпциг": "leipzig", "лейпциг": "leipzig",
	"psv": "psv", "psv eindhoven": "psv", "псв": "psv",
	"viking": "viking", "viking fk": "viking", "викинг": "viking",
	"bayern": "bayern", "bayern munich": "bayern", "bayern munchen": "bayern", "бавария": "bayern",
	"villarreal": "villarreal", "вильярреал": "villarreal",
	"napoli": "napoli", "ssc napoli": "napoli", "наполи": "napoli",
	"feyenoord": "feyenoord", "фейеноорд": "feyenoord",
	"como": "como", "комо": "como",
	"lask": "lask", "lask linz": "lask", "ласк": "lask",
	"liverpool": "liverpool", "ливерпуль": "liverpool",
	"roma": "roma", "as roma": "roma", "рома": "roma",
	"real madrid": "real madrid", "реал мадрид": "real madrid",
	"aston villa": "aston villa", "астон вилла": "aston villa",
	"fenerbahce": "fenerbahce", "фенербахче": "fenerbahce",
	"shakhtar donetsk": "shakhtar", "shakhtar": "shakhtar", "шахтер донецк": "shakhtar", "шахтёр донецк": "shakhtar",
	"aek athens": "aek", "aek": "aek", "аек афины": "aek", "аек": "aek",
	"bodo glimt": "bodo glimt", "bodo/glimt": "bodo glimt", "буде-глимт": "bodo glimt", "буде глимт": "bodo glimt",
	"manchester city": "manchester city", "манчестер сити": "manchester city",
	"paris saint-germain": "psg", "paris saint germain": "psg", "paris sg": "psg", "psg": "psg", "пари сен-жермен": "psg", "пари сен жермен": "psg",
	"porto": "porto", "fc porto": "porto", "порту": "porto",
	"real betis": "real betis", "betis": "real betis", "реал бетис": "real betis",
	"slovan bratislava": "slovan", "slovan": "slovan", "слован братислава": "slovan",
	"stuttgart": "stuttgart", "vfb stuttgart": "stuttgart", "штутгарт": "stuttgart",
	"racing club de lens": "lens", "sk slavia praha": "slavia", "arsenal fc": "arsenal", "lille osc": "lille",
	"club atletico de madrid": "atletico", "manchester united fc": "manchester united",
	"fc internazionale milano": "inter", "club brugge kv": "brugge", "galatasaray sk": "galatasaray",
	"fc bayern munchen": "bayern", "villarreal cf": "villarreal", "feyenoord rotterdam": "feyenoord",
	"como 1907": "como", "liverpool fc": "liverpool", "real madrid cf": "real madrid",
	"aston villa fc": "aston villa", "fenerbahce sk": "fenerbahce", "fk shakhtar donetsk": "shakhtar",
	"pae aek": "aek", "fk bodo/glimt": "bodo glimt", "будё-глимт": "bodo glimt",
	"manchester city fc": "manchester city", "paris saint-germain fc": "psg", "псж": "psg",
	"real betis balompie": "real betis", "бетис": "real betis", "sk slovan bratislava": "slovan", "слован": "slovan",
	"borussia dortmund": "dortmund", "боруссия дортмунд": "dortmund",
	"шахтёр": "shakhtar", "шахтер": "shakhtar",
}

var teamDisplayNames = map[string]string{
	"lens": "Ланс", "sporting": "Спортинг", "sabah": "Сабах", "slavia": "Славия Прага",
	"arsenal": "Арсенал", "lille": "Лилль", "atletico": "Атлетико Мадрид", "manchester united": "Манчестер Юнайтед",
	"inter": "Интер", "brugge": "Брюгге", "galatasaray": "Галатасарай", "barcelona": "Барселона",
	"leipzig": "РБ Лейпциг", "psv": "ПСВ", "viking": "Викинг", "bayern": "Бавария",
	"villarreal": "Вильярреал", "napoli": "Наполи", "feyenoord": "Фейеноорд", "como": "Комо",
	"lask": "ЛАСК", "liverpool": "Ливерпуль", "roma": "Рома", "real madrid": "Реал Мадрид",
	"aston villa": "Астон Вилла", "fenerbahce": "Фенербахче", "shakhtar": "Шахтёр Донецк", "aek": "АЕК Афины",
	"bodo glimt": "Буде-Глимт", "manchester city": "Манчестер Сити", "psg": "Пари Сен-Жермен",
	"porto": "Порту", "real betis": "Реал Бетис", "slovan": "Слован Братислава", "stuttgart": "Штутгарт", "dortmund": "Боруссия Дортмунд",
}

func displayTeam(apiName string) string {
	if russianName, ok := teamDisplayNames[teamKey(apiName)]; ok {
		return russianName
	}
	return apiName
}

var cyrillicToLatin = map[rune]string{
	'а': "a", 'б': "b", 'в': "v", 'г': "g", 'д': "d", 'е': "e", 'ё': "e", 'ж': "zh", 'з': "z",
	'и': "i", 'й': "y", 'к': "k", 'л': "l", 'м': "m", 'н': "n", 'о': "o", 'п': "p", 'р': "r",
	'с': "s", 'т': "t", 'у': "u", 'ф': "f", 'х': "h", 'ц': "ts", 'ч': "ch", 'ш': "sh", 'щ': "shch",
	'ъ': "", 'ы': "y", 'ь': "", 'э': "e", 'ю': "yu", 'я': "ya",
}

func teamKey(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.NewReplacer(
		"á", "a", "à", "a", "â", "a", "ä", "a", "ã", "a", "å", "a",
		"é", "e", "è", "e", "ê", "e", "ë", "e", "í", "i", "ì", "i", "ï", "i",
		"ó", "o", "ò", "o", "ô", "o", "ö", "o", "õ", "o", "ø", "o",
		"ú", "u", "ù", "u", "ü", "u", "ñ", "n", "ç", "c", "ß", "ss", "š", "s", "ğ", "g", "ş", "s", "ı", "i",
	).Replace(value)
	if canonical, ok := teamAliases[value]; ok {
		return canonical
	}
	var transliterated strings.Builder
	for _, r := range value {
		if converted, ok := cyrillicToLatin[r]; ok {
			transliterated.WriteString(converted)
		} else {
			transliterated.WriteRune(r)
		}
	}
	value = strings.Join(strings.Fields(transliterated.String()), " ")
	if canonical, ok := teamAliases[value]; ok {
		return canonical
	}
	return value
}

func fetchSeasonResults(ctx context.Context, client *http.Client, token string, season int) ([]publishedMatch, int, error) {
	return fetchSeasonMatches(ctx, client, token, season, 0, "")
}

func fetchSeasonMatchdayResults(ctx context.Context, client *http.Client, token string, season, matchday int) ([]publishedMatch, int, error) {
	if matchday < 1 {
		return nil, 0, fmt.Errorf("matchday must be positive")
	}
	return fetchSeasonMatches(ctx, client, token, season, matchday, "")
}

func fetchSeasonStageResults(ctx context.Context, client *http.Client, token string, season int, stage string) ([]publishedMatch, int, error) {
	if stage == "" {
		return nil, 0, fmt.Errorf("stage must not be empty")
	}
	return fetchSeasonMatches(ctx, client, token, season, 0, stage)
}

func fetchSeasonMatches(ctx context.Context, client *http.Client, token string, season, matchday int, stage string) ([]publishedMatch, int, error) {
	endpoint, err := url.Parse(footballDataAPI)
	if err != nil {
		return nil, 0, err
	}
	query := endpoint.Query()
	query.Set("season", strconv.Itoa(season))
	if matchday > 0 {
		query.Set("matchday", strconv.Itoa(matchday))
	}
	if stage != "" {
		query.Set("stage", stage)
	}
	endpoint.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("X-Auth-Token", token)
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("football-data API request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, 0, fmt.Errorf("football-data API returned %s: %s", resp.Status, strings.TrimSpace(string(message)))
	}
	var payload footballDataResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&payload); err != nil {
		return nil, 0, fmt.Errorf("decode football-data response: %w", err)
	}
	matches := make([]publishedMatch, 0, len(payload.Matches))
	for _, item := range payload.Matches {
		if item.Stage == "" && stage != "" {
			item.Stage = stage
		}
		match := publishedMatch{
			ID: item.ID, Date: item.UTCDate, Season: season, Matchday: item.Matchday, Stage: item.Stage,
			Home: displayTeam(item.HomeTeam.Name), Away: displayTeam(item.AwayTeam.Name),
			HomeID: item.HomeTeam.ID, AwayID: item.AwayTeam.ID,
			HomeNames: []string{item.HomeTeam.Name, item.HomeTeam.ShortName, item.HomeTeam.TLA},
			AwayNames: []string{item.AwayTeam.Name, item.AwayTeam.ShortName, item.AwayTeam.TLA}, Status: item.Status,
		}
		if stageRound := roundIndexForStage(match.Stage); stageRound > 0 {
			match.Matchday = stageRound
		} else if matchday > 0 && match.Matchday == 0 {
			match.Matchday = matchday
		}
		if item.Score.FullTime.Home != nil && item.Score.FullTime.Away != nil && item.Status == "FINISHED" {
			match.HasScore = true
			match.HomeGoals = *item.Score.FullTime.Home
			match.AwayGoals = *item.Score.FullTime.Away
		}
		matches = append(matches, match)
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].Date.Before(matches[j].Date) })
	currentMatchday := payload.Competition.CurrentSeason.CurrentMatchday
	if season != footballSeasonYear(time.Now().UTC()) {
		currentMatchday = 0
	} else if currentMatchday == 0 && matchday == 0 {
		currentMatchday = inferCurrentMatchday(matches)
	}
	return matches, currentMatchday, nil
}

// The internal matchday column is also used as a stable round key for scoring.
// Knockout stages occupy 9..13 after the eight league-phase matchdays.
func roundIndexForStage(stage string) int {
	switch strings.ToUpper(strings.TrimSpace(stage)) {
	case "PLAYOFFS", "PLAY_OFFS", "PLAY_OFF_ROUND", "PLAY_OFF_ROUND_1", "PLAY_OFF_ROUND_2", "PLAYOFF_ROUND_1", "PLAYOFF_ROUND_2", "LAST_32", "ROUND_OF_32":
		return 9
	case "LAST_16", "ROUND_OF_16":
		return 10
	case "QUARTER_FINALS":
		return 11
	case "SEMI_FINALS":
		return 12
	case "FINAL":
		return 13
	default:
		return 0
	}
}

func apiStageForRound(round int) string {
	switch round {
	case 9:
		return "PLAYOFFS"
	case 10:
		return "LAST_16"
	case 11:
		return "QUARTER_FINALS"
	case 12:
		return "SEMI_FINALS"
	case 13:
		return "FINAL"
	default:
		return ""
	}
}

func displayRound(round int) string {
	switch round {
	case 9:
		return "1/16 финала"
	case 10:
		return "1/8 финала"
	case 11:
		return "1/4 финала"
	case 12:
		return "1/2 финала"
	case 13:
		return "финал"
	default:
		if round > 0 {
			return fmt.Sprintf("тур %d", round)
		}
		return "тур не определён"
	}
}

func parseRoundSpecifier(value string) (int, string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1/16", "1/16финала", "1/16-финала":
		return 9, apiStageForRound(9), nil
	case "1/8", "1/8финала", "1/8-финала":
		return 10, apiStageForRound(10), nil
	case "1/4", "1/4финала", "1/4-финала":
		return 11, apiStageForRound(11), nil
	case "1/2", "1/2финала", "1/2-финала":
		return 12, apiStageForRound(12), nil
	case "1/1", "финал":
		return 13, apiStageForRound(13), nil
	default:
		round, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || round < 1 {
			return 0, "", fmt.Errorf("укажите номер тура или стадию: 1/16, 1/8, 1/4, 1/2, 1/1")
		}
		return round, "", nil
	}
}

func inferCurrentMatchday(matches []publishedMatch) int {
	current, last := 0, 0
	for _, match := range matches {
		if match.Matchday <= 0 {
			continue
		}
		if match.Matchday > last {
			last = match.Matchday
		}
		if match.Status != "FINISHED" && (current == 0 || match.Matchday < current) {
			current = match.Matchday
		}
	}
	if current != 0 {
		return current
	}
	return last
}

func footballSeasonYear(date time.Time) int {
	if date.Month() < time.July {
		return date.Year() - 1
	}
	return date.Year()
}
