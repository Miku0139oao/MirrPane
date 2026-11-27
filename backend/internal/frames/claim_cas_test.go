package frames

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func claimTestPublisher(t *testing.T) *Publisher {
	t.Helper()
	name := fmt.Sprintf(`Local\MirrPaneClaimCAS_%d_%s`, time.Now().UnixNano(), t.Name())
	p, err := NewPublisher(name, 64, 64)
	if err != nil {
		t.Skipf("shared memory unavailable: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// claimSlot is a compare-and-swap: racers may try the same free slot, but one
// wins and the others move on. With no picture published all three slots are
// available exactly once.
func TestClaimSlotCASIsExclusive(t *testing.T) {
	p := claimTestPublisher(t)
	const racers = 12
	results := make([]int, racers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range racers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i] = p.claimSlot()
		}(i)
	}
	close(start)
	wg.Wait()

	won := map[int]bool{}
	for _, slot := range results {
		if slot < 0 {
			continue
		}
		if slot >= SlotCount {
			t.Fatalf("claimSlot returned out-of-range slot %d", slot)
		}
		if won[slot] {
			t.Fatalf("slot %d was claimed twice", slot)
		}
		won[slot] = true
	}
	if len(won) != SlotCount {
		t.Fatalf("%d distinct slots claimed, want %d", len(won), SlotCount)
	}
	if got := p.claimSlot(); got != -1 {
		t.Fatalf("claimed slot %d although every slot is busy", got)
	}

	for slot := range won {
		atomic.StoreUint32(&p.ctrl.Slots[slot].Claim, claimFree)
	}
	if got := p.claimSlot(); got < 0 {
		t.Fatal("a released slot could not be claimed again")
	}
}

// The picture the reader may be mapping is never handed to the writer: the
// current published slot is skipped even while its claim word is free.
func TestClaimSlotSkipsPublishedSlot(t *testing.T) {
	p := claimTestPublisher(t)
	img := make([]byte, 32*16*4)
	if err := p.Publish(32, 16, 32*4, img); err != nil {
		t.Fatalf("publish: %v", err)
	}
	published := int(p.ctrl.Publish & 3)
	if published >= SlotCount {
		t.Fatalf("publish word points at slot %d", published)
	}
	publishWord := atomic.LoadUint64(&p.ctrl.Publish)

	a, b := p.claimSlot(), p.claimSlot()
	if a < 0 || b < 0 || a == b {
		t.Fatalf("claims = %d, %d, want two different slots", a, b)
	}
	if a == published || b == published {
		t.Fatalf("claimed the published slot %d (claims %d, %d)", published, a, b)
	}
	if c := p.claimSlot(); c != -1 {
		t.Fatalf("claimed slot %d although only the published slot is left", c)
	}
	if got := atomic.LoadUint64(&p.ctrl.Publish); got != publishWord {
		t.Fatalf("claiming changed the publish word: %#x -> %#x", publishWord, got)
	}
	if claim := atomic.LoadUint32(&p.ctrl.Slots[published].Claim); claim != claimFree {
		t.Fatalf("published slot claim = %d, want free", claim)
	}
}
