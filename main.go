package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"html"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const telegramAPI = "https://api.telegram.org/bot"

type User struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
}

func (u User) Name() string {
	name := strings.TrimSpace(strings.Join([]string{u.FirstName, u.LastName}, " "))
	if name != "" {
		return name
	}
	if u.Username != "" {
		return "@" + u.Username
	}
	return ""
}

func (u User) UsernameLabel() string {
	return strings.TrimPrefix(strings.TrimSpace(u.Username), "@")
}

func participantLabel(name, username string) string {
	name = strings.TrimSpace(name)
	username = strings.TrimPrefix(strings.TrimSpace(username), "@")
	if username == "" {
		return name
	}
	if name == "" || normalize(strings.TrimPrefix(name, "@")) == normalize(username) {
		return username
	}
	return fmt.Sprintf("%s (%s)", name, username)
}

type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type Message struct {
	MessageID       int            `json:"message_id"`
	MessageThreadID int            `json:"message_thread_id,omitempty"`
	Date            int64          `json:"date"`
	Text            string         `json:"text"`
	Caption         string         `json:"caption"`
	From            User           `json:"from"`
	Chat            Chat           `json:"chat"`
	ForwardOrigin   *MessageOrigin `json:"forward_origin"`
	ReplyTo         *Message       `json:"reply_to_message"`
}

// MessageOrigin contains the Telegram fields needed to identify a forwarded
// forecast's original author. Other origin fields are intentionally ignored.
type MessageOrigin struct {
	Type           string `json:"type"`
	Date           int64  `json:"date"`
	SenderUser     *User  `json:"sender_user"`
	SenderUserName string `json:"sender_user_name"`
}

func forecastAuthor(m Message) (User, int64, bool) {
	date := m.Date
	if origin := m.ForwardOrigin; origin != nil {
		if origin.Date > 0 {
			date = origin.Date
		}
		switch origin.Type {
		case "user":
			if origin.SenderUser == nil || origin.SenderUser.ID == 0 {
				return User{}, 0, false
			}
			return *origin.SenderUser, date, true
		case "hidden_user":
			name := strings.TrimSpace(origin.SenderUserName)
			if name == "" {
				return User{}, 0, false
			}
			h := fnv.New64a()
			_, _ = h.Write([]byte(strings.ToLower(name)))
			// Telegram deliberately omits the account ID for hidden origins.
			// A stable negative ID groups repeated forwards with the same name.
			id := -int64(h.Sum64() & (1<<63 - 1))
			if id == 0 {
				id = -1
			}
			return User{ID: id, FirstName: name}, date, true
		default:
			// Chat and channel posts do not identify an individual player.
			return User{}, 0, false
		}
	}
	if m.From.ID == 0 {
		return User{}, 0, false
	}
	return m.From, date, true
}

type Update struct {
	ID      int     `json:"update_id"`
	Message Message `json:"message"`
}

type apiResponse[T any] struct {
	OK          bool   `json:"ok"`
	Result      T      `json:"result"`
	Description string `json:"description"`
}

type Match struct {
	Home      string `json:"home"`
	Away      string `json:"away"`
	HomeGoals int    `json:"home_goals"`
	AwayGoals int    `json:"away_goals"`
	Number    int    `json:"number,omitempty"`
	FixtureID int    `json:"fixture_id,omitempty"`
}

type Forecast struct {
	Key      string `json:"key,omitempty"`
	ChatID   int64  `json:"chat_id"`
	UserID   int64  `json:"user_id"`
	Name     string `json:"name"`
	Username string `json:"username,omitempty"`
	Date     int64  `json:"date"`
	Match    Match  `json:"match"`
}

type UEFAResultSet struct {
	ChatID    int64            `json:"chat_id"`
	Season    int              `json:"season,omitempty"`
	Matchday  int              `json:"matchday,omitempty"`
	From      time.Time        `json:"from"`
	To        time.Time        `json:"to"`
	FetchedAt time.Time        `json:"fetched_at"`
	Matches   []publishedMatch `json:"matches"`
}

type FixtureList struct {
	ChatID  int64            `json:"chat_id"`
	Matches []publishedMatch `json:"matches"`
}

type ForecastDetail struct {
	Season       int    `json:"season,omitempty"`
	Matchday     int    `json:"matchday,omitempty"`
	ForecastDate int64  `json:"forecast_date,omitempty"`
	UserID       int64  `json:"user_id"`
	Name         string `json:"name"`
	Username     string `json:"username,omitempty"`
	Prediction   Match  `json:"prediction"`
	Actual       *Match `json:"actual,omitempty"`
	Points       int    `json:"points"`
	Reason       string `json:"reason"`
}

type Totals struct {
	Name     string `json:"name"`
	Username string `json:"username,omitempty"`
	Points   int    `json:"points"`
	Exact    int    `json:"exact"`
}

type SeasonSnapshot struct {
	ChatID          int64            `json:"chat_id"`
	Season          int              `json:"season"`
	FetchedAt       time.Time        `json:"fetched_at"`
	CurrentMatchday int              `json:"current_matchday,omitempty"`
	Matches         []publishedMatch `json:"matches"`
}

type RoundScore struct {
	ChatID   int64            `json:"chat_id"`
	UserID   int64            `json:"user_id"`
	Name     string           `json:"name"`
	Username string           `json:"username,omitempty"`
	Season   int              `json:"season"`
	Matchday int              `json:"matchday"`
	Points   int              `json:"points"`
	Exact    int              `json:"exact"`
	Missing  int              `json:"missing_matches,omitempty"`
	ScoredAt time.Time        `json:"scored_at"`
	Details  []ForecastDetail `json:"details,omitempty"`
}

type recentLog struct {
	mu    sync.Mutex
	lines []string
}

func (r *recentLog) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\r\n"), "\n") {
		if line == "" {
			continue
		}
		if len(line) > 2000 {
			line = line[len(line)-2000:]
		}
		r.mu.Lock()
		r.lines = append(r.lines, line)
		if len(r.lines) > 100 {
			r.lines = append([]string(nil), r.lines[len(r.lines)-100:]...)
		}
		r.mu.Unlock()
	}
	return len(p), nil
}

func (r *recentLog) Last(count int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if count > len(r.lines) {
		count = len(r.lines)
	}
	return append([]string(nil), r.lines[len(r.lines)-count:]...)
}

type Store struct {
	Forecasts    []Forecast        `json:"forecasts"`
	Totals       map[string]Totals `json:"totals"`
	ResultSets   []UEFAResultSet   `json:"uefa_results,omitempty"`
	Lists        []FixtureList     `json:"fixture_lists,omitempty"`
	Seasons      []SeasonSnapshot  `json:"season_results,omitempty"`
	RoundScores  []RoundScore      `json:"round_scores,omitempty"`
	UpdateOffset int               `json:"last_update_id,omitempty"`
}

type Bot struct {
	token             string
	footballDataToken string
	storePath         string
	storage           *SQLiteStore
	client            *http.Client
	mu                sync.Mutex
	footballMu        sync.Mutex
	lastFootballCall  time.Time
	apiInterval       time.Duration
	data              Store
	resultRunning     map[int64]bool
	startedAt         time.Time
	recentLog         *recentLog
}

var scorePattern = regexp.MustCompile(`(?i)(?:^|\s|:)(\d{1,2})\s*[:：-]\s*(\d{1,2})\s*$`)
var separatorPattern = regexp.MustCompile(`\s+[—–−-]\s+`)

func parseMatch(line string) (Match, bool) {
	line = strings.TrimSpace(line)
	number := 0
	if prefix := regexp.MustCompile(`^\s*(\d+)\s*[.)]\s*`).FindStringSubmatchIndex(line); prefix != nil {
		number, _ = strconv.Atoi(line[prefix[2]:prefix[3]])
		line = line[prefix[1]:]
	}
	fixtureID := 0
	if marker := regexp.MustCompile(`^\[#(\d+)\]\s*`).FindStringSubmatchIndex(line); marker != nil {
		fixtureID, _ = strconv.Atoi(line[marker[2]:marker[3]])
		line = line[marker[1]:]
	}
	scoreLoc := scorePattern.FindStringSubmatchIndex(line)
	if scoreLoc == nil {
		return Match{}, false
	}
	homeGoals, _ := strconv.Atoi(line[scoreLoc[2]:scoreLoc[3]])
	awayGoals, _ := strconv.Atoi(line[scoreLoc[4]:scoreLoc[5]])
	teams := strings.TrimSpace(line[:scoreLoc[0]])
	teams = strings.TrimSpace(strings.TrimSuffix(teams, ":"))
	parts := separatorPattern.Split(teams, 2)
	if len(parts) != 2 {
		// Some messages omit spaces around the dash; split at the first common dash.
		for _, dash := range []string{"—", "–", "−", "-"} {
			if i := strings.Index(teams, dash); i > 0 && i+len(dash) < len(teams) {
				parts = []string{teams[:i], teams[i+len(dash):]}
				break
			}
		}
	}
	if len(parts) != 2 {
		return Match{}, false
	}
	home, away := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
	if home == "" || away == "" || len(home) > 100 || len(away) > 100 {
		return Match{}, false
	}
	return Match{Home: home, Away: away, HomeGoals: homeGoals, AwayGoals: awayGoals, Number: number, FixtureID: fixtureID}, true
}

func parseMatches(text string) []Match {
	var out []Match
	for _, line := range strings.Split(text, "\n") {
		if m, ok := parseMatch(line); ok {
			out = append(out, m)
		}
	}
	return out
}

func normalize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.Join(strings.Fields(s), " ")
}

func sameTeams(a, b Match) bool {
	return normalize(a.Home) == normalize(b.Home) && normalize(a.Away) == normalize(b.Away)
}

