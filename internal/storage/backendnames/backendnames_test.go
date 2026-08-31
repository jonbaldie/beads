package backendnames

import (
	"fmt"
	"sync"
	"testing"
)

func TestRegistryLifecycleAndConcurrentAccess(t *testing.T) {
	const name = "backendnames-test-lifecycle"
	Remove(name)
	t.Cleanup(func() { Remove(name) })

	if Has(name) {
		t.Fatalf("Has(%q) = true before Add", name)
	}
	Add(name)
	if !Has(name) {
		t.Fatalf("Has(%q) = false after Add", name)
	}
	Remove(name)
	if Has(name) {
		t.Fatalf("Has(%q) = true after Remove", name)
	}

	var workers sync.WaitGroup
	for i := range 32 {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			workerName := fmt.Sprintf("backendnames-test-%d", i)
			Add(workerName)
			if !Has(workerName) {
				t.Errorf("Has(%q) = false after concurrent Add", workerName)
			}
			Remove(workerName)
		}(i)
	}
	workers.Wait()
}
