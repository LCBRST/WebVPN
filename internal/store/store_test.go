package store

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/LCBRST/webvpn/internal/config"
)

// testCost keeps the suite fast. The comparison logic under test does not
// depend on the work factor; bcrypt stores its cost in the hash, so a
// MinCost hash verifies fine against a store configured for production.
const testCost = bcrypt.MinCost

func hash(t *testing.T, password string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(password), testCost)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	return string(h)
}

// newConfig writes a config file into dir and loads it, which is the same path
// production takes. statePath names the state file inside the same directory.
func newConfig(t *testing.T, stateSection string) *config.Config {
	t.Helper()
	dir := t.TempDir()
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))

	doc := `
server:
  listen: "127.0.0.1:0"
  base_domain: "example.com"
  portal_host: "vpn.example.com"
  public_scheme: "https"
  allow_loopback_targets: true
session:
  cookie_name: "webvpn_session"
  signing_key: "` + key + `"
users:
  - name: "alice"
    password_hash: "` + hash(t, "alice-password") + `"
  - name: "bob"
    password_hash: "` + hash(t, "bob-password") + `"
login:
  attempts_per_minute: 100
  max_concurrent: 1
` + stateSection + `
services:
  - name: "Wiki"
    subdomain: "wiki-vpn"
    target: "http://192.168.1.10:5000"
  - name: "Files"
    subdomain: "files-vpn"
    target: "http://192.168.1.11:9000"
`
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return cfg
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// seedConfig is the common case: a state file that does not exist yet and a
// path pointing into the same temp directory.
func seedConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := newConfig(t, "")
	// Load defaults the path to one next to the config file, which is inside
	// the temp dir already. Assert that rather than assuming it.
	if !strings.HasSuffix(cfg.State.Path, "state.json") {
		t.Fatalf("state path defaulted to %q, want .../state.json", cfg.State.Path)
	}
	return cfg
}