func forecastStorageKey(f Forecast) string {
	if f.Key != "" {
		return f.Key
	}
	return fmt.Sprintf("%d:%d:%d:%s:%s:%d:%d", f.ChatID, f.UserID, f.Date, normalize(f.Match.Home), normalize(f.Match.Away), f.Match.HomeGoals, f.Match.AwayGoals)
}

func points(prediction, actual Match) int {
	if prediction.HomeGoals == actual.HomeGoals && prediction.AwayGoals == actual.AwayGoals {
		return 5
	}
	pd := prediction.HomeGoals - prediction.AwayGoals
	ad := actual.HomeGoals - actual.AwayGoals
	if (pd > 0 && ad > 0 || pd < 0 && ad < 0 || pd == 0 && ad == 0) && pd == ad {
		return 3
	}
	if pd > 0 && ad > 0 || pd < 0 && ad < 0 {
		return 1
	}
	return 0
}

func (b *Bot) saveLocked() error {
	if b.storage != nil {
		return b.storage.Save(b.data)
	}
	return saveStoreFile(b.storePath, b.data)
}

func filepathDir(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		if i == 0 {
			return "/"
		}
		return path[:i]
	}
	return "."
}

func (b *Bot) call(ctx context.Context, method string, payload any, result any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, telegramAPI+b.token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("telegram %s: %s", resp.Status, data)
	}
	var envelope apiResponse[json.RawMessage]
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return err
	}
	if !envelope.OK {
		return fmt.Errorf("telegram API: %s", envelope.Description)
	}
	if result != nil {
		return json.Unmarshal(envelope.Result, result)
	}
	return nil
}

func messageBody(m Message) string {
	if m.Text != "" {
		return m.Text
	}
	return m.Caption
}

func isReplyTrigger(text string) bool {
	return strings.TrimSpace(text) != ""
}

func (b *Bot) logText(text string) string {
	for _, secret := range []string{b.token, b.footballDataToken} {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "[скрыто]")
		}
	}
	runes := []rune(text)
	if len(runes) > 500 {
		text = string(runes[:500]) + "…"
	}
	return strconv.Quote(text)
}

func (b *Bot) send(chatID int64, text string) error {
	return b.sendTo(chatID, 0, text, "")
}

func (b *Bot) sendFormatted(chatID int64, text, parseMode string) error {
	return b.sendTo(chatID, 0, text, parseMode)
}

func (b *Bot) sendTo(chatID int64, threadID int, text, parseMode string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	log.Printf("telegram output attempt: chat_id=%d thread_id=%d text=%s", chatID, threadID, b.logText(text))
	params := map[string]any{"chat_id": chatID, "text": text}
	if threadID != 0 {
		params["message_thread_id"] = threadID
	}
	if parseMode != "" {
		params["parse_mode"] = parseMode
	}
	err := b.call(ctx, "sendMessage", params, nil)
	if err != nil {
		log.Printf("telegram output failed: chat_id=%d thread_id=%d error=%s", chatID, threadID, b.safeError(err))
		return err
	}
	log.Printf("telegram output sent: chat_id=%d thread_id=%d", chatID, threadID)
	return nil
}

func (b *Bot) safeError(err error) string {
	return strings.ReplaceAll(err.Error(), telegramAPI+b.token, telegramAPI+"<token-redacted>")
}

func command(text string) (string, string) {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return "", ""
	}
	cmd := strings.TrimPrefix(fields[0], "/")
	if i := strings.IndexByte(cmd, '@'); i >= 0 {
		cmd = cmd[:i]
	}
	return strings.ToLower(cmd), strings.TrimSpace(strings.TrimPrefix(text, fields[0]))
}

func (b *Bot) handle(m Message) bool {
	body := messageBody(m)
	replyToID := 0
	replyFromID := int64(0)
	replyFromName := ""
	replyDate := int64(0)
	replyText := ""
	if m.ReplyTo != nil {
		replyToID = m.ReplyTo.MessageID
		replyFromID = m.ReplyTo.From.ID
		replyFromName = m.ReplyTo.From.Name()
		replyDate = m.ReplyTo.Date
		replyText = messageBody(*m.ReplyTo)
	}
	log.Printf("telegram input: chat_id=%d message_id=%d date=%d from_id=%d from=%q reply_to=%d reply_date=%d reply_from_id=%d reply_from=%q text=%s reply_text=%s", m.Chat.ID, m.MessageID, m.Date, m.From.ID, m.From.Name(), replyToID, replyDate, replyFromID, replyFromName, b.logText(body), b.logText(replyText))
	if body == "" {
		log.Printf("telegram input decision: chat_id=%d message_id=%d action=ignored reason=empty_text", m.Chat.ID, m.MessageID)
		return true
	}
	cmd, args := command(body)
	if cmd != "" && (cmd == "result" || cmd == "stats" || cmd == "tour" || strings.HasPrefix(body, "/")) {
		log.Printf("telegram input decision: chat_id=%d message_id=%d action=command command=%q", m.Chat.ID, m.MessageID, cmd)
		switch cmd {
		case "result":
			b.handleResult(m, args)
		case "help":
			b.handleHelp(m)
		case "debug":
			b.handleDebug(m)
		case "work":
			_ = b.sendTo(m.Chat.ID, m.MessageThreadID, "Команда переименована: используйте /result.", "")
		case "stats":
			b.handleStats(m)
		case "tour":
			b.handleTour(m, args)
		}
		return true
	}
	source := m
	forecastText := body
	forecasts := parseMatches(forecastText)
	usedReplyAnchor := false
	if len(forecasts) == 0 && m.ReplyTo != nil && isReplyTrigger(body) {
		forecastText = messageBody(*m.ReplyTo)
		forecasts = parseMatches(forecastText)
		if len(forecasts) > 0 {
			source = *m.ReplyTo
			usedReplyAnchor = true
		}
	}
	if len(forecasts) == 0 {
		log.Printf("telegram input decision: chat_id=%d message_id=%d action=ignored reason=no_forecasts", m.Chat.ID, m.MessageID)
		return true
	}
	parsed, _ := json.Marshal(forecasts)
	author, forecastDate, ok := forecastAuthor(source)
	if !ok {
		log.Printf("telegram input decision: chat_id=%d message_id=%d action=ignored reason=unknown_author parsed=%s", m.Chat.ID, m.MessageID, b.logText(string(parsed)))
		return true
	}
	sourceMessageID := source.MessageID
	if usedReplyAnchor {
		log.Printf("telegram reply anchor: chat_id=%d reply_message_id=%d trigger_message_id=%d source=reply_to", m.Chat.ID, sourceMessageID, m.MessageID)
	}
	log.Printf("telegram input decision: chat_id=%d message_id=%d source_message_id=%d forecast_date=%d action=save_forecasts author_id=%d forecasts=%s", m.Chat.ID, m.MessageID, sourceMessageID, forecastDate, author.ID, b.logText(string(parsed)))
	b.mu.Lock()
	defer b.mu.Unlock()
	saved := 0
	duplicates := 0
	for index, match := range forecasts {
		if match.FixtureID == 0 && match.Number > 0 {
			for _, list := range b.data.Lists {
				if list.ChatID == m.Chat.ID && match.Number <= len(list.Matches) {
					match.FixtureID = list.Matches[match.Number-1].ID
					break
				}
			}
		}
		forecast := Forecast{ChatID: m.Chat.ID, UserID: author.ID, Name: author.Name(), Username: author.UsernameLabel(), Date: forecastDate, Match: match}
		forecast.Key = fmt.Sprintf("%d:%d:%d:%d", m.Chat.ID, sourceMessageID, author.ID, index)
		duplicate := false
		for _, existing := range b.data.Forecasts {
			if existing.Key == forecast.Key {
				duplicate = true
				break
			}
		}
		if !duplicate {
			b.updateParticipantNameLocked(m.Chat.ID, author)
			b.data.Forecasts = append(b.data.Forecasts, forecast)
			saved++
		} else {
			duplicates++
		}
	}
	if err := b.saveLocked(); err != nil {
		log.Printf("telegram input decision: chat_id=%d message_id=%d source_message_id=%d action=save_failed error=%v", m.Chat.ID, m.MessageID, sourceMessageID, err)
		return false
	}
	log.Printf("telegram input decision: chat_id=%d message_id=%d source_message_id=%d action=saved_forecasts saved=%d duplicates=%d", m.Chat.ID, m.MessageID, sourceMessageID, saved, duplicates)
	return true
}

// Keep old standings readable when Telegram provides a profile name on a new forecast.
func (b *Bot) updateParticipantNameLocked(chatID int64, user User) {
	name := user.Name()
	if name == "" || user.ID == 0 {
		return
	}
	username := user.UsernameLabel()
	for i := range b.data.Forecasts {
		forecast := &b.data.Forecasts[i]
		if forecast.ChatID == chatID && forecast.UserID == user.ID {
			forecast.Name, forecast.Username = name, username
		}
	}
	for i := range b.data.RoundScores {
		score := &b.data.RoundScores[i]
		if score.ChatID != chatID || score.UserID != user.ID {
			continue
		}
		score.Name, score.Username = name, username
		for j := range score.Details {
			score.Details[j].Name, score.Details[j].Username = name, username
		}
	}
	key := fmt.Sprintf("%d:%d", chatID, user.ID)
	if total, ok := b.data.Totals[key]; ok {
		total.Name, total.Username = name, username
		b.data.Totals[key] = total
	}
}

