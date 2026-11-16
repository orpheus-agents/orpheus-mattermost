package service

import (
	"testing"

	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
)

func TestRunCapacityRestorationAndReservations(t *testing.T) {
	first := conversation.Key{Workflow: "first", Root: "old"}
	fresh := first
	fresh.Root = "new"
	other := conversation.Key{Workflow: "other", Root: "old"}
	c := newRunCapacity()
	if c.reserve(first, 3) {
		t.Fatal("admission before startup accounting")
	}
	// Two unfinished generations occupy two places, even in the same thread.
	c.restore("first", map[conversation.Key]int{first: 2})
	if !c.reserve(fresh, 3) || c.reserve(fresh, 3) || c.reserve(other, 3) {
		t.Fatal("reservation or per-workflow initialization was ignored")
	}
	// Empty or older discovery cannot release an in-flight/lost-response
	// reservation, or tracked work absent from the latest time window.
	c.restore("first", nil)
	c.restore("first", map[conversation.Key]int{first: 1})
	if c.count(first) != 2 || c.count(fresh) != 1 || c.total() != 3 {
		t.Fatal("discovery replaced newer local accounting")
	}
	c.restore("other", nil)
	if !c.reserve(other, 1) {
		t.Fatal("one workflow consumed another workflow's limit")
	}
	clear(c.ready) // A configuration reload requires a new discovery.
	if c.reserve(conversation.Key{Workflow: "first", Root: "third"}, 10) || c.total() != 4 {
		t.Fatal("reload admitted work or discarded reservations")
	}
	c.restore("first", nil)
	if !c.observe(first, 0) || !c.observe(fresh, 0) || !c.reserve(fresh, 1) || c.total() != 2 {
		t.Fatal("authoritative observation did not release capacity")
	}
}
