package hostcpu

import (
	"os"
	"path/filepath"
	"testing"
)

func writeStat(t *testing.T, path string, fields string) {
	t.Helper()
	content := "cpu  " + fields + "\ncpu0 0 0 0 0 0 0 0 0 0 0\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write stat file: %v", err)
	}
}

func TestReadComputesUtilizationBetweenTwoSamples(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stat")
	r := NewReader(path)

	// user=100 nice=0 system=0 idle=900 (total=1000, 90% idle)
	writeStat(t, path, "100 0 0 900 0 0 0 0 0 0")
	if _, ok := r.Read(); ok {
		t.Fatal("first Read should return ok=false: nothing to diff against yet")
	}

	// a further 100 jiffies pass, all idle: total=1100, idle=1000
	writeStat(t, path, "100 0 0 1000 0 0 0 0 0 0")
	pct, ok := r.Read()
	if !ok {
		t.Fatal("second Read should return ok=true")
	}
	if pct != 0 {
		t.Errorf("pct = %v, want 0 (all-idle delta)", pct)
	}

	// another 100 jiffies pass, all busy: total=1200, idle stays 1000
	writeStat(t, path, "200 0 0 1000 0 0 0 0 0 0")
	pct, ok = r.Read()
	if !ok {
		t.Fatal("third Read should return ok=true")
	}
	if pct != 100 {
		t.Errorf("pct = %v, want 100 (all-busy delta)", pct)
	}
}

func TestReadTreatsIowaitAsNotBusy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stat")
	r := NewReader(path)

	writeStat(t, path, "0 0 0 0 0 0 0 0 0 0") // user nice system idle iowait ...
	r.Read()

	// 50 jiffies idle, 50 iowait, 0 truly busy: should read as 0% utilization.
	writeStat(t, path, "0 0 0 50 50 0 0 0 0 0")
	pct, ok := r.Read()
	if !ok {
		t.Fatal("Read should return ok=true")
	}
	if pct != 0 {
		t.Errorf("pct = %v, want 0 (iowait counted as idle, not busy)", pct)
	}
}

func TestReadMissingFileReturnsNotOkAndResetsBaseline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stat")
	r := NewReader(path)

	writeStat(t, path, "100 0 0 900 0 0 0 0 0 0")
	r.Read()

	os.Remove(path)
	if _, ok := r.Read(); ok {
		t.Fatal("Read over a missing file should return ok=false")
	}

	// Once the file is back, the reader shouldn't diff against the stale
	// pre-gap sample (that delta would span an unknown amount of missed time).
	writeStat(t, path, "999 0 0 1 0 0 0 0 0 0")
	if _, ok := r.Read(); ok {
		t.Fatal("first Read after a gap should return ok=false: baseline was reset")
	}
}