func (b *Bot) handleTour(m Message, args string) {
	args = strings.TrimSpace(args)
	if args != "" && args != "data" {
		round, stage, err := parseRoundSpecifier(args)
		if err != nil {
			_ = b.sendTo(m.Chat.ID, m.MessageThreadID, "Формат: /tour [номер_тура|1/16|1/8|1/4|1/2|1/1] или /tour data.", "")
			return
		}
		if b.footballDataToken == "" {
			_ = b.sendTo(m.Chat.ID, m.MessageThreadID, "Не настроен источник матчей. Добавьте FOOTBALL_DATA_TOKEN в .env и перезапустите бота.", "")
			return
		}
		go b.fetchAndSendTourFixtures(m.Chat.ID, m.MessageThreadID, round, stage)
		return
	}
	b.mu.Lock()
	var fixtures []publishedMatch
	var latest time.Time
	for i := range b.data.Seasons {
		set := &b.data.Seasons[i]
		if set.ChatID != m.Chat.ID || len(set.Matches) == 0 {
			continue
		}
		if set.FetchedAt.After(latest) {
			latest = set.FetchedAt
			fixtures = append(fixtures[:0], set.Matches...)
		}
	}
	if len(fixtures) == 0 {
		for i := range b.data.ResultSets {
			set := &b.data.ResultSets[i]
			if set.ChatID == m.Chat.ID && set.Matchday == 0 && len(set.Matches) > 0 && set.FetchedAt.After(latest) {
				latest = set.FetchedAt
				fixtures = append(fixtures[:0], set.Matches...)
			}
		}
	}
	if len(fixtures) == 0 {
		for i := range b.data.Lists {
			if b.data.Lists[i].ChatID == m.Chat.ID {
				fixtures = append(fixtures[:0], b.data.Lists[i].Matches...)
			}
		}
	}
	b.mu.Unlock()
	if len(fixtures) == 0 {
		_ = b.sendTo(m.Chat.ID, m.MessageThreadID, "Нет сохранённого списка матчей. Используйте /tour номер_тура.", "")
		return
	}
	b.publishFixtureList(m.Chat.ID, m.MessageThreadID, fixtures)
}

func (b *Bot) fetchAndSendTourFixtures(chatID int64, threadID, matchday int, stage string) {
	b.mu.Lock()
	season := footballSeasonYear(time.Now().UTC())
	var fixtures []publishedMatch
	for _, snapshot := range b.data.Seasons {
		if snapshot.ChatID == chatID && snapshot.Season == season {
			fixtures = fixturesForMatchday(snapshot.Matches, matchday)
			break
		}
	}
	if len(fixtures) == 0 {
		for _, set := range b.data.ResultSets {
			if set.ChatID == chatID && set.Season == season && set.Matchday == matchday {
				fixtures = append([]publishedMatch(nil), set.Matches...)
				break
			}
		}
	}
	b.mu.Unlock()
	if len(fixtures) > 0 {
		b.publishFixtureList(chatID, threadID, fixtures)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	var err error
	if stage != "" {
		fixtures, _, err = b.fetchStageRateLimited(ctx, season, stage)
		if err == nil && len(fixtures) == 0 {
			fixtures, _, err = b.fetchSeasonRateLimited(ctx, season)
			if err == nil {
				all := fixtures
				fixtures = fixturesForMatchday(all, matchday)
			}
		}
	} else {
		fixtures, _, err = b.fetchMatchdayRateLimited(ctx, season, matchday)
	}
	if err != nil {
		log.Printf("fetch fixture list season %d matchday %d: %v", season, matchday, err)
		_ = b.sendTo(chatID, threadID, "Не удалось получить список матчей: "+err.Error(), "")
		return
	}
	if len(fixtures) == 0 {
		_ = b.sendTo(chatID, threadID, fmt.Sprintf("API не вернул матчи Лиги чемпионов для %s сезона %d/%d.", displayRound(matchday), season, season+1), "")
		return
	}
	fetchedAt := time.Now().UTC()
	for i := range fixtures {
		fixtures[i].Season = season
		if fixtures[i].Matchday == 0 {
			fixtures[i].Matchday = matchday
		}
	}
	b.mu.Lock()
	upsertMatchdayResultSet(&b.data, UEFAResultSet{ChatID: chatID, Season: season, Matchday: matchday, FetchedAt: fetchedAt, Matches: append([]publishedMatch(nil), fixtures...)})
	if err := b.saveLocked(); err != nil {
		b.mu.Unlock()
		log.Printf("save fixture list results: %v", err)
		_ = b.sendTo(chatID, threadID, "Не удалось сохранить результаты тура: "+err.Error(), "")
		return
	}
	b.mu.Unlock()
	b.publishFixtureList(chatID, threadID, fixtures)
}

func (b *Bot) publishFixtureList(chatID int64, threadID int, fixtures []publishedMatch) {
	b.mu.Lock()
	kept := b.data.Lists[:0]
	for _, old := range b.data.Lists {
		if old.ChatID != chatID {
			kept = append(kept, old)
		}
	}
	b.data.Lists = append(kept, FixtureList{ChatID: chatID, Matches: append([]publishedMatch(nil), fixtures...)})
	err := b.saveLocked()
	b.mu.Unlock()
	if err != nil {
		log.Printf("save fixture list: %v", err)
		_ = b.sendTo(chatID, threadID, "Не удалось сохранить список матчей: "+err.Error(), "")
		return
	}
	b.sendLongTo(chatID, threadID, formatForecastTemplate(fixtures))
}

func (b *Bot) handleResult(m Message, args string) {
	opts, err := parseResultOptions(args)
	if err != nil {
		_ = b.sendTo(m.Chat.ID, m.MessageThreadID, "Ошибка: "+err.Error(), "")
		return
	}
	b.mu.Lock()
	forecasts := make([]Forecast, 0)
	userExists := opts.username == ""
	for _, f := range b.data.Forecasts {
		if f.ChatID != m.Chat.ID {
			continue
		}
		forecasts = append(forecasts, f)
		if opts.includesUser(f.Name, f.Username) {
			userExists = true
		}
	}
	if !userExists || len(forecasts) == 0 {
		b.mu.Unlock()
		_ = b.sendTo(m.Chat.ID, m.MessageThreadID, "Не нашёл прогнозов для этого тура и пользователя.", "")
		return
	}
	if opts.dataOnly {
		fixtures := make([]publishedMatch, 0)
		for _, snapshot := range b.data.Seasons {
			fixtures = append(fixtures, snapshot.Matches...)
		}
		for _, set := range b.data.ResultSets {
			if set.ChatID == m.Chat.ID {
				fixtures = append(fixtures, set.Matches...)
			}
		}
		for _, list := range b.data.Lists {
			if list.ChatID == m.Chat.ID {
				fixtures = append(fixtures, list.Matches...)
			}
		}
		preview := &Bot{data: Store{RoundScores: append([]RoundScore(nil), b.data.RoundScores...), Totals: map[string]Totals{}}}
		for key, total := range b.data.Totals {
			preview.data.Totals[key] = total
		}
		scores, pending := preview.scoreRoundsLocked(m.Chat.ID, forecasts, fixtures)
		snapshots := append([]SeasonSnapshot(nil), b.data.Seasons...)
		b.mu.Unlock()
		b.sendRoundResultReport(m.Chat.ID, m.MessageThreadID, scores, snapshots, fixtures, pending, opts)
		return
	}
	if b.resultRunning == nil {
		b.resultRunning = make(map[int64]bool)
	}
	if b.resultRunning[m.Chat.ID] {
		b.mu.Unlock()
		_ = b.sendTo(m.Chat.ID, m.MessageThreadID, "Обновление результатов для этого чата уже выполняется.", "")
		return
	}
	b.resultRunning[m.Chat.ID] = true
	b.mu.Unlock()
	_ = b.sendTo(m.Chat.ID, m.MessageThreadID, "Обновляю счета Лиги чемпионов и считаю очки по турам…", "")
	go b.fetchAndScoreRoundsTo(m.Chat.ID, m.MessageThreadID, forecasts, opts)
}

type resultOptions struct {
	matchday int
	stage    string
	username string
	dataOnly bool
	refresh  bool
}

func parseResultOptions(args string) (resultOptions, error) {
	fields := strings.Fields(args)
	var opts resultOptions
	if len(fields) > 0 && strings.EqualFold(fields[0], "data") {
		if len(fields) != 2 {
			return opts, errors.New("формат: /result data username")
		}
		opts.dataOnly, opts.username = true, fields[1]
		return opts, nil
	}
	filtered := fields[:0]
	for _, field := range fields {
		if field == "--refresh" {
			opts.refresh = true
			continue
		}
		filtered = append(filtered, field)
	}
	fields = filtered
	if len(fields) == 0 {
		if opts.refresh {
			return opts, errors.New("флаг --refresh можно использовать только с номером тура")
		}
		return opts, nil
	}
	if len(fields) == 1 {
		round, stage, err := parseRoundSpecifier(fields[0])
		if err != nil {
			return opts, err
		}
		opts.matchday, opts.stage = round, stage
		return opts, nil
	}
	if len(fields) != 2 {
		return opts, errors.New("формат: /result [номер_тура] [username] [--refresh]")
	}
	round, stage, err := parseRoundSpecifier(fields[0])
	if err != nil {
		return opts, err
	}
	if fields[1] == "--refresh" {
		return opts, errors.New("формат: /result [номер_тура] [username] [--refresh]")
	}
	opts.matchday, opts.stage, opts.username = round, stage, fields[1]
	return opts, nil
}

func (o resultOptions) includesUser(name string, usernames ...string) bool {
	if o.username == "" {
		return true
	}
	wanted := normalize(strings.TrimPrefix(strings.TrimSpace(o.username), "@"))
	if normalize(strings.TrimPrefix(strings.TrimSpace(name), "@")) == wanted {
		return true
	}
	for _, username := range usernames {
		if normalize(strings.TrimPrefix(strings.TrimSpace(username), "@")) == wanted {
			return true
		}
	}
	return false
}

func roundScoreKey(chatID, userID int64, season, matchday int) string {
	return fmt.Sprintf("%d:%d:%d:%d", chatID, userID, season, matchday)
}

func seasonForForecast(f Forecast, lists []FixtureList) int {
	if f.Match.FixtureID != 0 {
		for _, list := range lists {
			if list.ChatID != f.ChatID {
				continue
			}
			for _, fixture := range list.Matches {
				if fixture.ID == f.Match.FixtureID && !fixture.Date.IsZero() {
					return footballSeasonYear(fixture.Date)
				}
			}
		}
	}
	return footballSeasonYear(time.Unix(f.Date, 0).UTC())
}

func (b *Bot) fetchSeasonRateLimited(ctx context.Context, season int) ([]publishedMatch, int, error) {
	b.footballMu.Lock()
	defer b.footballMu.Unlock()
	if wait := b.apiInterval - time.Since(b.lastFootballCall); wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		case <-timer.C:
		}
	}
	b.lastFootballCall = time.Now()
	return fetchSeasonResults(ctx, b.client, b.footballDataToken, season)
}