func TestSeedGrantsEveryoneEverything(t *testing.T) {
	// The upgrade must not take anything away from anyone. Before this feature
	// existed, every login reached every service.
	cfg := seedConfig(t)
	s, err := New(cfg, quietLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for _, user := range []string{"alice", "bob"} {
		for _, sub := range []string{"wiki-vpn", "files-vpn"} {
			if !s.Allows(user, sub) {
				t.Errorf("%s was not granted %s by the seed", user, sub)
			}
		}
	}
}

func TestSeedPromotesAnAdmin(t *testing.T) {
	// A config written before the admin page existed carries no admin flag, and
	// a state with no admin cannot be edited back into having one.
	cfg := seedConfig(t)
	s, err := New(cfg, quietLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if !s.IsAdmin("alice") {
		t.Error("the first user was not promoted, so nothing can own the admin page")
	}
	if s.IsAdmin("bob") {
		t.Error("every user was promoted to admin; only the first should be")
	}
}

func TestSeedHonoursAnExplicitAdmin(t *testing.T) {
	cfg := seedConfig(t)
	// bob is marked admin in the config, so nobody should be promoted over him.
	cfg.Users[1].Admin = true

	s, err := New(cfg, quietLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !s.IsAdmin("bob") {
		t.Error("the configured admin lost the flag")
	}
	if s.IsAdmin("alice") {
		t.Error("a user was promoted even though the config named an admin")
	}
}

func TestSeededStateIsWrittenAndReloaded(t *testing.T) {
	// The second start must read the file, not re-seed from the config. Getting
	// this wrong resurrects every user who has been deleted.
	cfg := seedConfig(t)
	first, err := New(cfg, quietLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	snap := first.Snapshot()
	snap.Users = snap.Users[:1] // drop bob
	if err := first.Replace(snap); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	second, err := New(cfg, quietLogger())
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if second.Authenticate("bob", "bob-password") {
		t.Error("a deleted user came back, so the config seed was used instead of the state file")
	}
	if !second.Authenticate("alice", "alice-password") {
		t.Error("the surviving user cannot log in after a reload")
	}

	if _, err := os.Stat(cfg.State.Path); err != nil {
		t.Errorf("state file was not written: %v", err)
	}
}

func TestStateFileIsNotWorldReadable(t *testing.T) {
	// It holds password hashes.
	//
	// Only meaningful where the permission bits are: Windows reports 0666 for
	// everything and enforces access through ACLs instead, so this asserts
	// nothing there and would only be a false failure on a dev machine.
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not the access control on Windows")
	}

	cfg := seedConfig(t)
	if _, err := New(cfg, quietLogger()); err != nil {
		t.Fatalf("New: %v", err)
	}

	info, err := os.Stat(cfg.State.Path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}
}

func TestRejectedReplaceChangesNothing(t *testing.T) {
	// A save that fails validation must leave both the file and the running
	// server exactly as they were.
	cfg := seedConfig(t)
	s, err := New(cfg, quietLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	before, err := os.ReadFile(cfg.State.Path)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}

	bad := s.Snapshot()
	// A duplicate name, which config.ValidateUsers rejects.
	bad.Users = append(bad.Users, bad.Users[0])

	if err := s.Replace(bad); err == nil {
		t.Fatal("Replace accepted a state with a duplicate user name")
	}

	after, err := os.ReadFile(cfg.State.Path)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if string(before) != string(after) {
		t.Error("the state file changed even though the replacement was rejected")
	}
	if len(s.Snapshot().Users) != 2 {
		t.Error("the in-memory state changed even though the replacement was rejected")
	}
}

func TestReplaceRefusesToRemoveTheLastAdmin(t *testing.T) {
	// The one rule that has no way back: with no admin, the admin page cannot
	// be used to appoint one.
	cfg := seedConfig(t)
	s, err := New(cfg, quietLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	next := s.Snapshot()
	for i := range next.Users {
		next.Users[i].Admin = false
	}

	if err := s.Replace(next); err == nil {
		t.Fatal("Replace accepted a state with no admin in it")
	}
	if !s.IsAdmin("alice") {
		t.Error("the admin flag was dropped despite the replacement being rejected")
	}
}

func TestReplaceRefusesANonBcryptHash(t *testing.T) {
	cfg := seedConfig(t)
	s, err := New(cfg, quietLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	next := s.Snapshot()
	next.Users[0].PasswordHash = "hunter2"

	if err := s.Replace(next); err == nil {
		t.Fatal("Replace accepted a plaintext password in the hash field")
	}
}

func TestReplaceRefusesAnOpenRelayTarget(t *testing.T) {
	// The service rules are the config's, run through the same function, so a
	// target on a public address is refused here exactly as it would be by
	// -check.
	cfg := seedConfig(t)
	s, err := New(cfg, quietLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	next := s.Snapshot()
	next.Services[0].Target = "http://8.8.8.8/"

	if err := s.Replace(next); err == nil {
		t.Fatal("Replace accepted a target on a public address")
	}
}

func TestCorruptStateFileIsFatal(t *testing.T) {
	// Falling back to the config seed here would silently undo every edit.
	cfg := seedConfig(t)
	if err := os.WriteFile(cfg.State.Path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := New(cfg, quietLogger()); err == nil {
		t.Fatal("New accepted a corrupt state file")
	}
}

func TestUnknownStateVersionIsFatal(t *testing.T) {
	cfg := seedConfig(t)
	body, err := json.Marshal(State{Version: 99})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(cfg.State.Path, body, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := New(cfg, quietLogger()); err == nil {
		t.Fatal("New accepted a state file from a future version")
	}
}

func TestAuthenticate(t *testing.T) {
	cfg := seedConfig(t)
	s, err := New(cfg, quietLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if !s.Authenticate("alice", "alice-password") {
		t.Error("the right password was rejected")
	}
	if s.Authenticate("alice", "wrong horse") {
		t.Error("the wrong password was accepted")
	}
	if s.Authenticate("nobody", "alice-password") {
		t.Error("an unknown user was accepted")
	}
}

func TestAllows(t *testing.T) {
	cfg := seedConfig(t)
	s, err := New(cfg, quietLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Narrow bob down to one service.
	next := s.Snapshot()
	bob := next.IndexOfUser("bob")
	next.Users[bob].Services = []string{"wiki-vpn"}
	if err := s.Replace(next); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	if !s.Allows("bob", "wiki-vpn") {
		t.Error("the granted service was refused")
	}
	if s.Allows("bob", "files-vpn") {
		t.Error("a service that was not granted was allowed")
	}
	// alice is the admin, so she keeps everything without being listed.
	if !s.Allows("alice", "files-vpn") {
		t.Error("the admin was refused a service")
	}
	// A name that is not in the state at all: this is what a token outliving a
	// deleted account looks like.
	if s.Allows("carol", "wiki-vpn") {
		t.Error("an unknown user was allowed something")
	}
}

func TestGrantForADeletedServiceIsInert(t *testing.T) {
	// A stale entry left in the file grants nothing on its own, so a service
	// deleted and later re-added under the same name does not silently
	// re-grant access while the file is in that state.
	cfg := seedConfig(t)
	s, err := New(cfg, quietLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	next := s.Snapshot()
	// Drop the service but leave the grant behind.
	next.Services = next.Services[1:]
	if err := s.Replace(next); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	if s.Allows("bob", "wiki-vpn") {
		t.Error("a grant naming a service that no longer exists still allowed it")
	}
}

func TestSnapshotTargetsMatchTheServices(t *testing.T) {
	cfg := seedConfig(t)
	s, err := New(cfg, quietLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	snap := s.Snapshot()
	targets := snap.Targets()
	for _, svc := range snap.Services {
		u, ok := targets[svc.Subdomain]
		if !ok {
			t.Fatalf("%s has no parsed target", svc.Subdomain)
		}
		if u.Port() == "" {
			t.Errorf("%s: target %v lost its port", svc.Subdomain, u)
		}
	}
}
