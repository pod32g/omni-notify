package notifier

import (
	"hash/fnv"
	"sync"
)

// shardedMutex serializes work by string key using a fixed pool of mutexes, so
// memory stays bounded regardless of how many distinct keys are seen. Two keys
// may occasionally share a shard (harmless extra serialization).
type shardedMutex struct {
	shards [256]sync.Mutex
}

// lock locks and returns the mutex guarding key; the caller must Unlock it.
func (s *shardedMutex) lock(key string) *sync.Mutex {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	m := &s.shards[h.Sum32()%uint32(len(s.shards))]
	m.Lock()
	return m
}
