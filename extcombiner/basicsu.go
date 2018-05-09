package extcombiner

import (
	"runtime"
	"sync"
	"sync/atomic"
	"unsafe"
)

// based on https://software.intel.com/en-us/blogs/2013/02/22/combineraggregator-synchronization-primitive
type BasicSleepyUintptr struct {
	head    uintptr // *basicSleepyUintptrNode
	_       [7]uint64
	lock    sync.Mutex
	cond    sync.Cond
	_       [0]uint64
	batcher Batcher
}

type basicSleepyUintptrNode struct {
	next     uintptr // *basicSleepyUintptrNode
	argument interface{}
}

func NewBasicSleepyUintptr(batcher Batcher) *BasicSleepyUintptr {
	c := &BasicSleepyUintptr{
		batcher: batcher,
		head:    0,
	}
	c.cond.L = &c.lock
	return c
}

const basicSleepyUintptrLocked = uintptr(1)

func (c *BasicSleepyUintptr) Do(op interface{}) {
	node := &basicSleepyUintptrNode{argument: op}

	var cmp uintptr
	for {
		cmp = atomic.LoadUintptr(&c.head)
		xchg := basicSleepyUintptrLocked
		if cmp != 0 {
			// There is already a combiner, enqueue itself.
			xchg = uintptr(unsafe.Pointer(node))
			node.next = cmp
		}

		if atomic.CompareAndSwapUintptr(&c.head, cmp, xchg) {
			break
		}
	}

	if cmp != 0 {
		for try := 0; try < busyspin; try++ {
			if atomic.LoadUintptr(&node.next) == 0 {
				return
			}
			runtime.Gosched()
		}

		c.lock.Lock()
		for atomic.LoadUintptr(&node.next) != 0 {
			c.cond.Wait()
		}
		c.lock.Unlock()
	} else {
		c.batcher.Start()

		c.batcher.Include(node.argument)

		for {
			for {
				cmp = atomic.LoadUintptr(&c.head)
				// If there are some operations in the list,
				// grab the list and replace with LOCKED.
				// Otherwise, exchange to nil.
				var xchg uintptr = 0
				if cmp != basicSleepyUintptrLocked {
					xchg = basicSleepyUintptrLocked
				}

				if atomic.CompareAndSwapUintptr(&c.head, cmp, xchg) {
					break
				}
			}

			// No more operations to combine, return.
			if cmp == basicSleepyUintptrLocked {
				break
			}

			// Execute the list of operations.
			for cmp != basicSleepyUintptrLocked {
				node = (*basicSleepyUintptrNode)(unsafe.Pointer(cmp))
				cmp = node.next

				c.batcher.Include(node.argument)
				// Mark completion.
				atomic.StoreUintptr(&node.next, 0)
			}
		}

		c.batcher.Finish()

		c.lock.Lock()
		c.cond.Broadcast()
		c.lock.Unlock()
	}
}