package stats

import (
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/orca-zhang/cache"
)

var m sync.Map

type StatsNode struct {
	// don't reorder them or add field between them
	Evicted, Updated, Added, GetMiss, GetHit, DelMiss, DelHit uint64
}

// Bind - to stats a cache
// `pool` can be used to classify instances that store same items
func Bind(pool string, caches ...*cache.Cache) error {
	v, _ := m.LoadOrStore(pool, &StatsNode{})
	for _, c := range caches {
		c.Inspect(func(action int, _ string, ok int) {
			// very, very, very low-cost for stats
			atomic.AddUint64((*uint64)(unsafe.Pointer(uintptr(unsafe.Pointer(v.(*StatsNode)))+uintptr(ok+action*2-1)*unsafe.Sizeof(&ok))), 1)
		})
	}
	return nil
}

// Stats - get the result like follows
//
// `k` is categoy, type is string
// `v` is node, type is `*stats.StatsNode`
//
// stats.Stats().Range(func(k, v interface{}) bool {
//     fmt.Println("stats:", k, v)
//     return true
// })
func Stats() *sync.Map {
	return &m
}
