package main

/*
#cgo LDFLAGS: -lsqlite3
#include <stdlib.h>
typedef struct sqlite3 sqlite3;
typedef struct sqlite3_stmt sqlite3_stmt;
extern int sqlite3_open_v2(const char*, sqlite3**, int, const char*);
extern int sqlite3_close_v2(sqlite3*);
extern void sqlite3_free(void*);
extern const char* sqlite3_errmsg(sqlite3*);
extern int sqlite3_exec(sqlite3*, const char*, void*, void*, char**);
extern int sqlite3_prepare_v2(sqlite3*, const char*, int, sqlite3_stmt**, const char**);
extern int sqlite3_step(sqlite3_stmt*);
extern int sqlite3_finalize(sqlite3_stmt*);
extern int sqlite3_bind_text(sqlite3_stmt*, int, const char*, int, void(*)(void*));
extern int sqlite3_bind_int64(sqlite3_stmt*, int, long long);
extern const unsigned char* sqlite3_column_text(sqlite3_stmt*, int);
extern int sqlite3_column_bytes(sqlite3_stmt*, int);
extern long long sqlite3_column_int64(sqlite3_stmt*, int);
static int bind_text_copy(sqlite3_stmt* s, int i, const char* v, int n) {
  return sqlite3_bind_text(s, i, v, n, (void(*)(void*))-1);
}
*/
import "C"

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"unsafe"
)

const (
	sqliteOK            = 0
	sqliteRow           = 100
	sqliteDone          = 101
	sqliteOpenReadWrite = 0x00000002
	sqliteOpenCreate    = 0x00000004
)

// SQLiteStore uses a single normalized record catalog. Fixtures are stored once;
// lists and snapshots only retain references to fixture keys.
type SQLiteStore struct {
	mu   sync.Mutex
	db   *C.sqlite3
	path string
}

func OpenSQLiteStore(path string) (*SQLiteStore, Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, Store{}, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, Store{}, err
	}
	if err := file.Close(); err != nil {
		return nil, Store{}, err
	}
	db := &SQLiteStore{path: path}
	cp := C.CString(path)
	defer C.free(unsafe.Pointer(cp))
	if rc := C.sqlite3_open_v2(cp, &db.db, sqliteOpenReadWrite|sqliteOpenCreate, nil); rc != sqliteOK {
		err := db.err()
		if db.db != nil {
			C.sqlite3_close_v2(db.db)
		}
		return nil, Store{}, err
	}
	if err := db.exec("PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000; CREATE TABLE IF NOT EXISTS records (kind TEXT NOT NULL, key TEXT NOT NULL, value TEXT NOT NULL, PRIMARY KEY(kind,key));"); err != nil {
		C.sqlite3_close_v2(db.db)
		return nil, Store{}, err
	}
	if _, err := db.count(); err != nil {
		C.sqlite3_close_v2(db.db)
		return nil, Store{}, err
	}
	data, err := db.Load()
	if err != nil {
		C.sqlite3_close_v2(db.db)
		return nil, Store{}, err
	}
	return db, data, nil
}

func (s *SQLiteStore) err() error {
	return fmt.Errorf("sqlite: %s", C.GoString(C.sqlite3_errmsg(s.db)))
}

func (s *SQLiteStore) exec(sqlText string) error {
	cs := C.CString(sqlText)
	defer C.free(unsafe.Pointer(cs))
	var message *C.char
	if rc := C.sqlite3_exec(s.db, cs, nil, nil, &message); rc != sqliteOK {
		if message != nil {
			defer C.sqlite3_free(unsafe.Pointer(message))
			return fmt.Errorf("sqlite: %s", C.GoString(message))
		}
		return s.err()
	}
	return nil
}

func (s *SQLiteStore) count() (int64, error) {
	stmt, err := s.prepare("SELECT count(*) FROM records")
	if err != nil {
		return 0, err
	}
	defer C.sqlite3_finalize(stmt)
	if rc := C.sqlite3_step(stmt); rc != sqliteRow {
		return 0, s.err()
	}
	return int64(C.sqlite3_column_int64(stmt, 0)), nil
}

func (s *SQLiteStore) prepare(query string) (*C.sqlite3_stmt, error) {
	cs := C.CString(query)
	defer C.free(unsafe.Pointer(cs))
	var stmt *C.sqlite3_stmt
	if rc := C.sqlite3_prepare_v2(s.db, cs, -1, &stmt, nil); rc != sqliteOK {
		return nil, s.err()
	}
	return stmt, nil
}

func (s *SQLiteStore) put(kind, key string, value any, ignore bool) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	query := "INSERT INTO records(kind,key,value) VALUES(?,?,?) ON CONFLICT(kind,key) DO UPDATE SET value=excluded.value WHERE records.value<>excluded.value"
	if ignore {
		query = "INSERT OR IGNORE INTO records(kind,key,value) VALUES(?,?,?)"
	}
	stmt, err := s.prepare(query)
	if err != nil {
		return err
	}
	defer C.sqlite3_finalize(stmt)
	for i, v := range []string{kind, key, string(data)} {
		cv := C.CString(v)
		rc := C.bind_text_copy(stmt, C.int(i+1), cv, C.int(len(v)))
		C.free(unsafe.Pointer(cv))
		if rc != sqliteOK {
			return s.err()
		}
	}
	if rc := C.sqlite3_step(stmt); rc != sqliteDone {
		return s.err()
	}
	return nil
}