func (b *Bot) fetchMatchdayRateLimited(ctx context.Context, season, matchday int) ([]publishedMatch, int, error) {
	b.footballMu.Lock()
	defer b.footballMu.Unlock()
	if wait := b.apiInterval - time.Since(b.lastFootballCall); wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		case <-timer.C:
		}
	}
	b.lastFootballCall = time.Now()
	return fetchSeasonMatchdayResults(ctx, b.client, b.footballDataToken, season, matchday)
}

func (b *Bot) fetchStageRateLimited(ctx context.Context, season int, stage string) ([]publishedMatch, int, error) {
	b.footballMu.Lock()
	defer b.footballMu.Unlock()
	if wait := b.apiInterval - time.Since(b.lastFootballCall); wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		case <-timer.C:
		}
	}
	b.lastFootballCall = time.Now()
	return fetchSeasonStageResults(ctx, b.client, b.footballDataToken, season, stage)
}

func fixturesForMatchday(fixtures []publishedMatch, matchday int) []publishedMatch {
	out := make([]publishedMatch, 0)
	for _, fixture := range fixtures {
		if fixture.Matchday == matchday {
			out = append(out, fixture)
		}
	}
	return out
}

func forecastsForMatchday(forecasts []Forecast, fixtures []publishedMatch, matchday int) []Forecast {
	out := make([]Forecast, 0)
	for _, forecast := range forecasts {
		if resolveForecast(forecast, fixtures).Fixture.Matchday == matchday {
			out = append(out, forecast)
		}
	}
	return out
}

func forecastsHaveFinalScores(forecasts []Forecast, fixtures []publishedMatch) bool {
	for _, forecast := range forecasts {
		if resolveForecast(forecast, fixtures).Reason != "" {
			return false
		}
	}
	return true
}

func upsertMatchdayResultSet(data *Store, update UEFAResultSet) {
	for i := range data.ResultSets {
		current := &data.ResultSets[i]
		if current.ChatID == update.ChatID && current.Season == update.Season && current.Matchday == update.Matchday {
			*current = update
			return
		}
	}
	data.ResultSets = append(data.ResultSets, update)
}

func mergeFixtures(existing, updates []publishedMatch) []publishedMatch {
	merged := append([]publishedMatch(nil), existing...)
	byKey := make(map[string]int, len(merged)*2)
	for i, fixture := range merged {
		byKey[fixtureKey(fixture)] = i
		byKey[fixtureIdentity(fixture)] = i
	}
	for _, fixture := range updates {
		index, found := byKey[fixtureKey(fixture)]
		if !found {
			index, found = byKey[fixtureIdentity(fixture)]
		}
		if found {
			merged[index] = fixture
			byKey[fixtureKey(fixture)] = index
			byKey[fixtureIdentity(fixture)] = index
			continue
		}
		index = len(merged)
		merged = append(merged, fixture)
		byKey[fixtureKey(fixture)] = index
		byKey[fixtureIdentity(fixture)] = index
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].Date.Before(merged[j].Date) })
	return merged
}

func upsertSeasonSnapshot(data *Store, update SeasonSnapshot) {
	for i := range data.Seasons {
		current := &data.Seasons[i]
		if current.ChatID == update.ChatID && current.Season == update.Season {
			*current = update
			return
		}
	}
	data.Seasons = append(data.Seasons, update)
}

func updateSeasonMatchday(data *Store, chatID int64, season, currentMatchday int, fetchedAt time.Time) {
	if currentMatchday < 1 {
		return
	}
	for i := range data.Seasons {
		if data.Seasons[i].ChatID == chatID && data.Seasons[i].Season == season {
			data.Seasons[i].CurrentMatchday = currentMatchday
			if fetchedAt.After(data.Seasons[i].FetchedAt) {
				data.Seasons[i].FetchedAt = fetchedAt
			}
			return
		}
	}
	data.Seasons = append(data.Seasons, SeasonSnapshot{ChatID: chatID, Season: season, CurrentMatchday: currentMatchday, FetchedAt: fetchedAt})
}

func (b *Bot) fetchAndScoreMatchday(chatID int64, threadID int, forecasts []Forecast, opts resultOptions) {
	type seasonCache struct {
		fixtures        []publishedMatch
		available       bool
		complete        bool
		currentMatchday int
	}
	if opts.refresh && b.footballDataToken == "" {
		_ = b.sendTo(chatID, threadID, "Для принудительного обновления нужен FOOTBALL_DATA_TOKEN.", "")
		return
	}

	b.mu.Lock()
	lists := append([]FixtureList(nil), b.data.Lists...)
	snapshots := append([]SeasonSnapshot(nil), b.data.Seasons...)
	resultSets := append([]UEFAResultSet(nil), b.data.ResultSets...)
	b.mu.Unlock()

	seasons := map[int][]Forecast{}
	for _, forecast := range forecasts {
		season := seasonForForecast(forecast, lists)
		seasons[season] = append(seasons[season], forecast)
	}
	seasonYears := make([]int, 0, len(seasons))
	for season := range seasons {
		seasonYears = append(seasonYears, season)
	}
	sort.Ints(seasonYears)

	var allFixtures []publishedMatch
	var selectedForecasts []Forecast
	var fetchedSets []UEFAResultSet
	var updatedSnapshots []SeasonSnapshot
	roundFound := false
	type seasonMetadata struct {
		season          int
		currentMatchday int
		fetchedAt       time.Time
	}
	var metadata []seasonMetadata

	for _, season := range seasonYears {
		seasonForecasts := seasons[season]
		cache := seasonCache{}
		var fullSnapshot *SeasonSnapshot
		for i := range snapshots {
			if snapshots[i].ChatID == chatID && snapshots[i].Season == season && len(snapshots[i].Matches) > 0 {
				fullSnapshot = &snapshots[i]
				break
			}
		}
		if fullSnapshot != nil && !opts.refresh {
			cache.fixtures = fixturesForMatchday(fullSnapshot.Matches, opts.matchday)
			cache.currentMatchday = fullSnapshot.CurrentMatchday
			cache.available = len(cache.fixtures) > 0
			cache.complete = cache.available
			for _, forecast := range seasonForecasts {
				resolution := resolveForecast(forecast, fullSnapshot.Matches)
				if resolution.Fixture.Matchday == opts.matchday {
					if resolution.Reason != "" {
						cache.complete = false
					}
				} else if resolution.Fixture.Matchday == 0 {
					cache.complete = false
				}
			}
		}
		if !cache.available && !opts.refresh {
			for i := range resultSets {
				set := &resultSets[i]
				if set.ChatID == chatID && set.Season == season && set.Matchday == opts.matchday && len(set.Matches) > 0 {
					cache.fixtures = append([]publishedMatch(nil), set.Matches...)
					cache.available = true
					cache.currentMatchday = 0
					cache.complete = true
					selected := forecastsForMatchday(seasonForecasts, cache.fixtures, opts.matchday)
					if len(selected) > 0 && !forecastsHaveFinalScores(selected, cache.fixtures) {
						cache.complete = false
					}
					break
				}
			}
		}

		if opts.refresh || !cache.complete {
			if b.footballDataToken == "" {
				if !cache.available {
					_ = b.sendTo(chatID, threadID, "Нет сохранённых результатов этого тура. Добавьте FOOTBALL_DATA_TOKEN для запроса к football-data.org.", "")
					return
				}
			} else {
				ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
				var matches []publishedMatch
				var currentMatchday int
				var err error
				if opts.stage != "" {
					matches, currentMatchday, err = b.fetchStageRateLimited(ctx, season, opts.stage)
					if err == nil && len(matches) == 0 {
						matches, currentMatchday, err = b.fetchSeasonRateLimited(ctx, season)
						if err == nil {
							matches = fixturesForMatchday(matches, opts.matchday)
						}
					}
				} else {
					matches, currentMatchday, err = b.fetchMatchdayRateLimited(ctx, season, opts.matchday)
				}
				cancel()
				if err != nil {
					log.Printf("fetch Champions League season %d matchday %d: %s", season, opts.matchday, b.safeError(err))
					if !cache.available || opts.refresh {
						_ = b.sendTo(chatID, threadID, fmt.Sprintf("Не удалось получить %s сезона %d/%d: %s", displayRound(opts.matchday), season, season+1, b.safeError(err)), "")
						return
					}
				} else if len(matches) == 0 {
					if opts.refresh {
						_ = b.sendTo(chatID, threadID, fmt.Sprintf("API не вернул матчи для %s сезона %d/%d; сохранённые данные не изменены.", displayRound(opts.matchday), season, season+1), "")
						return
					}
					if !cache.available {
						cache.complete = true
					}
				} else {
					for i := range matches {
						matches[i].Season = season
						if matches[i].Matchday == 0 {
							matches[i].Matchday = opts.matchday
						}
					}
					cache.fixtures = matches
					cache.available = true
					cache.complete = true
					cache.currentMatchday = currentMatchday
					fetchedAt := time.Now().UTC()
					if fullSnapshot != nil {
						updatedSnapshots = append(updatedSnapshots, SeasonSnapshot{
							ChatID: chatID, Season: season, FetchedAt: fetchedAt,
							CurrentMatchday: currentMatchday,
							Matches:         mergeFixtures(fullSnapshot.Matches, matches),
						})
					} else {
						fetchedSets = append(fetchedSets, UEFAResultSet{ChatID: chatID, Season: season, Matchday: opts.matchday, FetchedAt: fetchedAt, Matches: append([]publishedMatch(nil), matches...)})
					}
					metadata = append(metadata, seasonMetadata{season: season, currentMatchday: currentMatchday, fetchedAt: fetchedAt})
				}
			}
		}
		if cache.available {
			roundFound = true
		}
		if !cache.available {
			continue
		}
		allFixtures = append(allFixtures, cache.fixtures...)
		selected := forecastsForMatchday(seasonForecasts, cache.fixtures, opts.matchday)
		selectedForecasts = append(selectedForecasts, selected...)
	}

	b.mu.Lock()
	for _, snapshot := range updatedSnapshots {
		upsertSeasonSnapshot(&b.data, snapshot)
	}
	for _, set := range fetchedSets {
		upsertMatchdayResultSet(&b.data, set)
	}
	for _, item := range metadata {
		updateSeasonMatchday(&b.data, chatID, item.season, item.currentMatchday, item.fetchedAt)
	}
	if opts.refresh {
		seasonsToRefresh := make(map[int]bool)
		for _, fixture := range allFixtures {
			if fixture.Matchday != opts.matchday {
				continue
			}
			season := fixture.Season
			if season == 0 {
				season = footballSeasonYear(fixture.Date)
			}
			seasonsToRefresh[season] = true
		}
		b.removeRoundScoresLocked(chatID, opts.matchday, seasonsToRefresh)
	}
	if len(selectedForecasts) == 0 {
		before := len(b.data.RoundScores)
		b.scoreNoForecastPlayersLocked(chatID, forecasts, allFixtures, resultOptions{matchday: opts.matchday})
		if len(b.data.RoundScores) > before {
			if err := b.saveLocked(); err != nil {
				b.mu.Unlock()
				log.Printf("save no-forecast round scores: %v", err)
				_ = b.sendTo(chatID, threadID, "Не удалось сохранить результаты тура: "+err.Error(), "")
				return
			}
			scores := append([]RoundScore(nil), b.data.RoundScores...)
			snapshots = append([]SeasonSnapshot(nil), b.data.Seasons...)
			b.mu.Unlock()
			b.sendRoundResultReport(chatID, threadID, scores, snapshots, allFixtures, nil, opts)
			return
		}
		if len(fetchedSets) > 0 || len(metadata) > 0 {
			if err := b.saveLocked(); err != nil {
				b.mu.Unlock()
				log.Printf("save matchday results: %v", err)
				_ = b.sendTo(chatID, threadID, "Не удалось сохранить результаты тура: "+err.Error(), "")
				return
			}
		}
		b.mu.Unlock()
		if !roundFound {
			_ = b.sendTo(chatID, threadID, fmt.Sprintf("football-data.org не вернул матчи для %s в сохранённых сезонах.", displayRound(opts.matchday)), "")
			return
		}
		_ = b.sendTo(chatID, threadID, fmt.Sprintf("Не нашёл прогнозов на %s%s.", displayRound(opts.matchday), resultUsernameSuffix(opts.username, forecasts)), "")
		return
	}
	_, pending := b.scoreRoundsLocked(chatID, selectedForecasts, allFixtures)
	b.scoreNoForecastPlayersLocked(chatID, forecasts, allFixtures, resultOptions{matchday: opts.matchday})
	if err := b.saveLocked(); err != nil {
		b.mu.Unlock()
		log.Printf("save matchday scores: %v", err)
		_ = b.sendTo(chatID, threadID, "Не удалось сохранить результаты тура: "+err.Error(), "")
		return
	}
	scores := append([]RoundScore(nil), b.data.RoundScores...)
	snapshots = append([]SeasonSnapshot(nil), b.data.Seasons...)
	b.mu.Unlock()
	b.sendRoundResultReport(chatID, threadID, scores, snapshots, allFixtures, pending, opts)
}

