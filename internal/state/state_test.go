package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// withTempHome reroutes the state path to a temp directory. Returns
// the resolved state file path for assertions.
func withTempHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	t.Setenv(PathEnv, p)
	return p
}

func TestLoad_MissingFileReturnsEmptyStateNoError(t *testing.T) {
	withTempHome(t)
	s, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.Version != CurrentVersion {
		t.Errorf("version = %d, want %d", s.Version, CurrentVersion)
	}
	if len(s.Services) != 0 || len(s.Accessories) != 0 {
		t.Errorf("expected empty state, got %+v", s)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	p := withTempHome(t)
	in := Empty()
	in.SetAccessoryHostPort("postgres", 5433)
	in.RecordService("myapp/api", map[string]map[string]string{
		"postgres": {"dbname": "myapp"},
		"proxy":    {"domain": "app.example.com"},
	})
	if err := Save(in); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("expected file at %s: %v", p, err)
	}

	out, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, set := out.AccessoryHostPort("postgres")
	if !set || got != 5433 {
		t.Errorf("AccessoryHostPort = %d set=%v, want 5433 true", got, set)
	}
	usage := out.ServicesUsingAccessory("postgres")
	if len(usage) != 1 || usage[0].Key != "myapp/api" || usage[0].Config["dbname"] != "myapp" {
		t.Errorf("unexpected usage: %+v", usage)
	}
}

func TestRecordService_RefreshesEntry(t *testing.T) {
	withTempHome(t)
	s := Empty()
	s.RecordService("svc", map[string]map[string]string{"proxy": {"domain": "old"}})
	first := s.Services["svc"].LastSeenAt
	time.Sleep(2 * time.Millisecond)
	s.RecordService("svc", map[string]map[string]string{"proxy": {"domain": "new"}})
	if s.Services["svc"].Accessories["proxy"]["domain"] != "new" {
		t.Errorf("expected refreshed config, got %+v", s.Services["svc"].Accessories)
	}
	if !s.Services["svc"].LastSeenAt.After(first) {
		t.Errorf("LastSeenAt did not advance: %v vs %v", first, s.Services["svc"].LastSeenAt)
	}
}

func TestRecordService_DropsAccessoriesWhenAllRemoved(t *testing.T) {
	s := Empty()
	s.RecordService("svc", map[string]map[string]string{"proxy": {"domain": "x"}})
	s.RecordService("svc", map[string]map[string]string{})
	if s.Services["svc"].Accessories != nil {
		t.Errorf("expected nil Accessories after empty record, got %+v", s.Services["svc"].Accessories)
	}
}

func TestSave_WritesAtomicallyAndDoesNotLeaveTemp(t *testing.T) {
	p := withTempHome(t)
	s := Empty()
	s.SetAccessoryHostPort("x", 1)
	if err := Save(s); err != nil {
		t.Fatalf("Save: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(p))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if e.Name() == filepath.Base(p) {
			continue
		}
		t.Errorf("unexpected leftover file: %s", e.Name())
	}
}

func TestServicesUsingAccessory_FiltersAndSorts(t *testing.T) {
	s := Empty()
	s.RecordService("b/svc", map[string]map[string]string{"postgres": {"dbname": "b"}})
	s.RecordService("a/svc", map[string]map[string]string{"postgres": {"dbname": "a"}})
	s.RecordService("c/svc", map[string]map[string]string{"proxy": {"domain": "c"}})

	got := s.ServicesUsingAccessory("postgres")
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Key != "a/svc" || got[1].Key != "b/svc" {
		t.Errorf("not sorted: %+v", got)
	}
}
