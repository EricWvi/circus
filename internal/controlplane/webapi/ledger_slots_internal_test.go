package webapi

import "testing"

func TestLedgerSlotsPerUser(t *testing.T) {
	slots := &ledgerSlots{byUser: make(map[string]int), limit: maxLedgerRequests, perUser: maxLedgerRequestsPerUser}
	for i := range maxLedgerRequestsPerUser {
		if !slots.take("a") {
			t.Fatalf("slot %d refused", i)
		}
	}
	if slots.take("a") {
		t.Fatal("one user took more than its share")
	}
	for i := maxLedgerRequestsPerUser; i < maxLedgerRequests; i++ {
		if !slots.take("b") && !slots.take("c") {
			t.Fatalf("slot %d refused to other users", i)
		}
	}
	if slots.take("d") {
		t.Fatal("more requests than slots")
	}
	slots.release("a")
	if slots.take("d") == false || slots.take("a") {
		t.Fatal("a released slot went to the wrong user")
	}
	for _, id := range []string{"a", "b", "c", "d"} {
		for slots.byUser[id] > 0 {
			slots.release(id)
		}
	}
	if slots.total != 0 || len(slots.byUser) != 0 {
		t.Fatalf("released slots: %d in total, %v", slots.total, slots.byUser)
	}
}