func (b *Bot) removeRoundScoresLocked(chatID int64, matchday int, seasons map[int]bool) {
	kept := b.data.RoundScores[:0]
	for _, score := range b.data.RoundScores {
		if score.ChatID != chatID || score.Matchday != matchday || !seasons[score.Season] {
			kept = append(kept, score)
			continue
		}
		key := fmt.Sprintf("%d:%d", chatID, score.UserID)
		total := b.data.Totals[key]
		total.Points -= score.Points
		total.Exact -= score.Exact
		if total.Points < 0 {
			total.Points = 0
		}
		if total.Exact < 0 {
			total.Exact = 0
		}
		b.data.Totals[key] = total
	}
	b.data.RoundScores = kept
}

func resultUsernameSuffix(username string, forecasts []Forecast) string {
	if username == "" {
		return ""
	}
	for _, forecast := range forecasts {
		if (resultOptions{username: username}).includesUser(forecast.Name, forecast.Username) {
			return " для пользователя " + participantLabel(forecast.Name, forecast.Username)
		}
	}
	return " для пользователя " + strings.TrimPrefix(strings.TrimSpace(username), "@")
}

func (b *Bot) fetchAndScoreRounds(chatID int64, forecasts []Forecast, opts resultOptions) {
	b.fetchAndScoreRoundsTo(chatID, 0, forecasts, opts)
}

