package webapi

import (
	"errors"
	"testing"

	"connectrpc.com/connect"
	"github.com/EricWvi/circus/gen/go/circus/v1/circusv1connect"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
)

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

// A LedgerService request refused for want of a slot is marked as busy
// (ErrorInfo LEDGER_BUSY), so clients retry it, for a session's ledger
// reads (one per user at a time) as for the other reads.
func TestLedgerRequestRefusedAsBusy(t *testing.T) {
	newSlots := func() (*ledgerSlots, *ledgerSlots) {
		return &ledgerSlots{byUser: make(map[string]int), limit: maxLedgerRequests, perUser: maxLedgerRequestsPerUser},
			&ledgerSlots{byUser: make(map[string]int), limit: maxSessionLedgerReads, perUser: maxSessionLedgerReadsPerUser}
	}
	busy := func(t *testing.T, err error) {
		t.Helper()
		var ce *connect.Error
		if !errors.As(err, &ce) || ce.Code() != connect.CodeResourceExhausted {
			t.Fatalf("refusal = %v, want resource_exhausted", err)
		}
		for _, d := range ce.Details() {
			if v, e := d.Value(); e == nil {
				if info, ok := v.(*errdetails.ErrorInfo); ok && info.Reason == ledgerBusy {
					return
				}
			}
		}
		t.Fatalf("refusal %v has no ErrorInfo %s", err, ledgerBusy)
	}

	for _, procedure := range []string{
		circusv1connect.LedgerServiceGetSessionLedgerProcedure,
		circusv1connect.LedgerServiceGetSessionLedgerDiffProcedure,
	} {
		t.Run(procedure, func(t *testing.T) {
			slots, sessionReads := newSlots()
			held := &ledgerAdmission{}
			if err := admitLedgerRequest(slots, sessionReads, circusv1connect.LedgerServiceGetSessionLedgerProcedure, "u", held); err != nil {
				t.Fatal(err)
			}
			if held.user != "u" || !held.sessionRead {
				t.Fatalf("admission = %+v", held)
			}
			refused := &ledgerAdmission{}
			busy(t, admitLedgerRequest(slots, sessionReads, procedure, "u", refused))
			if *refused != (ledgerAdmission{}) {
				t.Fatalf("a refused request holds %+v", refused)
			}
		})
	}

	t.Run("all ledger slots of the user", func(t *testing.T) {
		slots, sessionReads := newSlots()
		for range maxLedgerRequestsPerUser {
			if err := admitLedgerRequest(slots, sessionReads, circusv1connect.LedgerServiceGetLedgerProcedure, "u", &ledgerAdmission{}); err != nil {
				t.Fatal(err)
			}
		}
		refused := &ledgerAdmission{}
		busy(t, admitLedgerRequest(slots, sessionReads, circusv1connect.LedgerServiceGetSessionLedgerProcedure, "u", refused))
		// The session read's own slot went back with the refusal.
		if *refused != (ledgerAdmission{}) || !sessionReads.take("u") {
			t.Fatalf("a refused session read holds %+v", refused)
		}
	})
}
