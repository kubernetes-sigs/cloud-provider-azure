package threadsafesets

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
)

func TestNewString(t *testing.T) {
	s := NewString("Apple", "Banana", "cherry")

	if !s.Initialized() {
		t.Error("Set should be initialized")
	}
	if s.Len() != 3 {
		t.Errorf("Set should have 3 items, got %d", s.Len())
	}
	if !s.Has("apple") {
		t.Error("Set should have 'apple'")
	}
	if !s.Has("BANANA") {
		t.Error("Set should have 'BANANA' (case-insensitive)")
	}
	if !s.Has("Cherry") {
		t.Error("Set should have 'Cherry' (case-insensitive)")
	}
}

func TestInsert(t *testing.T) {
	s := NewString("apple")
	s.Insert("Banana", "CHERRY")

	if s.Len() != 3 {
		t.Errorf("Set should have 3 items, got %d", s.Len())
	}
	if !s.Has("apple") {
		t.Error("Set should have 'apple'")
	}
	if !s.Has("banana") {
		t.Error("Set should have 'banana'")
	}
	if !s.Has("cherry") {
		t.Error("Set should have 'cherry'")
	}

	// Test inserting duplicate (case-insensitive)
	s.Insert("APPLE")
	if s.Len() != 3 {
		t.Errorf("Set should still have 3 items after inserting duplicate, got %d", s.Len())
	}
}

func TestSafeInsert(t *testing.T) {
	// Test with initialized set
	s := NewString("apple")
	result := SafeInsert(s, "banana")

	if result != s {
		t.Error("SafeInsert should return the original set when it's initialized")
	}
	if !s.Has("banana") {
		t.Error("Set should have 'banana'")
	}

	// Test with nil set
	var nilSet *ThreadSafeIgnoreCaseSet
	result = SafeInsert(nilSet, "apple", "banana")

	if !result.Initialized() {
		t.Error("SafeInsert should return initialized set")
	}
	if result.Len() != 2 {
		t.Errorf("Set should have 2 items, got %d", result.Len())
	}
	if !result.Has("apple") {
		t.Error("Set should have 'apple'")
	}
}

func TestDelete(t *testing.T) {
	s := NewString("apple", "banana", "cherry")

	// Test successful delete
	success := s.Delete("APPLE")
	if !success {
		t.Error("Delete should return true when item exists")
	}
	if s.Has("apple") {
		t.Error("Set should not have 'apple' after deletion")
	}
	if s.Len() != 2 {
		t.Errorf("Set should have 2 items after deletion, got %d", s.Len())
	}

	// Test delete non-existent item
	success = s.Delete("orange")
	if success {
		t.Error("Delete should return false when item doesn't exist")
	}
	if s.Len() != 2 {
		t.Errorf("Set should still have 2 items, got %d", s.Len())
	}

	// Test delete on nil set
	var nilSet *ThreadSafeIgnoreCaseSet
	success = nilSet.Delete("apple")
	if success {
		t.Error("Delete on nil set should return false")
	}
}

func TestHas(t *testing.T) {
	s := NewString("Apple", "BANANA")

	if !s.Has("apple") {
		t.Error("Set should have 'apple' (case-insensitive)")
	}
	if !s.Has("APPLE") {
		t.Error("Set should have 'APPLE' (case-insensitive)")
	}
	if !s.Has("banana") {
		t.Error("Set should have 'banana' (case-insensitive)")
	}
	if s.Has("cherry") {
		t.Error("Set should not have 'cherry'")
	}

	// Test Has on nil set
	var nilSet *ThreadSafeIgnoreCaseSet
	if nilSet.Has("apple") {
		t.Error("Has on nil set should return false")
	}
}

func TestInitialized(t *testing.T) {
	s := NewString()
	if !s.Initialized() {
		t.Error("Newly created set should be initialized")
	}

	var nilSet *ThreadSafeIgnoreCaseSet
	if nilSet.Initialized() {
		t.Error("Nil set should not be initialized")
	}

	emptySet := &ThreadSafeIgnoreCaseSet{}
	if emptySet.Initialized() {
		t.Error("Set without initialized map should not be initialized")
	}
}

func TestUnsortedList(t *testing.T) {
	s := NewString("apple", "banana", "cherry")
	list := s.UnsortedList()

	if len(list) != 3 {
		t.Errorf("List should have 3 items, got %d", len(list))
	}

	// Sort the list for consistent comparison
	sort.Strings(list)
	expected := []string{"apple", "banana", "cherry"}
	for i, item := range expected {
		if list[i] != item {
			t.Errorf("Expected %s at position %d, got %s", item, i, list[i])
		}
	}

	// Test UnsortedList on nil set
	var nilSet *ThreadSafeIgnoreCaseSet
	list = nilSet.UnsortedList()
	if len(list) != 0 {
		t.Error("UnsortedList on nil set should return empty list")
	}
}