func (b *Bot) fetchAndScoreRoundsTo(chatID int64, threadID int, forecasts []Forecast, opts resultOptions) {
	defer func() {
		b.mu.Lock()
		delete(b.resultRunning, chatID)
		b.mu.Unlock()
	}()
	if opts.matchday > 0 {
		b.fetchAndScoreMatchday(chatID, threadID, forecasts, opts)
		return
	}
	seasonsSet := map[int]bool{}
	b.mu.Lock()
	lists := append([]FixtureList(nil), b.data.Lists...)
	for _, f := range forecasts {
		seasonsSet[seasonForForecast(f, lists)] = true
	}
	existingSnapshots := append([]SeasonSnapshot(nil), b.data.Seasons...)
	existingRoundSets := append([]UEFAResultSet(nil), b.data.ResultSets...)
	b.mu.Unlock()
	seasonYears := make([]int, 0, len(seasonsSet))
	for season := range seasonsSet {
		seasonYears = append(seasonYears, season)
	}
	sort.Ints(seasonYears)
	newSnapshots := make([]SeasonSnapshot, 0, len(seasonYears))
	var cachedRoundFixtures []publishedMatch
	for _, season := range seasonYears {
		var cached *SeasonSnapshot
		for i := range existingSnapshots {
			if existingSnapshots[i].ChatID == chatID && existingSnapshots[i].Season == season && len(existingSnapshots[i].Matches) > 0 {
				cached = &existingSnapshots[i]
				break
			}
		}
		if cached != nil {
			complete := true
			for _, forecast := range forecasts {
				if seasonForForecast(forecast, lists) != season {
					continue
				}
				if resolveForecast(forecast, cached.Matches).Reason != "" {
					complete = false
					break
				}
			}
			if complete {
				newSnapshots = append(newSnapshots, *cached)
				continue
			}
		}
		if cached == nil {
			var savedRoundFixtures []publishedMatch
			for _, resultSet := range existingRoundSets {
				if resultSet.ChatID == chatID && resultSet.Season == season && resultSet.Matchday > 0 {
					savedRoundFixtures = append(savedRoundFixtures, resultSet.Matches...)
				}
			}
			if len(savedRoundFixtures) > 0 {
				complete := true
				for _, forecast := range forecasts {
					if seasonForForecast(forecast, lists) == season && resolveForecast(forecast, savedRoundFixtures).Reason != "" {
						complete = false
						break
					}
				}
				if complete {
					cachedRoundFixtures = append(cachedRoundFixtures, savedRoundFixtures...)
					continue
				}
			}
		}
		if b.footballDataToken == "" {
			_ = b.sendTo(chatID, threadID, "Нет сохранённых финальных результатов для всех прогнозов. Добавьте FOOTBALL_DATA_TOKEN для обновления.", "")
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		matches, currentMatchday, err := b.fetchSeasonRateLimited(ctx, season)
		cancel()
		if err != nil {
			log.Printf("fetch Champions League season %d: %s", season, b.safeError(err))
			for _, old := range existingSnapshots {
				if old.ChatID == chatID && old.Season == season && len(old.Matches) > 0 {
					newSnapshots = append(newSnapshots, old)
					matches = old.Matches
					break
				}
			}
			if len(matches) == 0 {
				_ = b.sendTo(chatID, threadID, fmt.Sprintf("Не удалось получить результаты сезона %d/%d: %s", season, season+1, err.Error()), "")
				return
			}
			continue
		}
		if len(matches) == 0 {
			_ = b.sendTo(chatID, threadID, fmt.Sprintf("API не вернул матчи Лиги чемпионов за сезон %d/%d.", season, season+1), "")
			return
		}
		newSnapshots = append(newSnapshots, SeasonSnapshot{ChatID: chatID, Season: season, FetchedAt: time.Now().UTC(), CurrentMatchday: currentMatchday, Matches: matches})
	}
	var fixtures []publishedMatch
	for _, snapshot := range newSnapshots {
		fixtures = append(fixtures, snapshot.Matches...)
	}
	fixtures = append(fixtures, cachedRoundFixtures...)
	if len(fixtures) == 0 {
		_ = b.sendTo(chatID, threadID, "Не удалось загрузить матчи для расчёта.", "")
		return
	}
	b.mu.Lock()
	for _, snapshot := range newSnapshots {
		updated := false
		for i := range b.data.Seasons {
			if b.data.Seasons[i].ChatID == chatID && b.data.Seasons[i].Season == snapshot.Season {
				b.data.Seasons[i] = snapshot
				updated = true
				break
			}
		}
		if !updated {
			b.data.Seasons = append(b.data.Seasons, snapshot)
		}
	}
	selectedForecasts := forecasts
	if opts.username != "" {
		selectedForecasts = selectedForecasts[:0]
		for _, forecast := range forecasts {
			if opts.includesUser(forecast.Name, forecast.Username) {
				selectedForecasts = append(selectedForecasts, forecast)
			}
		}
	}
	_, pending := b.scoreRoundsLocked(chatID, selectedForecasts, fixtures)
	matchdays := map[int]bool{}
	for _, fixture := range fixtures {
		if fixture.Matchday > 0 {
			matchdays[fixture.Matchday] = true
		}
	}
	for matchday := range matchdays {
		b.scoreNoForecastPlayersLocked(chatID, forecasts, fixtures, resultOptions{matchday: matchday, username: opts.username})
	}
	if err := b.saveLocked(); err != nil {
		b.mu.Unlock()
		log.Printf("save round results: %v", err)
		_ = b.sendTo(chatID, threadID, "Не удалось сохранить результаты: "+err.Error(), "")
		return
	}
	allScores := append([]RoundScore(nil), b.data.RoundScores...)
	allSnapshots := append([]SeasonSnapshot(nil), b.data.Seasons...)
	b.mu.Unlock()
	b.sendRoundResultReport(chatID, threadID, allScores, allSnapshots, fixtures, pending, opts)
}

func (b *Bot) scoreRoundsLocked(chatID int64, forecasts []Forecast, fixtures []publishedMatch) ([]RoundScore, []ForecastDetail) {
	selected := selectForecasts(forecasts, fixtures)
	groups := map[string]*RoundScore{}
	pending := map[string]bool{}
	for _, f := range selected {
		resolution := resolveForecast(f, fixtures)
		season := resolution.Fixture.Season
		if season == 0 && !resolution.Fixture.Date.IsZero() {
			season = footballSeasonYear(resolution.Fixture.Date)
		}
		detail := ForecastDetail{
			Season: season, Matchday: resolution.Fixture.Matchday, ForecastDate: f.Date,
			UserID: f.UserID, Name: f.Name, Username: f.Username, Prediction: f.Match,
		}
		if resolution.Reason == "" {
			actual := resolution.Actual
			detail.Actual = &actual
			detail.Points = points(f.Match, actual)
			detail.Reason = scoreReason(f.Match, actual)
		} else {
			if resolution.Fixture.HasScore {
				actual := resolution.Actual
				detail.Actual = &actual
			}
			detail.Reason = resolution.Reason
		}
		key := roundScoreKey(chatID, f.UserID, season, detail.Matchday)
		if detail.Matchday == 0 {
			key = fmt.Sprintf("unknown:%d:%d:%s", f.UserID, season, resolution.Key)
		}
		group := groups[key]
		if group == nil {
			group = &RoundScore{ChatID: chatID, UserID: f.UserID, Name: f.Name, Username: f.Username, Season: season, Matchday: detail.Matchday}
			groups[key] = group
		}
		group.Details = append(group.Details, detail)
		if detail.Actual == nil || detail.Matchday == 0 {
			pending[key] = true
		} else {
			group.Points += detail.Points
			if detail.Reason == "точный счёт" {
				group.Exact++
			}
		}
	}
	for _, group := range groups {
		group.Missing = missingForecastCount(group.UserID, group.Season, group.Matchday, group.Details, fixtures)
	}

	existing := map[string]RoundScore{}
	for _, score := range b.data.RoundScores {
		existing[roundScoreKey(score.ChatID, score.UserID, score.Season, score.Matchday)] = score
	}
	result := make([]RoundScore, 0, len(groups))
	for key, group := range groups {
		if score, ok := existing[key]; ok {
			result = append(result, score)
			continue
		}
		if pending[key] {
			continue
		}
		group.ScoredAt = time.Now().UTC()
		b.data.RoundScores = append(b.data.RoundScores, *group)
		rowKey := fmt.Sprintf("%d:%d", chatID, group.UserID)
		total := b.data.Totals[rowKey]
		total.Name, total.Username = group.Name, group.Username
		total.Points += group.Points
		total.Exact += group.Exact
		b.data.Totals[rowKey] = total
		result = append(result, *group)
	}
	return result, collectPendingGroups(groups, pending)
}

func missingForecastCount(userID int64, season, matchday int, details []ForecastDetail, fixtures []publishedMatch) int {
	covered := map[string]bool{}
	total := 0
	for _, fixture := range fixtures {
		fixtureSeason := fixture.Season
		if fixtureSeason == 0 {
			fixtureSeason = footballSeasonYear(fixture.Date)
		}
		if fixtureSeason != season || fixture.Matchday != matchday {
			continue
		}
		total++
	}
	for _, detail := range details {
		resolution := resolveForecast(Forecast{Date: detail.ForecastDate, Match: detail.Prediction}, fixtures)
		if resolution.Key != "" && resolution.Fixture.Matchday == matchday {
			fixtureSeason := resolution.Fixture.Season
			if fixtureSeason == 0 {
				fixtureSeason = footballSeasonYear(resolution.Fixture.Date)
			}
			if fixtureSeason == season {
				covered[resolution.Key] = true
			}
		}
	}
	if total-len(covered) < 0 {
		return 0
	}
	return total - len(covered)
}

// A player who has participated in the season but skipped a whole round gets an
// explicit zero once every fixture in that round has a final score.
func (b *Bot) scoreNoForecastPlayersLocked(chatID int64, roster []Forecast, fixtures []publishedMatch, opts resultOptions) {
	players := map[string]Forecast{}
	for _, forecast := range roster {
		if !opts.includesUser(forecast.Name, forecast.Username) {
			continue
		}
		season := footballSeasonYear(time.Unix(forecast.Date, 0).UTC())
		players[fmt.Sprintf("%d:%d", season, forecast.UserID)] = forecast
	}
	tours := map[string][]publishedMatch{}
	for _, fixture := range fixtures {
		if fixture.Matchday != opts.matchday {
			continue
		}
		season := fixture.Season
		if season == 0 {
			season = footballSeasonYear(fixture.Date)
		}
		key := fmt.Sprintf("%d:%d", season, fixture.Matchday)
		tours[key] = append(tours[key], fixture)
	}
	for _, forecast := range players {
		season := footballSeasonYear(time.Unix(forecast.Date, 0).UTC())
		key := fmt.Sprintf("%d:%d", season, opts.matchday)
		tour := tours[key]
		if len(tour) == 0 {
			continue
		}
		complete := true
		for _, fixture := range tour {
			if !fixture.HasScore {
				complete = false
				break
			}
		}
		if !complete {
			continue
		}
		hasPrediction := false
		for _, candidate := range roster {
			if candidate.UserID != forecast.UserID || footballSeasonYear(time.Unix(candidate.Date, 0).UTC()) != season {
				continue
			}
			resolution := resolveForecast(candidate, tour)
			if resolution.Reason == "" || resolution.Fixture.Matchday == opts.matchday {
				hasPrediction = true
				break
			}
		}
		if hasPrediction {
			continue
		}
		scoreKey := roundScoreKey(chatID, forecast.UserID, season, opts.matchday)
		alreadyScored := false
		for _, score := range b.data.RoundScores {
			if roundScoreKey(score.ChatID, score.UserID, score.Season, score.Matchday) == scoreKey {
				alreadyScored = true
				break
			}
		}
		if alreadyScored {
			continue
		}
		b.data.RoundScores = append(b.data.RoundScores, RoundScore{ChatID: chatID, UserID: forecast.UserID, Name: forecast.Name, Username: forecast.Username, Season: season, Matchday: opts.matchday, Missing: len(tour), ScoredAt: time.Now().UTC()})
		rowKey := fmt.Sprintf("%d:%d", chatID, forecast.UserID)
		total := b.data.Totals[rowKey]
		if total.Name == "" {
			total.Name, total.Username = forecast.Name, forecast.Username
			b.data.Totals[rowKey] = total
		}
	}
}

func collectPendingGroups(groups map[string]*RoundScore, pending map[string]bool) []ForecastDetail {
	var out []ForecastDetail
	for key := range pending {
		out = append(out, groups[key].Details...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Season != out[j].Season {
			return out[i].Season < out[j].Season
		}
		if out[i].Matchday != out[j].Matchday {
			return out[i].Matchday < out[j].Matchday
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		if out[i].Prediction.Home != out[j].Prediction.Home {
			return out[i].Prediction.Home < out[j].Prediction.Home
		}
		return out[i].Prediction.Away < out[j].Prediction.Away
	})
	return out
}

func scoreReason(prediction, actual Match) string {
	if prediction.HomeGoals == actual.HomeGoals && prediction.AwayGoals == actual.AwayGoals {
		return "точный счёт"
	}
	predictedDiff := prediction.HomeGoals - prediction.AwayGoals
	actualDiff := actual.HomeGoals - actual.AwayGoals
	if predictedDiff == actualDiff {
		return "угаданы исход и разница голов"
	}
	if predictedDiff > 0 && actualDiff > 0 || predictedDiff < 0 && actualDiff < 0 {
		return "угадан победитель"
	}
	return "исход не угадан"
}

func shortReason(reason string) string {
	switch reason {
	case "точный счёт":
		return "точно"
	case "угаданы исход и разница голов":
		return "исход+разница"
	case "угадан победитель":
		return "победитель"
	case "исход не угадан":
		return "мимо"
	case "уже засчитан предыдущим запуском", "матч уже засчитан предыдущим запуском":
		return "уже учтён"
	case "не удалось сопоставить команды с сохранёнными матчами", "матч с указанным ID отсутствует в сохранённых результатах":
		return "матч не найден"
	case "найдено несколько подходящих матчей; уточните прогноз":
		return "несколько матчей"
	}
	if strings.HasPrefix(reason, "финальный счёт отсутствует (статус: ") {
		return "нет финала: " + strings.TrimSuffix(strings.TrimPrefix(reason, "финальный счёт отсутствует (статус: "), ")")
	}
	return reason
}

func (b *Bot) sendRoundResultReport(chatID int64, threadID int, scores []RoundScore, snapshots []SeasonSnapshot, fixtures []publishedMatch, pending []ForecastDetail, opts resultOptions) {
	type roundView struct {
		season   int
		matchday int
		scores   []RoundScore
		pending  []ForecastDetail
	}
	views := map[string]*roundView{}
	viewFor := func(season, matchday int) *roundView {
		key := fmt.Sprintf("%d:%d", season, matchday)
		if views[key] == nil {
			views[key] = &roundView{season: season, matchday: matchday}
		}
		return views[key]
	}
	for _, score := range scores {
		if score.ChatID != chatID || !opts.includesUser(score.Name, score.Username) {
			continue
		}
		if opts.matchday > 0 && score.Matchday != opts.matchday {
			continue
		}
		filtered := score
		filtered.Details = nil
		for _, detail := range score.Details {
			if opts.includesUser(detail.Name, detail.Username) {
				filtered.Details = append(filtered.Details, detail)
			}
		}
		if len(filtered.Details) == 0 && opts.username != "" && filtered.Missing == 0 {
			continue
		}
		viewFor(score.Season, score.Matchday).scores = append(viewFor(score.Season, score.Matchday).scores, filtered)
	}
	for _, detail := range pending {
		if opts.matchday > 0 && detail.Matchday != opts.matchday || !opts.includesUser(detail.Name, detail.Username) {
			continue
		}
		viewFor(detail.Season, detail.Matchday).pending = append(viewFor(detail.Season, detail.Matchday).pending, detail)
	}
	keys := make([]string, 0, len(views))
	for key := range views {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, c := views[keys[i]], views[keys[j]]
		if a.season != c.season {
			return a.season < c.season
		}
		return a.matchday < c.matchday
	})
	if len(keys) == 0 {
		_ = b.sendTo(chatID, threadID, "Нет завершённых туров для отображения.", "")
		return
	}
	var out strings.Builder
	out.WriteString("Результаты Лиги чемпионов по турам\n")
	for _, key := range keys {
		view := views[key]
		if view.matchday > 0 {
			fmt.Fprintf(&out, "\nСезон %d/%d · %s\n", view.season, view.season+1, displayRound(view.matchday))
		} else {
			fmt.Fprintf(&out, "\nСезон %d/%d · тур не определён\n", view.season, view.season+1)
		}
		shown := map[string]bool{}
		for _, fixture := range fixtures {
			if opts.dataOnly || opts.username != "" {
				continue
			}
			season := fixture.Season
			if season == 0 {
				season = footballSeasonYear(fixture.Date)
			}
			if season != view.season || fixture.Matchday != view.matchday || view.matchday == 0 {
				continue
			}
			key := fixtureKey(fixture)
			if shown[key] {
				continue
			}
			shown[key] = true
			if fixture.HasScore {
				fmt.Fprintf(&out, "%s—%s %d:%d\n", fixture.Home, fixture.Away, fixture.HomeGoals, fixture.AwayGoals)
			} else {
				fmt.Fprintf(&out, "%s—%s: нет финала (%s)\n", fixture.Home, fixture.Away, fixture.Status)
			}
		}
		if len(view.scores) > 0 && opts.username == "" {
			out.WriteString("Очки:\n")
		}
		sort.Slice(view.scores, func(i, j int) bool {
			if view.scores[i].Points == view.scores[j].Points {
				return view.scores[i].Name < view.scores[j].Name
			}
			return view.scores[i].Points > view.scores[j].Points
		})
		for i, score := range view.scores {
			if i > 0 {
				out.WriteByte('\n')
			}
			fmt.Fprintf(&out, "%s %+d (точно: %d", participantLabel(score.Name, score.Username), score.Points, score.Exact)
			if score.Missing > 0 {
				fmt.Fprintf(&out, ", пропусков: %d", score.Missing)
			}
			out.WriteByte(')')
		}
		if len(view.scores) > 0 {
			out.WriteByte('\n')
		}
		if len(view.pending) > 0 {
			pendingByUser := map[int64]string{}
			for _, detail := range view.pending {
				pendingByUser[detail.UserID] = participantLabel(detail.Name, detail.Username)
			}
			out.WriteString("Ожидают финального счёта: ")
			pendingNames := make([]string, 0, len(pendingByUser))
			for _, name := range pendingByUser {
				pendingNames = append(pendingNames, name)
			}
			sort.Strings(pendingNames)
			first := true
			for _, name := range pendingNames {
				if !first {
					out.WriteString(", ")
				}
				first = false
				out.WriteString(name)
			}
			if first {
				out.WriteString("нет")
			}
			out.WriteByte('\n')
		}
		if opts.username != "" {
			for _, score := range view.scores {
				fmt.Fprintf(&out, "Игрок: %s · %+d очков (точно: %d)\n", participantLabel(score.Name, score.Username), score.Points, score.Exact)
				covered := map[string]bool{}
				for _, detail := range score.Details {
					if resolution := resolveForecast(Forecast{Date: detail.ForecastDate, Match: detail.Prediction}, fixtures); resolution.Key != "" {
						covered[resolution.Key] = true
					}
					actual := "?"
					if detail.Actual != nil {
						actual = fmt.Sprintf("%d:%d", detail.Actual.HomeGoals, detail.Actual.AwayGoals)
					}
					fmt.Fprintf(&out, "%s %d:%d→%s %+d (%s)\n", detail.Prediction.Home+"—"+detail.Prediction.Away, detail.Prediction.HomeGoals, detail.Prediction.AwayGoals, actual, detail.Points, shortReason(detail.Reason))
				}
				for _, fixture := range fixtures {
					season := fixture.Season
					if season == 0 {
						season = footballSeasonYear(fixture.Date)
					}
					if season != view.season || fixture.Matchday != view.matchday || covered[fixtureKey(fixture)] {
						continue
					}
					fmt.Fprintf(&out, "%s—%s ?→%d:%d +0 (прогноз не подан)\n", fixture.Home, fixture.Away, fixture.HomeGoals, fixture.AwayGoals)
				}
				if len(score.Details) == 0 && score.Missing > 0 {
					out.WriteString("На тур прогнозы не поданы; 0 очков.\n")
				}
			}
			pendingHeaders := map[int64]bool{}
			for _, detail := range view.pending {
				if len(view.scores) == 0 && !pendingHeaders[detail.UserID] {
					fmt.Fprintf(&out, "Игрок: %s\n", participantLabel(detail.Name, detail.Username))
					pendingHeaders[detail.UserID] = true
				}
				actual, points, reason := "?", 0, shortReason(detail.Reason)
				if detail.Actual != nil {
					actual = fmt.Sprintf("%d:%d", detail.Actual.HomeGoals, detail.Actual.AwayGoals)
					points = detail.Points
					reason += ", тур не засчитан"
				}
				fmt.Fprintf(&out, "%s %d:%d→%s %+d (%s)\n", detail.Prediction.Home+"—"+detail.Prediction.Away, detail.Prediction.HomeGoals, detail.Prediction.AwayGoals, actual, points, reason)
			}
		}
	}
	currentSeason := 0
	var currentMatchday int
	var latest time.Time
	for _, snapshot := range snapshots {
		if snapshot.ChatID == chatID && snapshot.FetchedAt.After(latest) {
			latest = snapshot.FetchedAt
			currentSeason, currentMatchday = snapshot.Season, snapshot.CurrentMatchday
		}
	}
	if opts.username == "" && currentSeason > 0 && currentMatchday > 0 {
		fmt.Fprintf(&out, "\nСейчас идёт тур %d сезона %d/%d.", currentMatchday, currentSeason, currentSeason+1)
	}
	b.sendLongTo(chatID, threadID, strings.TrimSpace(out.String()))
}

func (b *Bot) sendLong(chatID int64, text string) {
	b.sendLongTo(chatID, 0, text)
}

func (b *Bot) sendLongTo(chatID int64, threadID int, text string) {
	const maxChars = 3500
	var chunk strings.Builder
	for _, line := range strings.Split(text, "\n") {
		if chunk.Len() > 0 && chunk.Len()+len(line)+1 > maxChars {
			if err := b.sendTo(chatID, threadID, chunk.String(), ""); err != nil {
				log.Printf("send report: %s", b.safeError(err))
			}
			chunk.Reset()
		}
		if chunk.Len() > 0 {
			chunk.WriteByte('\n')
		}
		chunk.WriteString(line)
	}
	if chunk.Len() > 0 {
		if err := b.sendTo(chatID, threadID, chunk.String(), ""); err != nil {
			log.Printf("send report: %s", b.safeError(err))
		}
	}
}

func (b *Bot) handleStats(m Message) {
	type rankedTotal struct {
		userID int64
		total  Totals
	}
	b.mu.Lock()
	rows := make([]rankedTotal, 0, len(b.data.Totals))
	prefix := fmt.Sprintf("%d:", m.Chat.ID)
	for key, t := range b.data.Totals {
		if strings.HasPrefix(key, prefix) {
			parts := strings.Split(key, ":")
			if len(parts) == 2 {
				if userID, err := strconv.ParseInt(parts[1], 10, 64); err == nil {
					rows = append(rows, rankedTotal{userID: userID, total: t})
				}
			}
		}
	}
	rounds := make([]RoundScore, 0)
	var currentSnapshot SeasonSnapshot
	hasCurrentSnapshot := false
	for i := range b.data.Seasons {
		snapshot := &b.data.Seasons[i]
		if snapshot.ChatID != m.Chat.ID {
			continue
		}
		if !hasCurrentSnapshot || snapshot.FetchedAt.After(currentSnapshot.FetchedAt) {
			currentSnapshot = *snapshot
			hasCurrentSnapshot = true
		}
	}
	for _, score := range b.data.RoundScores {
		if score.ChatID == m.Chat.ID {
			rounds = append(rounds, score)
		}
	}
	b.mu.Unlock()
	if len(rows) == 0 {
		_ = b.sendTo(m.Chat.ID, m.MessageThreadID, "Статистика пока пустая. Сначала соберите прогнозы и выполните /result.", "")
		return
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].total.Points == rows[j].total.Points {
			if rows[i].total.Name == rows[j].total.Name {
				return rows[i].userID < rows[j].userID
			}
			return rows[i].total.Name < rows[j].total.Name
		}
		return rows[i].total.Points > rows[j].total.Points
	})
	statsSeason := currentSnapshot.Season
	if statsSeason == 0 {
		for _, round := range rounds {
			if round.Season > statsSeason {
				statsSeason = round.Season
			}
		}
	}
	byUser := map[int64]Totals{}
	for _, row := range rows {
		byUser[row.userID] = row.total
	}
	byRound := map[string]RoundScore{}
	for _, round := range rounds {
		if round.Season != statsSeason || round.Matchday < 1 || round.Matchday > 13 {
			continue
		}
		byUser[round.UserID] = Totals{Name: round.Name, Username: round.Username, Points: byUser[round.UserID].Points, Exact: byUser[round.UserID].Exact}
		byRound[fmt.Sprintf("%d:%d", round.UserID, round.Matchday)] = round
	}
	var table strings.Builder
	if len(byUser) > 0 {
		const totalWidth = 11
		playerWidth := 14
		for i, row := range rows {
			player := fmt.Sprintf("%d. %s", i+1, participantLabel(row.total.Name, row.total.Username))
			if width := len([]rune(player)); width > playerWidth {
				playerWidth = width
			}
		}
		fmt.Fprintf(&table, "%-*s  %*s", playerWidth, "Игрок", totalWidth, "Очки/точно")
		for round := 1; round <= 13; round++ {
			header := fmt.Sprintf("%d", round)
			if round > 8 {
				header = map[int]string{9: "1/16", 10: "1/8", 11: "1/4", 12: "1/2", 13: "ФИН"}[round]
			}
			fmt.Fprintf(&table, " %4s", header)
		}
		table.WriteByte('\n')
		separatorLength := playerWidth + 2 + totalWidth + 13*5
		table.WriteString(strings.Repeat("─", separatorLength))
		table.WriteByte('\n')
		for i, row := range rows {
			total, userID := row.total, row.userID
			player := fmt.Sprintf("%d. %s", i+1, participantLabel(total.Name, total.Username))
			fmt.Fprintf(&table, "%-*s  %*d/%-*d", playerWidth, player, totalWidth-4, total.Points, 3, total.Exact)
			for round := 1; round <= 13; round++ {
				if score, ok := byRound[fmt.Sprintf("%d:%d", userID, round)]; ok {
					fmt.Fprintf(&table, " %+4d", score.Points)
				} else {
					table.WriteString("    ·")
				}
			}
			table.WriteByte('\n')
		}
	}
	var out strings.Builder
	fmt.Fprintf(&out, "Таблица Лиги чемпионов · сезон %d/%d\n<pre>%s</pre>\nИгроки отсортированы по общему числу очков. В колонке «Очки/точно» второй показатель — число точных счетов. · — тур ещё не оценён.", statsSeason, statsSeason+1, html.EscapeString(strings.TrimRight(table.String(), "\n")))
	_ = b.sendTo(m.Chat.ID, m.MessageThreadID, strings.TrimSpace(out.String()), "HTML")
}

