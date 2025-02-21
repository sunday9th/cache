package cache

import (
	"sync"
	"sync/atomic"
	"time"
)

func now() int64 {
	return atomic.LoadInt64(&clock)
}

var clock = time.Now().UnixNano()

func init() {
	go func() {
		for {
			// calibration every second
			atomic.StoreInt64(&clock, time.Now().UnixNano())
			for i := 0; i < 9; i++ {
				time.Sleep(100 * time.Millisecond)
				atomic.AddInt64(&clock, int64(100*time.Millisecond))
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()
}

// node to store cache item
type node struct {
	p, n *node
	k    string
	v    interface{}
	ts   int64 // nano timestamp
}

// a data structure that is efficient to insert/fetch/delete cache items [both O(1) time complexity]
type cache struct {
	cap  int
	hmap map[string]*node
	head *node // not use pointer-to-pointer here,
	tail *node // coz it's trade-off for performance
}

// create a new lru cache object
func create(cap int) *cache {
	return &cache{cap, make(map[string]*node, cap), nil, nil}
}

// put a cache item into lru cache, if insert return true
func (c *cache) put(k string, v interface{}) int {
	if e, ok := c.hmap[k]; ok {
		e.v, e.ts = v, now()
		c._refresh(e)
		return 0
	}

	if c.cap <= 0 {
		return 0
	} else if len(c.hmap) >= c.cap {
		// transfer the tail item as the new item, then refresh
		delete(c.hmap, c.tail.k)
		c.tail.k, c.tail.v, c.tail.ts = k, v, now() // reuse to reduce gc
		c.hmap[k] = c.tail
		c._refresh(c.tail)
		return -1
	}

	e := &node{nil, c.head, k, v, now()}
	if len(c.hmap) <= 0 {
		c.tail = e
	} else {
		c.head.p = e
	}
	c.hmap[k], c.head = e, e
	return 1
}

// get value of key from lru cache with result
func (c *cache) get(k string) (*node, bool) {
	if e, ok := c.hmap[k]; ok {
		c._refresh(e)
		return e, ok
	}
	return nil, false
}

// delete item by key from lru cache
func (c *cache) del(k string) (*node, bool) {
	if e, ok := c.hmap[k]; ok {
		delete(c.hmap, k)
		c._remove(e)
		return e, true
	}
	return nil, false
}

// calls f sequentially for each key and value present in the lru cache
func (c *cache) foreach(f func(k string, v interface{}) bool) {
	for i := c.head; i != nil; i = i.n {
		if !f(i.k, i.v) {
			break
		}
	}
}

// length of lru cache
func (c *cache) length() int {
	return len(c.hmap)
}

func (c *cache) _refresh(e *node) {
	if e.p == nil { // head node
		return
	}
	e.p.n = e.n
	if e.n == nil { // tail node
		c.tail = e.p
	} else {
		e.n.p = e.p
	}
	e.p, e.n, c.head.p, c.head = nil, c.head, e, e
}

func (c *cache) _remove(e *node) {
	if e.p == nil { // head node
		c.head = e.n
	} else {
		e.p.n = e.n
	}
	if e.n == nil { // tail node
		c.tail = e.p
	} else {
		e.n.p = e.p
	}
}

// hashCode hashes a string to a unique hashcode. BKDR hash as default
func hashCode(s string) (hash int) {
	for i := 0; i < len(s); i++ {
		hash = hash*131 + int(s[i])
	}
	return hash
}

// Cache - concurrent cache structure
type Cache struct {
	locks  []sync.Mutex
	insts  [][2]*cache // level-0 for normal LRU, level-1 for LRU-2
	mask   int
	expire time.Duration
	on     inspector
}

func nextPowOf2(cap int) int {
	if cap <= 1 {
		return 1
	}
	if cap&(cap-1) == 0 {
		return cap
	}
	cap |= cap >> 1
	cap |= cap >> 2
	cap |= cap >> 4
	cap |= cap >> 8
	cap |= cap >> 16
	return cap + 1
}

// NewLRUCache - create lru cache
// `bucketCnt` is buckets that shard items to reduce lock racing
// `capPerBkt` is length of each bucket
// can store `capPerBkt * bucketCnt` count of element in Cache at most
// `expire` is expiration that item alive (and we only use lazy eviction here)
func NewLRUCache(bucketCnt int, capPerBkt int, expire time.Duration) *Cache {
	size := nextPowOf2(bucketCnt)
	c := &Cache{make([]sync.Mutex, size), make([][2]*cache, size), size - 1, expire, func(int, string, int) {}}
	for i := range c.insts {
		c.insts[i][0] = create(capPerBkt)
	}
	return c
}

// LRU2 - add LRU-2 support (especially LRU-2 that when item visited twice it moves to upper-level-cache)
// `capPerBkt` is length of each LRU-2 bucket
// can store extra `capPerBkt * bucketCnt` count of element in Cache at most
func (c *Cache) LRU2(capPerBkt int) *Cache {
	for i := range c.insts {
		c.insts[i][1] = create(capPerBkt)
	}
	return c
}

// Put - put a item into cache
func (c *Cache) Put(key string, val interface{}) {
	idx := hashCode(key) & c.mask
	c.locks[idx].Lock()
	ok := c.insts[idx][0].put(key, val)
	c.locks[idx].Unlock()
	c.on(PUT, key, ok)
}

// internal sub function that get item at specific level
func (c *Cache) get(key string, idx, level int) (*node, bool) {
	if n, b := c.insts[idx][level].get(key); b {
		if now()-n.ts > int64(c.expire) {
			// not necessary to remove the expired item here
			// removal is also ok that can control the memory usage before the cache is full, but will cause GC thrashing
			// c.insts[idx][level].del(key)
			return n, false
		}
		return n, b
	}
	return nil, false
}

// Get - get value of key from cache with result
// if the item is expired, maybe you can also get the former item even if it returns `false`
func (c *Cache) Get(key string) (v interface{}, b bool) {
	idx := hashCode(key) & c.mask
	c.locks[idx].Lock()
	var n *node
	if c.insts[idx][1] == nil { // (if LRU-2 mode not support, loss is little)
		// normal lru mode
		n, b = c.get(key, idx, 0)
	} else {
		// LRU-2 mode
		n, b = c.insts[idx][0].del(key)
		if !b {
			// re-find in level-1
			n, b = c.get(key, idx, 1)
		} else {
			// find in level-0, move to level-1
			c.insts[idx][1].put(key, n.v)
		}
	}
	if !b {
		c.locks[idx].Unlock()
		c.on(GET, key, 0)
		return v, false
	}
	c.locks[idx].Unlock()
	c.on(GET, key, 1)
	return n.v, b
}

// Del - delete item by key from cache
func (c *Cache) Del(key string) {
	idx := hashCode(key) & c.mask
	c.locks[idx].Lock()
	_, b := c.insts[idx][0].del(key)
	if c.insts[idx][1] != nil { // (if LRU-2 mode not support, loss is little)
		_, b2 := c.insts[idx][1].del(key)
		b = b || b2
	}
	c.locks[idx].Unlock()
	if b {
		c.on(DEL, key, 1)
	} else {
		c.on(DEL, key, 0)
	}
}

const (
	PUT = iota + 1
	GET
	DEL
)

// can be used to statistics like cache hit/miss rate
type inspector func(action int, key string, ok int)

// Inspect - to inspect the actions
func (c *Cache) Inspect(insptr inspector) {
	old := c.on
	c.on = func(action int, key string, ok int) {
		insptr(action, key, ok)
		old(action, key, ok)
	}
}
