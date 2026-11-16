package service

import "github.com/orpheus-agents/orpheus-mattermost/internal/conversation"

// runCapacity owns admission accounting, independently of delivery recovery.
// Runtime.mu serializes all access. A reservation counts until a subsequent
// thread observation establishes whether Orpheus accepted it.
type runCapacity struct {
	active   map[conversation.Key]int
	ready    map[string]bool
	versions map[string]uint64
}

func newRunCapacity() runCapacity {
	return runCapacity{active: map[conversation.Key]int{}, ready: map[string]bool{}, versions: map[string]uint64{}}
}

// restore only adds discovered work: a paginated list cannot disprove local
// reservations or work outside its time window. Thread observations release it.
func (c *runCapacity) restore(workflow string, active map[conversation.Key]int) {
	for key, n := range active {
		c.active[key] = max(c.active[key], n)
	}
	c.ready[workflow] = true
	c.versions[workflow]++
}

func (c *runCapacity) reserve(key conversation.Key, limit int) bool {
	if !c.ready[key.Workflow] || c.active[key] > 0 {
		return false
	}
	count := 0
	for k, n := range c.active {
		if k.Workflow == key.Workflow {
			count += n
		}
	}
	if count >= limit {
		return false
	}
	c.active[key] = 1
	return true
}

func (c *runCapacity) observe(key conversation.Key, count int) bool {
	old := c.active[key]
	if count == 0 {
		delete(c.active, key)
	} else {
		c.active[key] = count
	}
	return old > count
}

func (c *runCapacity) count(key conversation.Key) int { return c.active[key] }

func (c *runCapacity) total() int {
	total := 0
	for _, n := range c.active {
		total += n
	}
	return total
}