func TestLen(t *testing.T) {
	s := NewString("apple", "banana")
	if s.Len() != 2 {
		t.Errorf("Set should have 2 items, got %d", s.Len())
	}

	s.Insert("cherry")
	if s.Len() != 3 {
		t.Errorf("Set should have 3 items after insertion, got %d", s.Len())
	}

	s.Delete("apple")
	if s.Len() != 2 {
		t.Errorf("Set should have 2 items after deletion, got %d", s.Len())
	}

	// Test Len on nil set
	var nilSet *ThreadSafeIgnoreCaseSet
	if nilSet.Len() != 0 {
		t.Error("Len on nil set should return 0")
	}
}

func TestConcurrency(t *testing.T) {
	s := NewString()

	const numGoroutines = 10
	const opsPerGoroutine = 100

	var wg sync.WaitGroup
	var successfulOps int32

	// Launch goroutines that perform concurrent operations
	wg.Add(numGoroutines)
	for i := 0; i < numGoroutines; i++ {
		go func() {
			defer wg.Done()

			for j := 0; j < opsPerGoroutine; j++ {
				key := fmt.Sprintf("item-%d", j%20) // Use limited keys to force contention

				switch j % 4 {
				case 0: // Insert
					s.Insert(key)
				case 1: // Delete
					s.Delete(key)
				case 2: // Check existence
					s.Has(key)
				case 3: // Get length
					s.Len()
				}

				atomic.AddInt32(&successfulOps, 1)
			}
		}()
	}

	wg.Wait()

	// Test passes if no race conditions were detected
	// and all operations were performed
	if int(successfulOps) != numGoroutines*opsPerGoroutine {
		t.Errorf("Expected %d operations, got %d", numGoroutines*opsPerGoroutine, successfulOps)
	}
}
func TestEquals(t *testing.T) {
	// Test identical sets
	s1 := NewString("apple", "banana", "cherry")
	s2 := NewString("APPLE", "BANANA", "CHERRY")
	if !s1.Equals(s2) {
		t.Error("Sets with same elements (different case) should be equal")
	}

	// Test reflexivity
	if !s1.Equals(s1) {
		t.Error("Set should equal itself")
	}

	// Test sets with different elements
	s3 := NewString("apple", "orange")
	if s1.Equals(s3) {
		t.Error("Sets with different elements should not be equal")
	}

	// Test with empty sets
	empty1 := NewString()
	empty2 := NewString()
	if !empty1.Equals(empty2) {
		t.Error("Empty sets should be equal")
	}

	// Test with nil/uninitialized sets
	var nilSet *ThreadSafeIgnoreCaseSet
	if nilSet.Equals(s1) || s1.Equals(nilSet) {
		t.Error("Nil set should not equal initialized set")
	}

	// Test with nil sets comparing to each other
	var nilSet2 *ThreadSafeIgnoreCaseSet
	if nilSet.Equals(nilSet2) {
		t.Error("Nil sets should not be equal (not initialized)")
	}

	// Test with same contents but different order of insertion
	s4 := NewString("cherry", "banana", "apple")
	if !s1.Equals(s4) {
		t.Error("Sets with same elements in different order should be equal")
	}
}

func TestConcurrentReadWrite(t *testing.T) {
	s := NewString("initial")

	const numReaders = 5
	const numWriters = 3
	const iterations = 100

	var wg sync.WaitGroup
	wg.Add(numReaders + numWriters)

	// Start readers
	for i := 0; i < numReaders; i++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				s.Has(fmt.Sprintf("item-%d", j%20))
				s.Len()
				s.UnsortedList()
			}
		}(i)
	}

	// Start writers
	for i := 0; i < numWriters; i++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				key := fmt.Sprintf("item-%d", j%20)
				if j%2 == 0 {
					s.Insert(key)
				} else {
					s.Delete(key)
				}
			}
		}(i)
	}

	wg.Wait()
}

func TestEdgeCases(t *testing.T) {
	// Test with special characters
	s := NewString("hello!", "@world", "#special$", "")

	// Test empty string
	if !s.Has("") {
		t.Error("Set should contain empty string")
	}

	// Test with Unicode characters
	s.Insert("café", "résumé")
	if !s.Has("CAFÉ") {
		t.Error("Set should handle Unicode characters case-insensitively")
	}

	// Test sequential operations
	s = NewString("item1", "item2")
	s.Insert("item3")
	s.Delete("item2")
	s.Insert("ITEM2")

	if s.Len() != 3 {
		t.Errorf("Set should have 3 items after operations, got %d", s.Len())
	}

	if !s.Has("item2") {
		t.Error("Set should have 'item2' after reinsertion with different case")
	}
}