func (s *SQLiteStore) Load() (Store, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := diskStore{Fixtures: map[string]publishedMatch{}, Totals: map[string]Totals{}}
	stmt, err := s.prepare("SELECT kind,key,value FROM records ORDER BY kind,key")
	if err != nil {
		return Store{}, err
	}
	defer C.sqlite3_finalize(stmt)
	for {
		rc := C.sqlite3_step(stmt)
		if rc == sqliteDone {
			break
		}
		if rc != sqliteRow {
			return Store{}, s.err()
		}
		kind := columnString(stmt, 0)
		key := columnString(stmt, 1)
		value := columnString(stmt, 2)
		decode := func(out any) error { return json.Unmarshal([]byte(value), out) }
		switch kind {
		case "fixture":
			var v publishedMatch
			err = decode(&v)
			d.Fixtures[key] = v
		case "forecast":
			var v Forecast
			err = decode(&v)
			d.Forecasts = append(d.Forecasts, v)
		case "total":
			var v Totals
			err = decode(&v)
			d.Totals[key] = v
		case "list":
			var v diskList
			err = decode(&v)
			d.Lists = append(d.Lists, v)
		case "season":
			var v diskSeason
			err = decode(&v)
			d.Seasons = append(d.Seasons, v)
		case "resultset":
			var v diskResultSet
			err = decode(&v)
			d.ResultSets = append(d.ResultSets, v)
		case "round":
			var v RoundScore
			err = decode(&v)
			d.RoundScores = append(d.RoundScores, v)
		case "meta":
			if key == "update_offset" {
				d.UpdateOffset, err = strconv.Atoi(value)
			}
		}
		if err != nil {
			return Store{}, fmt.Errorf("decode sqlite %s %s: %w", kind, key, err)
		}
	}
	return expandStore(d)
}

func columnString(stmt *C.sqlite3_stmt, col int) string {
	p := C.sqlite3_column_text(stmt, C.int(col))
	if p == nil {
		return ""
	}
	n := C.sqlite3_column_bytes(stmt, C.int(col))
	return string(C.GoBytes(unsafe.Pointer(p), n))
}

func (s *SQLiteStore) Save(data Store) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.exec("BEGIN IMMEDIATE"); err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			_ = s.exec("ROLLBACK")
		}
	}()
	d := compactStore(data)
	for key, value := range d.Fixtures {
		if err := s.put("fixture", key, value, false); err != nil {
			return err
		}
	}
	for _, value := range d.Forecasts {
		if err := s.put("forecast", forecastStorageKey(value), value, true); err != nil {
			return err
		}
	}
	for key, value := range d.Totals {
		if err := s.put("total", key, value, false); err != nil {
			return err
		}
	}
	for _, value := range d.Lists {
		if err := s.put("list", strconv.FormatInt(value.ChatID, 10), value, false); err != nil {
			return err
		}
	}
	for _, value := range d.Seasons {
		key := fmt.Sprintf("%d:%d", value.ChatID, value.Season)
		if err := s.put("season", key, value, false); err != nil {
			return err
		}
	}
	for _, value := range d.ResultSets {
		key := fmt.Sprintf("%d:%s", value.ChatID, value.FetchedAt)
		if value.Season > 0 && value.Matchday > 0 {
			key = fmt.Sprintf("%d:%d:%d", value.ChatID, value.Season, value.Matchday)
		}
		if err := s.put("resultset", key, value, false); err != nil {
			return err
		}
	}
	// Round scores can be deliberately removed when a forced refresh makes a
	// previously final fixture pending again. Keep this record kind in sync
	// with the in-memory snapshot inside the same transaction.
	if err := s.exec("DELETE FROM records WHERE kind='round'"); err != nil {
		return err
	}
	for _, value := range d.RoundScores {
		key := roundScoreKey(value.ChatID, value.UserID, value.Season, value.Matchday)
		if err := s.put("round", key, value, false); err != nil {
			return err
		}
	}
	if err := s.put("meta", "update_offset", d.UpdateOffset, false); err != nil {
		return err
	}
	if err := s.exec("COMMIT"); err != nil {
		return err
	}
	ok = true
	return nil
}

func (s *SQLiteStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	if err := s.exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return err
	}
	if rc := C.sqlite3_close_v2(s.db); rc != sqliteOK {
		return s.err()
	}
	s.db = nil
	return nil
}

// loadStore is used by tests and one-off local inspection. Runtime owns one open SQLite handle.
func loadStore(path string) (Store, error) {
	if file, err := os.Open(path); err == nil {
		var header [16]byte
		n, _ := file.Read(header[:])
		_ = file.Close()
		if n >= 16 && string(header[:]) == "SQLite format 3\x00" {
			s, data, err := OpenSQLiteStore(path)
			if err != nil {
				return Store{}, err
			}
			if err := s.Close(); err != nil {
				return Store{}, err
			}
			return data, nil
		}
		return Store{}, fmt.Errorf("expected SQLite database at %s; JSON history is not imported", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Store{}, err
	}
	s, data, err := OpenSQLiteStore(path)
	if err != nil {
		return Store{}, err
	}
	if err := s.Close(); err != nil {
		return Store{}, err
	}
	return data, nil
}

func saveStoreFile(path string, data Store) error {
	s, _, err := OpenSQLiteStore(path)
	if err != nil {
		return err
	}
	if err = s.Save(data); err != nil {
		_ = s.Close()
		return err
	}
	return s.Close()
}
