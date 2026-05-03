package quota

import (
	"encoding/json"
	"errors"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	ErrRateLimitMinute = errors.New("rate limit: only 1 request per minute allowed")
	ErrRateLimitDay    = errors.New("daily quota exceeded: max 15 requests per day")
)

var bucket = []byte("quotas")

type record struct {
	LastRequestAt int64 `json:"last_at"`  // unix nano
	DayCount      int   `json:"day_count"`
	DayResetAt    int64 `json:"day_reset"` // unix nano, start of tracked day
}

type Info struct {
	RequestsToday  int        `json:"requests_today"`
	LimitDay       int        `json:"limit_day"`
	LimitMinute    int        `json:"limit_minute"`
	NextRequestAt  *time.Time `json:"next_request_at,omitempty"` // set when minute cooldown active
}

type Manager struct {
	db         *bolt.DB
	perMinute  int
	perDay     int
}

func New(db *bolt.DB, perMinute, perDay int) (*Manager, error) {
	if err := db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(bucket)
		return err
	}); err != nil {
		return nil, err
	}
	return &Manager{db: db, perMinute: perMinute, perDay: perDay}, nil
}

func dayStart(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

func (m *Manager) load(tx *bolt.Tx, userID string) record {
	v := tx.Bucket(bucket).Get([]byte(userID))
	if v == nil {
		return record{}
	}
	var r record
	_ = json.Unmarshal(v, &r)
	return r
}

func (m *Manager) save(tx *bolt.Tx, userID string, r record) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return tx.Bucket(bucket).Put([]byte(userID), data)
}

// Check returns an error if the user has exceeded any quota. Does not modify state.
func (m *Manager) Check(userID string) error {
	return m.db.View(func(tx *bolt.Tx) error {
		r := m.load(tx, userID)
		now := time.Now()

		if r.LastRequestAt > 0 {
			elapsed := now.Sub(time.Unix(0, r.LastRequestAt))
			cooldown := time.Minute / time.Duration(m.perMinute)
			if elapsed < cooldown {
				return ErrRateLimitMinute
			}
		}

		today := dayStart(now)
		dayIsActive := r.DayResetAt > 0 && !time.Unix(0, r.DayResetAt).Before(today)
		if dayIsActive && r.DayCount >= m.perDay {
			return ErrRateLimitDay
		}

		return nil
	})
}

// Consume records a successful request for the user.
func (m *Manager) Consume(userID string) error {
	return m.db.Update(func(tx *bolt.Tx) error {
		r := m.load(tx, userID)
		now := time.Now()
		today := dayStart(now)

		if r.DayResetAt == 0 || time.Unix(0, r.DayResetAt).Before(today) {
			r.DayCount = 0
			r.DayResetAt = today.UnixNano()
		}

		r.LastRequestAt = now.UnixNano()
		r.DayCount++

		return m.save(tx, userID, r)
	})
}

// Info returns current quota status for the user.
func (m *Manager) Info(userID string) Info {
	info := Info{LimitDay: m.perDay, LimitMinute: m.perMinute}

	_ = m.db.View(func(tx *bolt.Tx) error {
		r := m.load(tx, userID)
		now := time.Now()
		today := dayStart(now)

		if r.DayResetAt > 0 && !time.Unix(0, r.DayResetAt).Before(today) {
			info.RequestsToday = r.DayCount
		}

		if r.LastRequestAt > 0 {
			cooldown := time.Minute / time.Duration(m.perMinute)
			next := time.Unix(0, r.LastRequestAt).Add(cooldown)
			if next.After(now) {
				info.NextRequestAt = &next
			}
		}

		return nil
	})

	return info
}
