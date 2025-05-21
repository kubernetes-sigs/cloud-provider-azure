package threadsafesets

import (
	"strings"
	"sync"
)

// ThreadSafeIgnoreCaseSet is a set of strings that is case-insensitive and thread-safe.
type ThreadSafeIgnoreCaseSet struct {
	mu  sync.RWMutex
	set map[string]struct{}
}

func (s *ThreadSafeIgnoreCaseSet) Equals(other *ThreadSafeIgnoreCaseSet) bool {
	if !s.Initialized() || !other.Initialized() {
		return false
	}

	if s == other {
		return true
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	other.mu.RLock()
	defer other.mu.RUnlock()

	if len(s.set) != len(other.set) {
		return false
	}

	for item := range s.set {
		if _, exists := other.set[item]; !exists {
			return false
		}
	}

	return true
}

// NewString creates a new ThreadSafeIgnoreCaseSet with the given items.
func NewString(items ...string) *ThreadSafeIgnoreCaseSet {
	s := &ThreadSafeIgnoreCaseSet{
		set: make(map[string]struct{}),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, item := range items {
		s.set[strings.ToLower(item)] = struct{}{}
	}
	return s
}

// Insert adds the given items to the set. It only works if the set is initialized.
func (s *ThreadSafeIgnoreCaseSet) Insert(items ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, item := range items {
		s.set[strings.ToLower(item)] = struct{}{}
	}
}

// SafeInsert creates a new ThreadSafeIgnoreCaseSet with the given items if the set is not initialized.
// This is the recommended way to insert elements into the set.
func SafeInsert(s *ThreadSafeIgnoreCaseSet, items ...string) *ThreadSafeIgnoreCaseSet {
	if s.Initialized() {
		s.Insert(items...)
		return s
	}
	return NewString(items...)
}

// Delete removes the given item from the set.
// It will be a no-op if the set is not initialized or the item is not in the set.
func (s *ThreadSafeIgnoreCaseSet) Delete(item string) bool {
	if !s.Initialized() {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	item = strings.ToLower(item)
	_, exists := s.set[item]
	if exists {
		delete(s.set, item)
		return true
	}
	return false
}

// Has returns true if the given item is in the set, and the set is initialized.
func (s *ThreadSafeIgnoreCaseSet) Has(item string) bool {
	if !s.Initialized() {
		return false
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	_, exists := s.set[strings.ToLower(item)]
	return exists
}

// Initialized returns true if the set is initialized.
func (s *ThreadSafeIgnoreCaseSet) Initialized() bool {
	return s != nil && s.set != nil
}

// UnsortedList returns the items in the set in an arbitrary order.
func (s *ThreadSafeIgnoreCaseSet) UnsortedList() []string {
	if !s.Initialized() {
		return []string{}
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	var list []string
	for item := range s.set {
		list = append(list, item)
	}
	return list
}

// Len returns the number of items in the set.
func (s *ThreadSafeIgnoreCaseSet) Len() int {
	if !s.Initialized() {
		return 0
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	return len(s.set)
}
