package sqlite

import (
	"strings"
	"testing"
)

// TestJobsCallLookupUsesIndex: ListJobs by device, boot and call id (the
// lookup when a session call settles lost) searches the jobs_call index on
// all three columns instead of scanning every job of the boot.
func TestJobsCallLookupUsesIndex(t *testing.T) {
	d := openAt(t, t.TempDir(), Options{})
	defer d.Close()
	rows, err := d.sqldb.QueryContext(t.Context(), `EXPLAIN QUERY PLAN SELECT `+jobCols+
		` FROM jobs WHERE device_id = ? AND boot_id = ? AND call_id = ? ORDER BY device_id, boot_id, job_id LIMIT ?`, "d", "b", "c", 4)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(plan) == 0 || !strings.Contains(plan[0], "USING INDEX jobs_call (device_id=? AND boot_id=? AND call_id=?)") {
		t.Fatalf("plan %q", plan)
	}
}