func (b *Bot) handleHelp(m Message) {
	_ = b.sendTo(m.Chat.ID, m.MessageThreadID, "Команды бота:\n"+
		"/help — эта справка\n"+
		"/tour [номер_тура|1/16|1/8|1/4|1/2|1/1] — список матчей и шаблон прогнозов\n"+
		"/tour data — последний сохранённый список матчей\n"+
		"/result [номер_тура|1/16|1/8|1/4|1/2|1/1] [username] [--refresh] — посчитать тур/стадию для всех\n"+
		"/result data username — детальный расчёт по сохранённым данным без записи\n"+
		"/stats — общие очки, точные счета и баллы по турам\n"+
		"/debug — время работы бота и последние логи\n\n"+
		"Прогноз отправляйте строками, например: 1. Ланс — Спортинг: 1:1\n"+
		"Чтобы учесть старый прогноз, ответьте на его сообщение любым текстом.", "")
}

func (b *Bot) handleDebug(m Message) {
	startedAt := b.startedAt
	if startedAt.IsZero() {
		startedAt = time.Now()
	}
	uptime := time.Since(startedAt)
	var out strings.Builder
	fmt.Fprintf(&out, "Бот работает: %s\nПоследние логи:\n", formatUptime(uptime))
	if b.recentLog == nil {
		out.WriteString("Логи пока недоступны.")
	} else {
		lines := b.recentLog.Last(20)
		if len(lines) == 0 {
			out.WriteString("Записей пока нет.")
		} else {
			visible := 0
			for _, line := range lines {
				// Message contents stay in Docker logs and are not broadcast to
				// every chat through the shared /debug command.
				if strings.HasPrefix(line, "telegram input") || strings.HasPrefix(line, "telegram output") {
					continue
				}
				if b.token != "" {
					line = strings.ReplaceAll(line, b.token, "[скрыто]")
				}
				if b.footballDataToken != "" {
					line = strings.ReplaceAll(line, b.footballDataToken, "[скрыто]")
				}
				out.WriteString(line)
				out.WriteByte('\n')
				visible++
			}
			if visible == 0 {
				out.WriteString("Нет общих логов для показа; подробный журнал сообщений доступен через docker compose logs.")
			}
		}
	}
	b.sendLongTo(m.Chat.ID, m.MessageThreadID, strings.TrimSpace(out.String()))
}

