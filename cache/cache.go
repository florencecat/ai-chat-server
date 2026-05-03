package cache

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
)

var bucket = []byte("cache")

type entry struct {
	Value     json.RawMessage `json:"v"`
	ExpiresAt int64           `json:"e"` // unix nano
}

type Cache struct {
	db  *bolt.DB
	ttl time.Duration
}

func New(db *bolt.DB, ttl time.Duration) (*Cache, error) {
	if err := db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(bucket)
		return err
	}); err != nil {
		return nil, err
	}
	return &Cache{db: db, ttl: ttl}, nil
}

func (c *Cache) key(message string) []byte {
	h := sha256.Sum256([]byte(message))
	return []byte(fmt.Sprintf("%x", h))
}

func (c *Cache) Get(message string) (json.RawMessage, bool) {
	var result json.RawMessage
	_ = c.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bucket).Get(c.key(message))
		if v == nil {
			return nil
		}
		var e entry
		if err := json.Unmarshal(v, &e); err != nil {
			return nil
		}
		if time.Now().UnixNano() > e.ExpiresAt {
			return nil // expired
		}
		result = e.Value
		return nil
	})
	return result, result != nil
}

func (c *Cache) Set(message string, value json.RawMessage) error {
	e := entry{Value: value, ExpiresAt: time.Now().Add(c.ttl).UnixNano()}
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return c.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucket).Put(c.key(message), data)
	})
}
