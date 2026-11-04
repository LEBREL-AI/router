package settle

// deque is the line of operations ready to go: first in, first out, and one that has waited
// already (a settlement whose acceptance just landed, a batch put back) can go to the front.
type deque struct {
	items []*Op
	head  int
	size  int
}

func (d *deque) Len() int { return d.size }

func (d *deque) grow() {
	if d.size < len(d.items) {
		return
	}
	items := make([]*Op, max(16, 2*len(d.items)))
	for i := 0; i < d.size; i++ {
		items[i] = d.items[(d.head+i)%len(d.items)]
	}
	d.items, d.head = items, 0
}

func (d *deque) PushBack(op *Op) {
	d.grow()
	d.items[(d.head+d.size)%len(d.items)] = op
	d.size++
}

func (d *deque) PushFront(op *Op) {
	d.grow()
	d.head = (d.head - 1 + len(d.items)) % len(d.items)
	d.items[d.head] = op
	d.size++
}

// Front is the operation PopFront would return; nil when the line is empty.
func (d *deque) Front() *Op {
	if d.size == 0 {
		return nil
	}
	return d.items[d.head]
}

func (d *deque) PopFront() *Op {
	op := d.items[d.head]
	d.items[d.head] = nil
	d.head = (d.head + 1) % len(d.items)
	d.size--
	return op
}

// delayHeap holds the operations backing off, the soonest due on top (container/heap).
type delayHeap []*Op

func (h delayHeap) Len() int           { return len(h) }
func (h delayHeap) Less(i, j int) bool { return h[i].notBefore.Before(h[j].notBefore) }
func (h delayHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *delayHeap) Push(x any)        { *h = append(*h, x.(*Op)) }
func (h *delayHeap) Pop() any {
	old := *h
	n := len(old)
	op := old[n-1]
	old[n-1] = nil
	*h = old[:n-1]
	return op
}