func formatUptime(uptime time.Duration) string {
	if uptime < 0 {
		uptime = 0
	}
	days := int(uptime / (24 * time.Hour))
	uptime %= 24 * time.Hour
	hours := int(uptime / time.Hour)
	uptime %= time.Hour
	minutes := int(uptime / time.Minute)
	seconds := int(uptime / time.Second)
	if days > 0 {
		return fmt.Sprintf("%d дн. %d ч. %d мин.", days, hours, minutes)
	}
	if hours > 0 {
		return fmt.Sprintf("%d ч. %d мин. %d сек.", hours, minutes, seconds)
	}
	if minutes > 0 {
		return fmt.Sprintf("%d мин. %d сек.", minutes, seconds)
	}
	return fmt.Sprintf("%d сек.", seconds)
}

func (b *Bot) run(ctx context.Context) error {
	b.mu.Lock()
	offset := b.data.UpdateOffset + 1
	b.mu.Unlock()
	retryDelay := 3 * time.Second
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var updates []Update
		callCtx, cancel := context.WithTimeout(ctx, 70*time.Second)
		err := b.call(callCtx, "getUpdates", map[string]any{"offset": offset, "timeout": 60, "allowed_updates": []string{"message"}}, &updates)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("getUpdates: %s; retrying in %s", b.safeError(err), retryDelay)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(retryDelay):
			}
			if retryDelay < 15*time.Second {
				retryDelay *= 2
			} else {
				retryDelay = 30 * time.Second
			}
			continue
		}
		retryDelay = 3 * time.Second
		for _, update := range updates {
			if !b.handle(update.Message) {
				log.Printf("could not persist Telegram update %d; it will be retried", update.ID)
				break
			}
			b.mu.Lock()
			if update.ID > b.data.UpdateOffset {
				previousOffset := b.data.UpdateOffset
				b.data.UpdateOffset = update.ID
				cmd, args := command(update.Message.Text)
				preview, _ := parseResultOptions(args)
				if err := func() error {
					if cmd == "result" && preview.dataOnly {
						return nil
					}
					return b.saveLocked()
				}(); err != nil {
					b.data.UpdateOffset = previousOffset
					log.Printf("could not persist Telegram update offset %d: %v", update.ID, err)
					b.mu.Unlock()
					break
				}
			}
			b.mu.Unlock()
			offset = update.ID + 1
		}
	}
}

func main() {
	logs := &recentLog{}
	log.SetOutput(io.MultiWriter(os.Stderr, logs))
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" {
		log.Fatal("TELEGRAM_BOT_TOKEN is required")
	}
	path := os.Getenv("DATA_FILE")
	if path == "" {
		path = "/data/bot.db"
	}
	storage, data, err := OpenSQLiteStore(path)
	if err != nil {
		log.Fatalf("load data: %v", err)
	}
	defer func() {
		if err := storage.Close(); err != nil {
			log.Printf("close database: %v", err)
		}
	}()
	bot := &Bot{
		token:             token,
		footballDataToken: os.Getenv("FOOTBALL_DATA_TOKEN"),
		storePath:         path,
		storage:           storage,
		client:            &http.Client{Timeout: 75 * time.Second},
		data:              data,
		startedAt:         time.Now(),
		recentLog:         logs,
		apiInterval:       18 * time.Second,
	}
	log.Printf("t-bet-bot started; data file: %s", path)
	var identity struct {
		ID       int64  `json:"id"`
		IsBot    bool   `json:"is_bot"`
		Username string `json:"username"`
	}
	checkCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	err = bot.call(checkCtx, "getMe", map[string]any{}, &identity)
	cancel()
	retryDelay := 3 * time.Second
	for err != nil {
		log.Printf("Telegram Bot API getMe check failed: %s; retrying in %s", bot.safeError(err), retryDelay)
		time.Sleep(retryDelay)
		if retryDelay < 15*time.Second {
			retryDelay *= 2
		} else {
			retryDelay = 30 * time.Second
		}
		checkCtx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
		err = bot.call(checkCtx, "getMe", map[string]any{}, &identity)
		cancel()
	}
	log.Printf("Telegram Bot API connected as @%s (id %d)", identity.Username, identity.ID)
	if err := bot.run(context.Background()); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}
