package redis

import (
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/garyburd/redigo/redis"
	"github.com/jpillora/backoff"
	"github.com/prometheus/client_golang/prometheus"
)

var (
	keyWatcher            = make(map[string][]chan bool)
	keyMutex              sync.Mutex
	redisReconnectTimeout = backoff.Backoff{
		//These are the defaults
		Min:    100 * time.Millisecond,
		Max:    60 * time.Second,
		Factor: 2,
		Jitter: true,
	}
	keyWatchers = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "gitlab_workhorse_keywatcher_keywatchers",
			Help: "The number of keys that is being watched by gitlab-workhorse",
		},
	)
	hitMissCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gitlab_workhorse_keywatcher_hit_miss",
			Help: "How many redis queries have been completed by gitlab-workhorse, partitioned by hit and miss",
		},
		[]string{"status"},
	)
)

func init() {
	prometheus.MustRegister(keyWatchers)
}

const (
	keyPubEventSet     = "__keyevent@*__:set"
	keyPubEventExpired = "__keyevent@*__:expired"
	promStatusMiss     = "miss"
	promStatusHit      = "hit"
)

// KeyChan holds a key and a channel
type KeyChan struct {
	Key  string
	Chan chan bool
}

func processInner(conn redis.Conn) {
	defer func() {
		conn.Close()
		openConnections.Dec()
	}()
	psc := redis.PubSubConn{Conn: conn}
	if err := psc.PSubscribe(keyPubEventSet); err != nil {
		return
	}
	defer psc.PUnsubscribe(keyPubEventSet)
	if err := psc.PSubscribe(keyPubEventExpired); err != nil {
		return
	}
	defer psc.PUnsubscribe(keyPubEventExpired)

	for {
		switch v := psc.Receive().(type) {
		case redis.PMessage:
			notifyChanWatchers(string(v.Data))
		case error:
			return
		}
	}
}

// Process redis subscriptions
func Process() {
	go func() {
		log.Print("Processing redis queue")

		for {
			conn, err := redisDialFunc()
			if err == nil {
				processInner(conn)
				redisReconnectTimeout.Reset()
			} else {
				time.Sleep(redisReconnectTimeout.Duration())
			}
		}
	}()
}

func notifyChanWatchers(key string) {
	keyMutex.Lock()
	defer keyMutex.Unlock()
	if chanList, ok := keyWatcher[key]; ok {
		for _, c := range chanList {
			c <- true
			keyWatchers.Dec()
		}
		delete(keyWatcher, key)
	}
}

func addKeyChan(kc *KeyChan) {
	keyMutex.Lock()
	defer keyMutex.Unlock()
	keyWatcher[kc.Key] = append(keyWatcher[kc.Key], kc.Chan)
	keyWatchers.Inc()
}

func delKeyChan(kc *KeyChan) {
	keyMutex.Lock()
	defer keyMutex.Unlock()
	if chans, ok := keyWatcher[kc.Key]; ok {
		for i, c := range chans {
			if kc.Chan == c {
				keyWatcher[kc.Key] = append(chans[:i], chans[i+1:]...)
				keyWatchers.Dec()
				break
			}
		}
		if len(keyWatcher[kc.Key]) == 0 {
			delete(keyWatcher, kc.Key)
		}
	}
}

// WatchKeyStatus is used to tell how WatchKey returned
type WatchKeyStatus int

const (
	// WatchKeyStatusFailure is return when there's a failure
	WatchKeyStatusFailure WatchKeyStatus = iota
	// WatchKeyStatusNotifiedNoChange for when re-set by Rails
	WatchKeyStatusNotifiedNoChange
	// WatchKeyStatusTimedout when the function timed out
	WatchKeyStatusTimedout
	// WatchKeyStatusImmediately for when the key had already changed
	WatchKeyStatusImmediately
	// WatchKeyStatusNotified for when the key changed during the call
	WatchKeyStatusNotified
)

// WatchKey waits for a key to be updated or expired
//
// Returns true if the value has changed, otherwise false
func WatchKey(key, value string, timeout time.Duration) (WatchKeyStatus, error) {
	kw := &KeyChan{
		Key:  key,
		Chan: make(chan bool, 1),
	}

	addKeyChan(kw)
	defer delKeyChan(kw)

	currentValue, err := GetString(key)
	if err != nil {
		return WatchKeyStatusFailure, fmt.Errorf("Failed to get value from Redis: %#v", err)
	}
	if currentValue != value {
		hitMissCounter.WithLabelValues(promStatusMiss).Inc()
		return WatchKeyStatusImmediately, nil
	}

	select {
	case <-kw.Chan:
		currentValue, err = GetString(key)
		if err != nil {
			return WatchKeyStatusFailure, fmt.Errorf("Failed to get value from Redis: %#v", err)
		}
		if currentValue != value {
			hitMissCounter.WithLabelValues(promStatusMiss).Inc()
			return WatchKeyStatusNotified, nil
		}
		hitMissCounter.WithLabelValues(promStatusHit).Inc()
		return WatchKeyStatusNotifiedNoChange, nil

	case <-time.After(timeout):
		hitMissCounter.WithLabelValues(promStatusHit).Inc()
		return WatchKeyStatusTimedout, nil
	}
}
