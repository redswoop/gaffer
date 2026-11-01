package controller

import (
	"path/filepath"
	"testing"
)

// A db from before pauses had an author: its paused rows become a human's (nothing auto-resumes them).
func TestOpenDBMigratesPausedBy(t *testing.T) {
	p := filepath.Join(t.TempDir(), "gaffer.db")
	db, err := openDB(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO items(id, kind, title, state, created, updated, spec) VALUES(1,'fake','a','paused',1,1,'{}'),
		(2,'fake','b','queued',1,1,'{}')`); err != nil {
		t.Fatal(err)
	}
	db.Exec("UPDATE items SET paused_by='' WHERE id=1")
	db.Close()
	db, err = openDB(p)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var by string
	db.QueryRow("SELECT paused_by FROM items WHERE id=1").Scan(&by)
	if by != "human" {
		t.Errorf("paused row: paused_by=%q, want human", by)
	}
	db.QueryRow("SELECT paused_by FROM items WHERE id=2").Scan(&by)
	if by != "" {
		t.Errorf("queued row: paused_by=%q, want empty", by)
	}
	// every column in `cols` scans and writes back (the positional lists stay in step)
	c := &Controller{db: db}
	it := c.get(1)
	if it == nil {
		t.Fatal("get(1) failed: cols vs scanRec out of step?")
	}
	it.YieldTo, it.PausedBoot, it.RunMS = "#9", "boot", 42
	c.put(it)
	if again := c.get(1); again.YieldTo != "#9" || again.PausedBoot != "boot" || again.RunMS != 42 {
		t.Errorf("round trip lost fields: %+v", again.Item)
	}
}
