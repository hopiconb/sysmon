// Package store persists samples to SQLite for historical querying.
package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/hopiconb/sysmon/internal/collector"
)

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	// modernc.org/sqlite serializes writes per connection; a single
	// connection avoids SQLITE_BUSY between the daemon's writer and readers.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS samples (
			ts   INTEGER NOT NULL,
			cpu  REAL NOT NULL,
			mem  REAL NOT NULL,
			temp REAL NOT NULL
		);
		CREATE TABLE IF NOT EXISTS proc_samples (
			ts     INTEGER NOT NULL,
			pid    INTEGER NOT NULL,
			name   TEXT NOT NULL,
			cpu    REAL NOT NULL,
			mem_mb REAL NOT NULL
		);
		CREATE TABLE IF NOT EXISTS sensor_samples (
			ts    INTEGER NOT NULL,
			chip  TEXT NOT NULL,
			label TEXT NOT NULL,
			kind  TEXT NOT NULL,
			value REAL NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_samples_ts ON samples(ts);
		CREATE INDEX IF NOT EXISTS idx_proc_name_ts ON proc_samples(name, ts);
		CREATE INDEX IF NOT EXISTS idx_sensor_chip_ts ON sensor_samples(chip, label, ts);
		CREATE INDEX IF NOT EXISTS idx_sensor_ts ON sensor_samples(ts);
	`); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Write(sm collector.Sample) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	ts := sm.Timestamp.Unix()
	if _, err := tx.Exec(
		`INSERT INTO samples (ts, cpu, mem, temp) VALUES (?, ?, ?, ?)`,
		ts, sm.CPUPct, sm.MemPct, sm.MaxTemp(),
	); err != nil {
		return err
	}
	for _, p := range sm.Procs {
		if _, err := tx.Exec(
			`INSERT INTO proc_samples (ts, pid, name, cpu, mem_mb) VALUES (?, ?, ?, ?, ?)`,
			ts, p.PID, p.Name, p.CPUPct, p.MemMB,
		); err != nil {
			return err
		}
	}
	for _, r := range sm.Sensors {
		if _, err := tx.Exec(
			`INSERT INTO sensor_samples (ts, chip, label, kind, value) VALUES (?, ?, ?, ?, ?)`,
			ts, r.Chip, r.Label, string(r.Kind), r.Value,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Prune deletes rows older than the retention window. Freed pages are
// reused by subsequent inserts, so the file stops growing at steady state.
func (s *Store) Prune(retention time.Duration) error {
	cutoff := time.Now().Add(-retention).Unix()
	for _, table := range []string{"samples", "proc_samples", "sensor_samples"} {
		if _, err := s.db.Exec(`DELETE FROM `+table+` WHERE ts < ?`, cutoff); err != nil {
			return err
		}
	}
	return nil
}

// SystemRow is one system-wide sample row for history views.
type SystemRow struct {
	TS   time.Time
	CPU  float64
	Mem  float64
	Temp float64
}

// RecentSystem returns the last n system samples, oldest first.
func (s *Store) RecentSystem(n int) ([]SystemRow, error) {
	rows, err := s.db.Query(
		`SELECT ts, cpu, mem, temp FROM samples ORDER BY ts DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SystemRow
	for rows.Next() {
		var r SystemRow
		var ts int64
		if err := rows.Scan(&ts, &r.CPU, &r.Mem, &r.Temp); err != nil {
			return nil, err
		}
		r.TS = time.Unix(ts, 0)
		out = append(out, r)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

// span returns the query start time and bucket width (both unix seconds)
// for a range of `since` (0 = everything) downsampled to ~buckets points.
func (s *Store) span(since time.Duration, buckets int) (start, bucket int64) {
	now := time.Now().Unix()
	if since > 0 {
		start = now - int64(since.Seconds())
	} else {
		// All-time: find the oldest sample.
		if err := s.db.QueryRow(`SELECT COALESCE(MIN(ts), ?) FROM samples`, now).Scan(&start); err != nil {
			start = now
		}
	}
	if buckets < 10 {
		buckets = 10
	}
	bucket = (now - start) / int64(buckets)
	if bucket < 1 {
		bucket = 1
	}
	return start, bucket
}

func (s *Store) series(query string, args ...any) ([]float64, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []float64
	for rows.Next() {
		var b int64
		var v float64
		if err := rows.Scan(&b, &v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// SystemSeries returns a downsampled series of a system metric
// (cpu | mem | temp) over the range, oldest first.
func (s *Store) SystemSeries(metric string, since time.Duration, buckets int) ([]float64, error) {
	switch metric {
	case "cpu", "mem", "temp":
	default:
		return nil, fmt.Errorf("unknown system metric %q", metric)
	}
	start, bucket := s.span(since, buckets)
	return s.series(
		`SELECT (ts/?)*? AS b, AVG(`+metric+`) FROM samples
		 WHERE ts >= ? GROUP BY b ORDER BY b`,
		bucket, bucket, start)
}

// ProcSeries returns a downsampled series for processes whose name
// matches (substring): total CPU %% or total memory MB across matching
// PIDs, summed per sample then averaged per bucket.
func (s *Store) ProcSeries(name, metric string, since time.Duration, buckets int) ([]float64, error) {
	col := "cpu"
	if metric == "mem" {
		col = "mem_mb"
	}
	start, bucket := s.span(since, buckets)
	return s.series(
		`SELECT (ts/?)*? AS b, AVG(total) FROM
		   (SELECT ts, SUM(`+col+`) AS total FROM proc_samples
		    WHERE name LIKE ? AND ts >= ? GROUP BY ts)
		 GROUP BY b ORDER BY b`,
		bucket, bucket, "%"+name+"%", start)
}

// SensorSeries returns a downsampled series for one sensor, matched by
// substring on "chip/label".
func (s *Store) SensorSeries(key string, since time.Duration, buckets int) ([]float64, error) {
	start, bucket := s.span(since, buckets)
	return s.series(
		`SELECT (ts/?)*? AS b, AVG(value) FROM sensor_samples
		 WHERE (chip || '/' || label) LIKE ? AND ts >= ? GROUP BY b ORDER BY b`,
		bucket, bucket, "%"+key+"%", start)
}

// ProcFilter narrows QueryProcs. Zero values mean "no constraint".
type ProcFilter struct {
	Name   string        // substring match on process name
	Since  time.Duration // only rows newer than now-Since
	MinCPU float64       // only rows with cpu >= MinCPU
	Limit  int
}

type ProcRow struct {
	TS    time.Time
	PID   int32
	Name  string
	CPU   float64
	MemMB float64
}

// QueryProcs backs the logs tab: filterable per-process history, newest first.
func (s *Store) QueryProcs(f ProcFilter) ([]ProcRow, error) {
	var conds []string
	var args []any
	if f.Name != "" {
		conds = append(conds, `name LIKE ?`)
		args = append(args, "%"+f.Name+"%")
	}
	if f.Since > 0 {
		conds = append(conds, `ts >= ?`)
		args = append(args, time.Now().Add(-f.Since).Unix())
	}
	if f.MinCPU > 0 {
		conds = append(conds, `cpu >= ?`)
		args = append(args, f.MinCPU)
	}
	q := `SELECT ts, pid, name, cpu, mem_mb FROM proc_samples`
	if len(conds) > 0 {
		q += ` WHERE ` + strings.Join(conds, ` AND `)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 500
	}
	q += fmt.Sprintf(` ORDER BY ts DESC LIMIT %d`, limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ProcRow
	for rows.Next() {
		var r ProcRow
		var ts int64
		if err := rows.Scan(&ts, &r.PID, &r.Name, &r.CPU, &r.MemMB); err != nil {
			return nil, err
		}
		r.TS = time.Unix(ts, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}
